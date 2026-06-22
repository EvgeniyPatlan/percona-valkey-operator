#!/usr/bin/env bash
# regen-fixtures.sh — regenerate unit-test golden fixtures that assert image tags after a
# `make release` rewrite (GO-7.4; docs/architecture/10 §8.1 step 4). The trio convention is
# that such fixtures are REGENERATED, never hand-edited, so `make test` stays green after a
# release. This is the single deterministic entry point the Makefile `regen-fixtures` target
# and the Go track share.
#
# Mechanism: golden tests in this repo follow the Go convention of regenerating via a
# `-update` (a.k.a. UPDATE_GOLDEN) flag. We invoke `go test` on any package carrying golden
# fixtures with UPDATE_GOLDEN=1 set so its TestMain/flag rewrites the committed golden.
#
# It is intentionally a no-op-tolerant scan: if no golden-bearing package exists yet (early
# milestones), it prints a notice and exits 0 rather than failing the release.
set -euo pipefail

GO="${GO:-go}"

# Packages that hold image-asserting golden fixtures (defaults snapshots). Extend this list
# as fixtures are added; keep it explicit so regen is deterministic and reviewable.
GOLDEN_PKGS=(
  ./pkg/apis/valkey/v1alpha1/...
)

found=0
for pkg in "${GOLDEN_PKGS[@]}"; do
  # Only run where the package actually has *_test.go with a golden updater.
  dirs="$($GO list -f '{{.Dir}}' "$pkg" 2>/dev/null || true)"
  for d in $dirs; do
    if ls "$d"/*_test.go >/dev/null 2>&1 && grep -lqE 'UPDATE_GOLDEN|-update|update-golden' "$d"/*_test.go 2>/dev/null; then
      found=1
      echo "regen-fixtures: updating golden in $d"
      ( UPDATE_GOLDEN=1 $GO test "$d/..." -run '.*[Gg]olden.*' -count=1 ) || \
        ( UPDATE_GOLDEN=1 $GO test "$d/..." -count=1 )
    fi
  done
done

if [ "$found" -eq 0 ]; then
  echo "regen-fixtures: no image-asserting golden fixtures found yet — nothing to regenerate."
fi
