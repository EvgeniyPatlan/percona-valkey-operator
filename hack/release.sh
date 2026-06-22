#!/usr/bin/env bash
# release.sh — the Percona "footgun engine" (docs/architecture/10 §8.1/§8.2, impl 08 OPS-7.3).
#
# Rewrites, IN PLACE, the operator-axis version (pkg/version/version.txt) and EVERY image
# field + spec.crVersion in the named deploy/cr*.yaml files. Two modes:
#
#   --owner percona               GA pinning (make release): tags from e2e-tests/release_versions
#   --owner perconalab --dev-tags main   dev re-point (make after-release): repo:main-<component>
#
# crVersion is ALWAYS major.minor of --version (docs/architecture/10 §6.1 trap 2): a patch
# release (1.1.0 -> 1.1.1) MUST NOT churn crVersion. release.sh enforces x.y.z input.
#
# EVERY image field is rewritten (R1 / trap 4): spec.image (server), spec.backup.image
# (backup), spec.exporter.image (exporter when present). A missing field is a silent GA leak,
# so the GA path asserts no perconalab/ remains afterward.
#
# Usage:
#   release.sh --version 0.1.0 --crversion 0.1 --release-versions e2e-tests/release_versions \
#     --cr deploy/cr.yaml --cr deploy/cr-minimal.yaml --owner percona
#   release.sh --version 0.2.0 --crversion 0.2 --cr deploy/cr.yaml --cr deploy/cr-minimal.yaml \
#     --owner perconalab --dev-tags main
set -euo pipefail

VERSION=""
CRVERSION=""
RELEASE_VERSIONS=""
OWNER=""
DEV_TAGS=""
CR_FILES=()

while [ $# -gt 0 ]; do
  case "$1" in
    --version)          VERSION="$2"; shift 2 ;;
    --crversion)        CRVERSION="$2"; shift 2 ;;
    --release-versions) RELEASE_VERSIONS="$2"; shift 2 ;;
    --cr)               CR_FILES+=("$2"); shift 2 ;;
    --owner)            OWNER="$2"; shift 2 ;;
    --dev-tags)         DEV_TAGS="$2"; shift 2 ;;
    *) echo "release.sh: unknown arg '$1'" >&2; exit 2 ;;
  esac
done

[ -n "$VERSION" ]   || { echo "release.sh: --version required" >&2; exit 2; }
[ -n "$CRVERSION" ] || { echo "release.sh: --crversion required" >&2; exit 2; }
[ -n "$OWNER" ]     || { echo "release.sh: --owner required (percona|perconalab)" >&2; exit 2; }
[ "${#CR_FILES[@]}" -gt 0 ] || { echo "release.sh: at least one --cr required" >&2; exit 2; }

# Footgun guard (also enforced by the Makefile): --version MUST be a real semver.
printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' \
  || { echo "release.sh: --version '$VERSION' is not x.y.z (branch-name footgun)" >&2; exit 1; }
printf '%s' "$CRVERSION" | grep -Eq '^[0-9]+\.[0-9]+$' \
  || { echo "release.sh: --crversion '$CRVERSION' is not major.minor" >&2; exit 1; }

# --- operator-axis SoT ---
echo "$VERSION" > pkg/version/version.txt
echo "release.sh: pkg/version/version.txt = $VERSION"

# --- resolve image references ---
img_server=""; img_backup=""; img_exporter=""
if [ -n "$RELEASE_VERSIONS" ]; then
  [ -f "$RELEASE_VERSIONS" ] || { echo "release.sh: $RELEASE_VERSIONS not found" >&2; exit 1; }
  # shellcheck disable=SC1090
  . "$RELEASE_VERSIONS"
fi

if [ "$OWNER" = "percona" ]; then
  # GA path — pin to the consistent set from release_versions.
  img_server="${IMAGE_VALKEY_DEFAULT:?release.sh: IMAGE_VALKEY_DEFAULT unset in release_versions}"
  img_backup="${IMAGE_BACKUP:?release.sh: IMAGE_BACKUP unset in release_versions}"
  img_exporter="${IMAGE_EXPORTER:?release.sh: IMAGE_EXPORTER unset in release_versions}"
else
  # Dev re-point path (after-release): repo:<dev-tags> under perconalab/.
  tag="${DEV_TAGS:-main}"
  img_server="perconalab/percona-valkey:${tag}"
  img_backup="perconalab/valkey-backup:${tag}"
  img_exporter="perconalab/valkey-exporter:${tag}-dev-latest"
fi

for cr in "${CR_FILES[@]}"; do
  [ -f "$cr" ] || { echo "release.sh: $cr not found" >&2; exit 1; }

  # crVersion is set on every CR that declares one. cr-minimal intentionally omits it
  # (relies on the auto-stamp), so only update where the key already exists.
  if [ "$(yq 'has("spec") and (.spec | has("crVersion"))' "$cr")" = "true" ]; then
    crv="$CRVERSION" yq -i '.spec.crVersion = strenv(crv)' "$cr"
  fi

  # EVERY image field — only where the field already exists (don't invent fields on
  # the minimal CR). server / backup / exporter.
  if [ "$(yq '.spec | has("image")' "$cr")" = "true" ]; then
    im="$img_server" yq -i '.spec.image = strenv(im)' "$cr"
  fi
  if [ "$(yq '(.spec.backup // {}) | has("image")' "$cr")" = "true" ]; then
    im="$img_backup" yq -i '.spec.backup.image = strenv(im)' "$cr"
  fi
  if [ "$(yq '(.spec.exporter // {}) | has("image")' "$cr")" = "true" ]; then
    im="$img_exporter" yq -i '.spec.exporter.image = strenv(im)' "$cr"
  fi

  echo "release.sh: $cr  crVersion=$CRVERSION  server=$img_server  backup=$img_backup  exporter=$img_exporter"

  # R1/R9 guard: a GA tree must contain NO perconalab/ in any IMAGE FIELD value. We check the
  # rendered image VALUES via yq (not a raw grep over the file) so descriptive comments
  # mentioning perconalab/ do not produce a false positive. (docs/architecture/10 §8.1 step 2)
  if [ "$OWNER" = "percona" ]; then
    leaked="$(yq -r '
      [ .spec.image, .spec.backup.image, .spec.exporter.image ]
      | .[] | select(. != null) | select(test("perconalab/"))' "$cr" 2>/dev/null || true)"
    if [ -n "$leaked" ]; then
      echo "release.sh: FAIL — $cr still has a perconalab/ image field after GA pinning (R1 leak): $leaked" >&2
      exit 1
    fi
  fi
done

echo "release.sh: done (owner=$OWNER, version=$VERSION, crVersion=$CRVERSION)"
