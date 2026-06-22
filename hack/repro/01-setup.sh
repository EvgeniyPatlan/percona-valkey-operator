#!/usr/bin/env bash
# Create the kind cluster, install cert-manager + the (unpatched, perconalab:main)
# operator from the repo bundle, apply the cluster CR, and wait for state=Ready.
# Idempotent: safe to re-run.
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

# 1) kind cluster
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  say "creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER" --wait 120s
else
  ok "kind cluster '$CLUSTER' already exists"
fi

# 2) cert-manager (the operator needs it for TLS)
if ! kk get ns cert-manager >/dev/null 2>&1; then
  say "installing cert-manager $CERT_MANAGER_VER"
  kk apply -f "https://github.com/cert-manager/cert-manager/releases/download/$CERT_MANAGER_VER/cert-manager.yaml"
fi
say "waiting for cert-manager"
kk -n cert-manager wait --for=condition=Available deploy --all --timeout=180s

# 3) operator (unpatched bundle from the repo)
kk create namespace "$NS" --dry-run=client -o yaml | kk apply -f - >/dev/null
if [ -n "$OPERATOR_BUNDLE_URL" ]; then
  BUNDLE="$OPERATOR_BUNDLE_URL"
else
  BUNDLE="$OPERATOR_REPO/deploy/bundle.yaml"
  [ -f "$BUNDLE" ] || { echo "bundle not found: $BUNDLE (set OPERATOR_REPO or OPERATOR_BUNDLE_URL)"; exit 1; }
fi
say "deploying operator from $BUNDLE into namespace '$NS'"
k apply --server-side -f "$BUNDLE"
k rollout status "deploy/$OPERATOR_DEPLOY" --timeout=180s

# 4) cluster CR
say "applying cluster CR ($CR_FILE)"
k apply -f "$CR_FILE"

say "waiting for cluster '$CR_NAME' to reach state=Ready (pulls Valkey + exporter images; minutes)"
deadline=$(( SECONDS + ${READY_TIMEOUT:-1200} ))
while [ "$SECONDS" -lt "$deadline" ]; do
  st="$(cluster_state)"; rc="$(valkey_ready_count)"
  printf '  state=%s valkey_ready=%s\n' "${st:-<none>}" "$rc"
  [ "$st" = "Ready" ] && break
  sleep 15
done
echo
k get pvk,pods
[ "$(cluster_state)" = "Ready" ] && ok "cluster ready — now run ./02-reproduce.sh" \
  || warn "cluster not ready yet; re-run this script or inspect: kubectl --context $KCTX -n $NS get pods"
