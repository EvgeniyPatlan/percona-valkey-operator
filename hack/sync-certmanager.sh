#!/usr/bin/env bash
# sync-certmanager.sh — sync CERT_MANAGER_VER into the kuttl e2e vars file
# (docs/architecture/10 §8.1 step 3; impl 08 OPS-7.3). PS uses e2e-tests/vars.sh.
#
# Source of truth order:
#   1. the cert-manager module version in go.mod (github.com/cert-manager/cert-manager), if present
#   2. else CERT_MANAGER_VER from e2e-tests/release_versions
# The resolved version is written as CERT_MANAGER_VER=<ver> into the target vars file
# (replacing any existing line, or appending if absent).
#
# Usage: hack/sync-certmanager.sh go.mod e2e-tests/vars.sh
set -euo pipefail

GOMOD="${1:?usage: sync-certmanager.sh <go.mod> <vars.sh>}"
VARS="${2:?usage: sync-certmanager.sh <go.mod> <vars.sh>}"

ver=""
if [ -f "$GOMOD" ]; then
  # e.g. "	github.com/cert-manager/cert-manager v1.16.2" -> 1.16.2
  ver="$(grep -E 'cert-manager/cert-manager[[:space:]]+v[0-9]' "$GOMOD" 2>/dev/null \
        | head -1 | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | tr -d 'v' || true)"
fi

if [ -z "$ver" ] && [ -f e2e-tests/release_versions ]; then
  # shellcheck disable=SC1091
  ver="$(grep -E '^CERT_MANAGER_VER=' e2e-tests/release_versions | head -1 | cut -d= -f2 | tr -d '[:space:]' || true)"
fi

if [ -z "$ver" ]; then
  echo "sync-certmanager: could not resolve a cert-manager version from $GOMOD or release_versions" >&2
  exit 1
fi

touch "$VARS"
if grep -qE '^CERT_MANAGER_VER=' "$VARS"; then
  tmp="$(mktemp)"
  sed "s|^CERT_MANAGER_VER=.*|CERT_MANAGER_VER=${ver}|" "$VARS" > "$tmp"
  mv "$tmp" "$VARS"
else
  printf 'CERT_MANAGER_VER=%s\n' "$ver" >> "$VARS"
fi

echo "sync-certmanager: CERT_MANAGER_VER=${ver} -> $VARS"
