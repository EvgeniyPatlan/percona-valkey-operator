#!/usr/bin/env bash
# Shared config + helpers for a percona-valkey-operator Kind reproduction harness.
# Source this from the numbered scripts:  . "$(dirname "$0")/lib.sh"
#
# TEMPLATE (M8 foundation, arch §7.2). Copy this directory to `repro-K8SVK-<n>/` per
# reported issue and adapt cr-cluster.yaml + 02-reproduce.sh to the specific bug. The
# config/helper CONTRACT here mirrors repro-K8SPS-732 exactly, adapted to Valkey
# names/labels. (docs/architecture/11-testing-qa.md §7)
set -euo pipefail

# ---- config (override via env) ----
export CLUSTER="${CLUSTER:-vk-repro}"
export KCTX="${KCTX:-kind-$CLUSTER}"
# NOTE: do NOT name the namespace after a real ccTLD — the per-pod short FQDN would end
# in that ccTLD and CoreDNS forwards it upstream, returning bogus answers. Use a clearly
# non-TLD suffix (valkey-repro) and always resolve full …svc.cluster.local names in probes.
export NS="${NS:-valkey-repro}"
export CR_NAME="${CR_NAME:-cluster1}"
export OPERATOR_BUNDLE_URL="${OPERATOR_BUNDLE_URL:-}"   # empty => repo deploy/bundle.yaml
export OPERATOR_DEPLOY="${OPERATOR_DEPLOY:-valkey-operator}"
export CERT_MANAGER_VER="${CERT_MANAGER_VER:-v1.16.2}"
export LOCAL_BIN="${LOCAL_BIN:-$HOME/.local/bin}"

# Locate the operator repo (for deploy/bundle.yaml). This template lives at
# <repo>/hack/repro/; a copied repro-K8SVK-<n>/ lives beside the repo, so allow both.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$HERE/../../deploy/bundle.yaml" ]; then
  export OPERATOR_REPO="${OPERATOR_REPO:-$(cd "$HERE/../.." && pwd)}"
else
  export OPERATOR_REPO="${OPERATOR_REPO:-$(cd "$HERE/.." && pwd)/percona-valkey-operator}"
fi
export CR_FILE="${CR_FILE:-$HERE/cr-cluster.yaml}"
export PATH="$LOCAL_BIN:$PATH"

# ---- kubectl wrappers ----
k()  { kubectl --context "$KCTX" -n "$NS" "$@"; }
kk() { kubectl --context "$KCTX" "$@"; }   # cluster-scoped
say(){ printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
ok() { printf '\033[1;32m%s\033[0m\n' "$*"; }
warn(){ printf '\033[1;33m%s\033[0m\n' "$*"; }

# ---- readiness helpers (Valkey charter labels) ----
# Ready Valkey server pods, selected by the labels the operator ACTUALLY stamps
# (verified against pkg/naming): the per-cluster topology label
# valkey.percona.com/cluster=<cluster> plus app.kubernetes.io/component=valkey.
#
# NB: do NOT select on app.kubernetes.io/name=valkey or
# app.kubernetes.io/instance=<cluster> here. The operator stamps
# app.kubernetes.io/name=percona-valkey (naming.AppNameValue), and on the server
# pods naming.NodeLabels() overrides app.kubernetes.io/instance to the *node*
# name (e.g. cluster1-0-0), not the cluster name — so the obvious-looking
# selector matches zero pods and the readiness wait hangs forever.
VK_POD_SELECTOR="${VK_POD_SELECTOR:-valkey.percona.com/cluster=$CR_NAME,app.kubernetes.io/component=valkey}"
valkey_ready_count() {
  k get pods -l "$VK_POD_SELECTOR" \
    --no-headers 2>/dev/null | grep -c 'Running' || true
}

wait_valkey_ready() {  # $1 expected count, $2 timeout seconds (default 600)
  local want="${1:-6}" deadline=$(( SECONDS + ${2:-600} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(valkey_ready_count)" = "$want" ] && return 0
    sleep 5
  done
  return 1
}

# Cluster CR state (short name: pvk = PerconaValkeyCluster).
cluster_state() { k get pvk "$CR_NAME" -o jsonpath='{.status.state}' 2>/dev/null; }

wait_for_state() {  # $1 desired state (e.g. Ready), $2 timeout seconds (default 300)
  local want="${1:-Ready}" deadline=$(( SECONDS + ${2:-300} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(cluster_state)" = "$want" ] && return 0
    sleep 5
  done
  return 1
}

# ---- helpers used by worked reproductions ----
# Sum of .status.observedGeneration across all StatefulSets of the cluster. A
# config-hash *roll* bumps a StatefulSet's generation (pod-template annotation
# change); a no-op reconcile must not. Shape-(b) repros diff this before/after.
sts_generation_sum() {
  k get statefulset -l "valkey.percona.com/cluster=$CR_NAME" \
    -o jsonpath='{range .items[*]}{.metadata.generation}{"\n"}{end}' 2>/dev/null \
    | awk '{s+=$1} END{print s+0}'
}

# Resolve the pod name of the CURRENT primary of a given shard (default 0) by
# parsing CLUSTER NODES from a live pod — the role is read from the engine, never
# inferred from the pod name (after a failover the primary is no longer -<shard>-0).
# Requires the operator ACL password in OPERATOR_PASS (see 04 §Level C in QA.md).
current_primary_pod() {  # $1 shard (default 0)
  local shard="${1:-0}" anypod ip
  anypod="$(k get pod -l "valkey.percona.com/cluster=$CR_NAME,valkey.percona.com/shard-index=$shard,app.kubernetes.io/component=valkey" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$anypod" ] || { return 1; }
  ip="$(k exec "$anypod" -c valkey -- valkey-cli -a "${OPERATOR_PASS:-}" --no-auth-warning \
    cluster nodes 2>/dev/null | awk '/master/ && /myself|connected/ {split($2,a,":"); print a[1]}' | head -1)"
  [ -n "$ip" ] || return 1
  k get pod -o jsonpath="{.items[?(@.status.podIP=='$ip')].metadata.name}" 2>/dev/null
}
