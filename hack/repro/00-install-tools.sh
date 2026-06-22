#!/usr/bin/env bash
# Install kind, kubectl, yq into ~/.local/bin (no sudo). Idempotent.
# The optional patched-image build (04-*) additionally needs Go (1.26+) and Docker.
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

mkdir -p "$LOCAL_BIN"
KIND_VER="${KIND_VER:-v0.27.0}"
YQ_VER="${YQ_VER:-v4.44.3}"

dl() { # url dest
  for i in 1 2 3; do curl -fsSL -o "$2" "$1" && return 0; warn "retry $i: $1"; sleep 3; done
  echo "failed to download $1" >&2; return 1
}

if ! command -v kind >/dev/null 2>&1; then
  say "installing kind $KIND_VER"
  dl "https://kind.sigs.k8s.io/dl/$KIND_VER/kind-linux-amd64" "$LOCAL_BIN/kind"
  chmod +x "$LOCAL_BIN/kind"
fi
if ! command -v kubectl >/dev/null 2>&1; then
  say "installing kubectl (stable)"
  KVER="$(curl -fsSL https://dl.k8s.io/release/stable.txt)"
  dl "https://dl.k8s.io/release/$KVER/bin/linux/amd64/kubectl" "$LOCAL_BIN/kubectl"
  chmod +x "$LOCAL_BIN/kubectl"
fi
if ! command -v yq >/dev/null 2>&1; then
  say "installing yq $YQ_VER"
  dl "https://github.com/mikefarah/yq/releases/download/$YQ_VER/yq_linux_amd64" "$LOCAL_BIN/yq"
  chmod +x "$LOCAL_BIN/yq"
fi

say "versions"
kind version; kubectl version --client | head -1; yq --version
ok "tools ready (make sure $LOCAL_BIN is on your PATH)"
echo "Note: building a patched operator image (optional, 04-*) additionally needs Go 1.26+ and Docker."
echo "Resource note: a single-node Kind needs ~4 CPU / 6 GB RAM / 6 GB disk to form a 3-shard cluster."
