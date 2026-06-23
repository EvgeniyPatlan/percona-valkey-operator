#!/usr/bin/env bash
#
# cut-release.sh — cut a full, installable release of the Percona Valkey Operator so
# end users can install and test it. End to end:
#   1. build & push the two images (operator + backup/sidecar)
#   2. render install manifests PINNED to those images (into ./dist, no tracked files touched)
#   3. create a GitHub Release (which also creates the git tag) with the manifests attached
#
# This is the simple, self-contained "personal release" path. It is DISTINCT from
# hack/release.sh, which does the Percona-style in-place version/image pinning of
# version.txt + deploy/cr*.yaml (used by `make release`). Use this one to ship a
# testable build under your own account/registry.
#
# USAGE
#   VERSION=0.1.0 ./hack/cut-release.sh [flags]
#
# REQUIRED
#   VERSION            semver, no leading 'v'   (e.g. 0.1.0)
#
# OPTIONAL (env or flag)
#   REGISTRY           container registry            (default: ghcr.io)
#   OWNER              registry namespace/user       (default: your lowercased GitHub login)
#   PLATFORMS          buildx platforms              (default: linux/amd64,linux/arm64)
#   --single-arch      build linux/amd64 only (no buildx multi-arch needed)
#   --skip-images      do NOT build/push images (use when CI already pushed them)
#   --no-latest        do not also tag images ':latest'
#   --dry-run          print every action; push/tag/release NOTHING
#   -h | --help        show this help
#
# PREREQUISITES: git, docker (with buildx for multi-arch), the GitHub CLI `gh`
# (logged in), and network access to the registry. kustomize is fetched into ./bin.
#
# EXAMPLES
#   VERSION=0.1.0 ./hack/cut-release.sh                       # GHCR, multi-arch, full release
#   VERSION=0.1.0 ./hack/cut-release.sh --single-arch         # amd64 only (no buildx)
#   VERSION=0.1.0 REGISTRY=docker.io OWNER=evgeniypatlan ./hack/cut-release.sh
#   VERSION=0.1.0 ./hack/cut-release.sh --dry-run             # preview only
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

SINGLE_ARCH=false ; SKIP_IMAGES=false ; NO_LATEST=false ; DRY_RUN=false
for arg in "$@"; do
  case "$arg" in
    --single-arch) SINGLE_ARCH=true ;;
    --skip-images) SKIP_IMAGES=true ;;
    --no-latest)   NO_LATEST=true ;;
    --dry-run)     DRY_RUN=true ;;
    -h|--help)     sed -n '2,38p' "${BASH_SOURCE[0]}" | sed 's/^#\{1,\} \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $arg (try --help)"; exit 2 ;;
  esac
done

: "${VERSION:?set VERSION (semver, no leading v) — e.g. VERSION=0.1.0}"
if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "ERROR: VERSION='$VERSION' is not semver (expected like 0.1.0)"; exit 2
fi
REGISTRY="${REGISTRY:-ghcr.io}"
if [[ -z "${OWNER:-}" ]]; then
  OWNER="$(gh api user -q .login 2>/dev/null | tr 'A-Z' 'a-z')" || true
  [[ -z "$OWNER" ]] && { echo "ERROR: set OWNER (could not derive it from gh)"; exit 2; }
fi
TAG="v$VERSION"
OPERATOR_IMAGE="$REGISTRY/$OWNER/valkey-operator"
BACKUP_IMAGE="$REGISTRY/$OWNER/valkey-backup"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
$SINGLE_ARCH && PLATFORMS="linux/amd64"
DIST="$ROOT/dist"

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
run() { if $DRY_RUN; then printf '   [dry-run] %s\n' "$*"; else eval "$*"; fi; }

cat <<EOF
Release plan
  version          : $VERSION  (git tag: $TAG)
  operator image   : $OPERATOR_IMAGE:$VERSION$([[ $NO_LATEST == false ]] && echo " (+ :latest)")
  backup image     : $BACKUP_IMAGE:$VERSION$([[ $NO_LATEST == false ]] && echo " (+ :latest)")
  platforms        : $PLATFORMS
  build/push images: $([[ $SKIP_IMAGES == true ]] && echo "NO (--skip-images)" || echo "yes")
  dry-run          : $DRY_RUN
EOF
echo

say "Preflight checks"
[[ -f Dockerfile && -f Dockerfile.sidecar ]] || { echo "ERROR: run from the operator repo root"; exit 1; }
if [[ -n "$(git status --porcelain)" ]]; then
  echo "ERROR: working tree is not clean — commit or stash first"; git status --short; exit 1
fi
gh auth status >/dev/null 2>&1 || { echo "ERROR: 'gh' is not authenticated (run: gh auth login)"; exit 1; }
git rev-parse "$TAG" >/dev/null 2>&1 && { echo "ERROR: tag $TAG already exists"; exit 1; }
gh release view "$TAG" >/dev/null 2>&1 && { echo "ERROR: release $TAG already exists"; exit 1; }
command -v docker >/dev/null || { echo "ERROR: docker not found"; exit 1; }
if [[ $SKIP_IMAGES == false && $SINGLE_ARCH == false ]] && ! docker buildx version >/dev/null 2>&1; then
  echo "ERROR: docker buildx not available — install it, pass --single-arch, or build in CI."; exit 1
fi
say "Ensuring kustomize is present"
run "make kustomize >/dev/null"
KUSTOMIZE="$ROOT/bin/kustomize"

if [[ $SKIP_IMAGES == false ]]; then
  say "Logging in to $REGISTRY"
  if [[ "$REGISTRY" == "ghcr.io" ]]; then
    run "gh auth token | docker login ghcr.io -u '$OWNER' --password-stdin"
  else
    echo "   (assuming you are already 'docker login'-ed to $REGISTRY)"
  fi
  LAT=""; [[ $NO_LATEST == false ]] && LAT="-t $OPERATOR_IMAGE:latest"
  say "Building + pushing operator image ($OPERATOR_IMAGE:$VERSION)"
  run "docker buildx build --platform '$PLATFORMS' --push --build-arg VERSION='$VERSION' \
       -t '$OPERATOR_IMAGE:$VERSION' $LAT -f Dockerfile ."
  LATB=""; [[ $NO_LATEST == false ]] && LATB="-t $BACKUP_IMAGE:latest"
  say "Building + pushing backup/sidecar image ($BACKUP_IMAGE:$VERSION)"
  run "docker buildx build --platform '$PLATFORMS' --push --build-arg VERSION='$VERSION' \
       -t '$BACKUP_IMAGE:$VERSION' $LATB -f Dockerfile.sidecar ."
else
  say "Skipping image build/push (--skip-images)"
fi

say "Rendering install manifests pinned to $OPERATOR_IMAGE:$VERSION (into ./dist)"
run "mkdir -p '$DIST'"
run "(cd config/manager && '$KUSTOMIZE' edit set image controller='$OPERATOR_IMAGE:$VERSION')"
run "'$KUSTOMIZE' build config/default      > '$DIST/bundle.yaml'"
run "'$KUSTOMIZE' build config/cluster-wide > '$DIST/cw-bundle.yaml'"
run "git checkout -- config/manager/kustomization.yaml"
run "cp deploy/cr-minimal.yaml '$DIST/cr-minimal.yaml'"

NOTES="$DIST/RELEASE_NOTES_$TAG.md"
if ! $DRY_RUN; then
cat > "$NOTES" <<EOF
## Install

\`\`\`bash
# Operator + CRDs (namespaced install):
kubectl apply --server-side -f \
  https://github.com/$OWNER/percona-valkey-operator/releases/download/$TAG/bundle.yaml

# A 3-shard test cluster:
kubectl apply -f \
  https://github.com/$OWNER/percona-valkey-operator/releases/download/$TAG/cr-minimal.yaml
\`\`\`

For a cluster-wide (all-namespaces) install use \`cw-bundle.yaml\`.
Next steps + verification: see \`docs/scenarios-and-verification.md\`.

## Images
- Operator: \`$OPERATOR_IMAGE:$VERSION\`
- Backup:   \`$BACKUP_IMAGE:$VERSION\`  (only needed to test backup/restore; set \`spec.backup.image\`)
- Engine:   \`percona/valkey:9.1.0\` (pulled from Docker Hub)
EOF
fi

say "Creating GitHub Release $TAG (creates the git tag at HEAD) + attaching manifests"
run "gh release create '$TAG' \
     --target '$(git rev-parse HEAD)' \
     --title '$TAG' \
     --notes-file '$NOTES' \
     '$DIST/bundle.yaml' '$DIST/cw-bundle.yaml' '$DIST/cr-minimal.yaml'"

echo
if $DRY_RUN; then
  say "DRY-RUN complete — nothing was pushed, tagged, or released."
else
  say "Released: https://github.com/$OWNER/percona-valkey-operator/releases/tag/$TAG"
  if [[ "$REGISTRY" == "ghcr.io" ]]; then
    echo "   One-time: make the GHCR packages PUBLIC (GitHub → Profile → Packages →"
    echo "   valkey-operator / valkey-backup → Package settings → Change visibility → Public),"
    echo "   otherwise users get 'manifest unknown' / pull-auth errors."
  fi
fi
