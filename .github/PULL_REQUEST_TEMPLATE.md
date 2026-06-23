<!--
Thanks for contributing to the Percona Valkey Operator. Fill in every section.
Keep the PR focused: one logical change per PR (see CLAUDE.md per-repo conventions).
-->

## What

<!-- One or two sentences: what does this PR change and why. -->

## Linked issue / task

<!-- e.g. Fixes #123 / GO-x.y / CR-n. -->

## Type of change

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds functionality)
- [ ] Breaking change (fix or feature that changes existing behavior / API)
- [ ] Refactor / chore (no functional change)
- [ ] Docs / CI only

## Regression test (must fail on `main`)

Every bug fix and behavior change ships a test that **fails before the fix and passes
after it**. This proves the test actually covers the change and is not a tautology.

- [ ] Added/updated a test that **reproduces the bug / exercises the new behavior**
- [ ] Verified the new test **FAILS on `main`** (without this PR's non-test changes)
- [ ] Verified the new test **PASSES with this PR**
- [ ] If this is intentionally test-free (docs/CI/chore only), say why here:

<!-- Paste the failing-on-main run, e.g.:
    git stash -- <non-test files> && go test ./pkg/... -run TestNewThing  # expect FAIL
    git stash pop && go test ./pkg/... -run TestNewThing                  # expect PASS
-->

## Definition of Done (per-PR DoD)

- [ ] `go build ./...` and `go vet ./...` are clean
- [ ] `gofmt`/`goimports` clean on touched files
- [ ] `golangci-lint run` reports **0 issues** on touched packages (no `//nolint`,
      no dot-imports, initialisms respected, exported symbols documented)
- [ ] `make test` (unit + envtest) passes
- [ ] **Generated code is regenerated, not hand-edited**: ran `make generate` +
      `make manifests` after any `*_types.go` change, and `make check-generate` is green
- [ ] Coverage does not regress (target: 80%+ on touched packages)
- [ ] Metadata persisted via `client.MergeFrom` PATCH, never a full `Update`
      (the omitempty + defaults round-trip footgun)
- [ ] Cross-repo version axes updated if a release-relevant constant changed
      (operator vs engine pins; see CLAUDE.md "Cross-repo version bumps")
- [ ] No secrets/credentials committed; user input validated at boundaries

## Test plan

<!-- How a reviewer reproduces the verification. List the exact commands. -->

```
go build ./... && go vet ./...
KUBEBUILDER_ASSETS="$(./bin/setup-envtest use 1.34.1 -p path)" go test ./...
```

## Notes for reviewers

<!-- Anything that needs special attention, follow-ups, or known limitations. -->
