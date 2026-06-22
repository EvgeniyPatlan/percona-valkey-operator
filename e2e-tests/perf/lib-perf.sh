#!/usr/bin/env bash
# e2e-tests/perf/lib-perf.sh — shared config + helpers for the perf/scale harness
# (sourced by run-perf.sh). Models the e2e-tests/functions vocabulary but for the perf
# layer (docs/architecture/11-testing-qa.md §5). NOT run in the authoring env — no live
# cluster; bash -n only.
set -o errexit
set -o nounset
set -o pipefail

# --- identity / config (override via env or a profile) ---
: "${OPERATOR_IMAGE:=${IMAGE:-perconalab/valkey-operator:main}}"
: "${CLUSTER_NAME:=perf1}"
: "${VALKEY_VERSION:=9.0}"
: "${NAMESPACE:=valkey-perf}"
: "${BENCH_IMAGE:=perconalab/percona-valkey:${VALKEY_VERSION}}"
: "${SVC_PORT:=6379}"

# Regression-gate thresholds (percent). See README "Target thresholds".
: "${PERF_P99_REGRESS_PCT:=20}"
: "${PERF_OPS_REGRESS_PCT:=15}"
: "${PERF_EXPORTER_OVERHEAD_PCT:=5}"

# Artifact output (object storage on CI).
: "${PERF_ARTIFACT_DIR:=./artifacts/perf}"

PERF_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export E2E_ROOT="${E2E_ROOT:-$(cd "$PERF_ROOT/.." && pwd)}"

# --- kubectl wrappers ---
kube()  { kubectl -n "$NAMESPACE" "$@"; }
kubec() { kubectl "$@"; }

say()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m%s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m%s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m%s\033[0m\n' "$*" >&2; exit 1; }

# Headless Service FQDN of the cluster (resolved by the benchmark container).
svc_host() { echo "valkey-${CLUSTER_NAME}.${NAMESPACE}.svc.cluster.local"; }

cluster_state() { kube get pvk "$CLUSTER_NAME" -o jsonpath='{.status.state}' 2>/dev/null || true; }

wait_cluster_ready() {  # $1 timeout seconds
  local timeout="${1:-600}" deadline
  deadline=$(( SECONDS + timeout ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(cluster_state)" = "Ready" ] && return 0
    sleep 5
  done
  die "cluster '$CLUSTER_NAME' did not reach Ready in ${timeout}s"
}

# Exec valkey-cli inside a Ready pod (used for eviction / scale probes).
exec_valkey() {
  local pod
  pod="$(kube get pod \
    -l app.kubernetes.io/instance="$CLUSTER_NAME",app.kubernetes.io/name=valkey \
    -o jsonpath='{.items[0].metadata.name}')"
  kube exec "$pod" -c valkey -- valkey-cli "$@"
}

# Block until cluster_state:ok with full slot coverage (used after scale-out).
wait_cluster_ok() {  # $1 timeout seconds
  local timeout="${1:-600}" deadline
  deadline=$(( SECONDS + timeout ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if exec_valkey 'CLUSTER' 'INFO' 2>/dev/null | grep -q 'cluster_state:ok' \
       && exec_valkey 'CLUSTER' 'INFO' 2>/dev/null | grep -q 'cluster_slots_assigned:16384'; then
      return 0
    fi
    sleep 5
  done
  die "cluster did not reach cluster_state:ok with full slots in ${timeout}s"
}

# Count reconciles attributable to rebalance moves, if the operator exposes the metric
# (valkey_operator_rebalance_moves_total). Falls back to "n/a" when unscrapable here.
rebalance_moves() {
  kube get --raw "/apis/metrics" >/dev/null 2>&1 || true
  echo "n/a"   # placeholder: real scrape wires Prometheus/PodMonitor (arch §8)
}

# Parse one valkey-benchmark --csv line: "<test>","<rps>","<avg>","<min>","<p50>",
# "<p95>","<p99>","<max>" (field set varies by version; we read by header-free
# position defensively and fall back to rps-only when the latency cols are absent).
# Echoes: <test> <rps> <p99>
parse_bench_csv_line() {
  local line="$1"
  # strip quotes, split on comma
  local clean; clean="$(printf '%s' "$line" | tr -d '"')"
  local test rps p99
  test="$(printf '%s' "$clean" | cut -d, -f1)"
  rps="$(printf '%s' "$clean" | cut -d, -f2)"
  # p99 column index differs across releases; take the 7th field if present.
  p99="$(printf '%s' "$clean" | cut -d, -f7)"
  [ -n "$p99" ] || p99="n/a"
  echo "$test $rps $p99"
}

# Percentage regression of $2 vs baseline $1 (positive = worse for latency). Integer math.
pct_delta() {  # $1 baseline  $2 current
  local base="$1" cur="$2"
  case "$base$cur" in *[!0-9.]*) echo "n/a"; return;; esac
  awk -v b="$base" -v c="$cur" 'BEGIN{ if (b==0){print "n/a"; exit} printf "%.1f", (c-b)/b*100 }'
}
