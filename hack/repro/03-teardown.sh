#!/usr/bin/env bash
# Delete the kind cluster. Pass --tools to also remove the binaries that
# 00-install-tools.sh placed in ~/.local/bin, and --images to prune docker images.
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

say "deleting kind cluster '$CLUSTER'"
kind delete cluster --name "$CLUSTER" || true

if [ "${1:-}" = "--tools" ] || [ "${2:-}" = "--tools" ]; then
  say "removing installed tool binaries"
  rm -f "$LOCAL_BIN/kind" "$LOCAL_BIN/kubectl" "$LOCAL_BIN/yq"
fi
if [ "${1:-}" = "--images" ] || [ "${2:-}" = "--images" ]; then
  say "pruning dangling docker images"
  docker image prune -f || true
fi
ok "torn down"
