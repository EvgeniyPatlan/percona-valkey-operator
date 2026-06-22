#!/usr/bin/env bash
# OPTIONAL / advanced: build the operator image from the fix branch, load it into kind,
# and redeploy — so the REAL operator (not a manual patch) is exercised end-to-end.
# Requires Go (1.26+) and Docker; takes a few minutes.
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

IMG="${IMG:-local/percona-valkey-operator:repro}"
BRANCH="${BRANCH:-}"

command -v go >/dev/null 2>&1 || { echo "Go not found (needed to build the operator). Install Go 1.26+ first."; exit 1; }
[ -d "$OPERATOR_REPO" ] || { echo "operator repo not found at $OPERATOR_REPO (set OPERATOR_REPO)"; exit 1; }

cd "$OPERATOR_REPO"
if [ -n "$BRANCH" ]; then
  say "checking out fix branch '$BRANCH'"
  git rev-parse --verify "$BRANCH" >/dev/null 2>&1 && git checkout "$BRANCH" \
    || warn "branch $BRANCH not found; building from current HEAD ($(git rev-parse --abbrev-ref HEAD))"
fi

say "building operator image $IMG  (runs 'make build'; needs Go + Docker)"
make build IMG="$IMG"

say "loading image into kind cluster '$CLUSTER'"
kind load docker-image "$IMG" --name "$CLUSTER"

say "pointing the operator deployment at the patched image"
k set image "deploy/$OPERATOR_DEPLOY" "*=$IMG"
k patch deploy "$OPERATOR_DEPLOY" --type=json \
  -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' || true
k rollout status "deploy/$OPERATOR_DEPLOY" --timeout=180s

ok "patched operator active — re-exercise the scenario (e.g. repeated current-primary kills)."
echo "NOTE: after the first failover the live primary is no longer 0-0 — resolve the CURRENT"
echo "primary each iteration by parsing CLUSTER NODES (role is read from the engine, not the"
echo "pod name). See docs/architecture/11-testing-qa.md §8 Level C for the loop."
