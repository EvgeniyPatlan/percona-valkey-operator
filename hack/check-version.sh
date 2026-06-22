#!/usr/bin/env bash
# check-version.sh — drift guard: pkg/version/version.txt major.minor MUST equal
# deploy/cr.yaml spec.crVersion (docs/architecture/10 §6 rec, §6.1 trap 2; impl 08 §8.1).
#
# Reads the embedded SoT file DIRECTLY (no Go binary, no cmd/printversion) — version.txt
# is the literal operator-version string, so `cut -d. -f1-2` yields major.minor.
#
# NOTE (impl 08, OPS-6.3 / OPS-7.10): the canonical home of this gate is M6 OPS-6.3; M7
# only relocates it into its own check-version.yml workflow. This script is the shared
# implementation both the `make check-version` target and that workflow invoke.
#
# Usage: hack/check-version.sh [version.txt] [deploy/cr.yaml]
set -euo pipefail

VERSION_TXT="${1:-pkg/version/version.txt}"
CR="${2:-deploy/cr.yaml}"

[ -f "$VERSION_TXT" ] || { echo "check-version: $VERSION_TXT not found" >&2; exit 1; }
[ -f "$CR" ]          || { echo "check-version: $CR not found" >&2; exit 1; }

v="$(tr -d '[:space:]' < "$VERSION_TXT" | cut -d. -f1-2)"
cr="$(yq '.spec.crVersion' "$CR" | tr -d '"[:space:]')"

if [ "$v" != "$cr" ]; then
  echo "check-version: MISMATCH — version.txt major.minor='$v' != $CR spec.crVersion='$cr'" >&2
  echo "  -> run 'make release VERSION=x.y.z' (do NOT hand-edit crVersion)" >&2
  exit 1
fi

echo "check-version: OK — version.txt major.minor='$v' == crVersion='$cr'"
