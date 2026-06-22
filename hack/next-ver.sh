#!/usr/bin/env bash
# next-ver.sh — derive NEXT_VER for `make after-release` (docs/architecture/10 §8.2).
#
# Reads spec.crVersion (major.minor) from deploy/cr.yaml and prints major.(minor+1).0.
# It does NOT read pkg/version/version.txt — after-release deliberately derives the next
# dev version from the CR API contract, not the operator semver.
#
# Usage: hack/next-ver.sh deploy/cr.yaml
set -euo pipefail

CR="${1:?usage: next-ver.sh <deploy/cr.yaml>}"

if [ ! -f "$CR" ]; then
  echo "next-ver.sh: $CR not found" >&2
  exit 1
fi

crv="$(yq '.spec.crVersion' "$CR" 2>/dev/null | tr -d '"')"
if ! printf '%s' "$crv" | grep -Eq '^[0-9]+\.[0-9]+$'; then
  echo "next-ver.sh: crVersion in $CR is '$crv', expected major.minor" >&2
  exit 1
fi

major="${crv%%.*}"
minor="${crv##*.}"
printf '%d.%d.0\n' "$major" $((minor + 1))
