# Release runbook — Percona Operator for Valkey

> **Authoritative source:** [docs/architecture/10-distribution-release.md](docs/architecture/10-distribution-release.md)
> and [docs/implementation/08-phase7-devops-distribution.md](docs/implementation/08-phase7-devops-distribution.md).
> This file is the operational checklist; the architecture doc is the rationale.

> **⚠️ PUBLISHING REQUIRES HUMAN APPROVAL.** Every outward action — `git push`, `git tag`
> push, `docker push` / `buildx --push`, `helm` chart publish / `chart-releaser`, `opm` /
> `operator-sdk` bundle/catalog push, OperatorHub (`community-operators`) submission, and
> `mike deploy` of the docs site — is performed **by a human release engineer**, never by
> automation or an assistant. The `make` targets here **author and locally validate**
> artifacts; the push-class targets (`bundle-push`, `catalog-push`) are hard-guarded behind
> `PUSH=true` and must be invoked deliberately by a person.

---

## 0. The two version axes (never conflate them)

| Axis | Source of truth | Propagates to |
|------|-----------------|---------------|
| **Operator version** (e.g. `0.1.0`) | `pkg/version/version.txt` | `deploy/cr*.yaml` `spec.crVersion` (= `major.minor`); Helm `appVersion`; docs `variables.yml` `release:` |
| **Engine / sidecar images** (Valkey/backup/exporter tags) | `e2e-tests/release_versions` | `deploy/cr*.yaml` image fields; chart `values.yaml`; docs `variables.yml` `*recommended:` + `versions.md` |

`spec.crVersion` is the operator **`major.minor` only** (e.g. `0.1`), never the full patch —
so patch upgrades (`0.1.0 → 0.1.1`) must never change it.

---

## 1. `percona/` vs `perconalab/` registry split

- **`percona/<image>:<version>`** — GA, immutable, signed. Written into `deploy/cr*.yaml`
  **only** by `make release VERSION=x.y.z`. What end users run.
- **`perconalab/<image>:main-*`** — dev / `main` builds. `make after-release` repoints
  `cr*.yaml` back to these for the next dev cycle.
- The four images: `percona/valkey-operator`, `percona/percona-valkey` (server + sidecars),
  `percona/valkey-backup`, `percona/valkey-exporter`. Default GA matrix is
  `linux/amd64,linux/arm64`; `s390x`/`ppc64le` are opt-in (call out in release notes).
- `IMAGE_TAG_OWNER` defaults to `perconalab`; GA overrides it to `percona`.

---

## 2. Release-branch flow

Releases are cut on **`release-X.Y.Z` branches**, not `main`. The `make after-release`
re-point lands back on `main`.

```
git checkout -b release-0.1.0
# edit e2e-tests/release_versions (bump IMAGE_OPERATOR + engine pins) FIRST
make release VERSION=0.1.0          # GA pinning, in place
git diff                            # review: version.txt, cr*.yaml, e2e-tests/vars.sh ONLY
git commit ...                      # (human)
# run-release.csv e2e on Jenkins/GKE → green
git tag v0.1.0                      # (human) — MUST exist before docs publish
# build & push GA images            # (human, percona/*) — buildx --push
git checkout main
make after-release                  # repoint to perconalab/*:main-*, NEXT_VER auto-derived
git commit ...                      # (human)
```

> **Never build/ship a GA release from a post-`after-release` tree** — its images point at
> `perconalab/*:main-*`. Cut GA from the `release-x.y.z` branch *before* `after-release`.

---

## 3. `make` vocabulary (this repo)

| Target | Does | Pushes? |
|--------|------|---------|
| `make release VERSION=x.y.z` | version.txt + `crVersion`=`major.minor` + every `cr*.yaml` image → GA `percona/*` from `release_versions`; sync `CERT_MANAGER_VER`; regen fixtures | No |
| `make after-release [NEXT_VER=x.y.z]` | `NEXT_VER` = `major.(minor+1).0` from `cr.yaml` `crVersion`; repoint images → `perconalab/*:main-*`; `update-version` | No |
| `make update-version` | writes `NEXT_VER` into `version.txt` **only** (not `crVersion`) | No |
| `make check-version` | gate: `version.txt major.minor == cr.yaml crVersion` | No |
| `make build` / `docker-buildx` | operator image (single-arch `--load`; multi-arch only when `PUSH=true`) | Only if `PUSH=true` |
| `make build-server` / `build-backup` / `build-exporter` / `build-all` | the other three images | Only if `PUSH=true` |
| `make bundle VERSION=x.y.z` | operator-sdk OLM bundle (CSV+CRDs+metadata) → `./bundle`, validated | No |
| `make bundle-build` | build the bundle image locally | No |
| `make bundle-push` | **guarded** — requires `PUSH=true` + human approval | Yes (human only) |
| `make catalog-build VERSION=x.y.z` | `opm index add --mode semver` → catalog image locally | No |
| `make catalog-push` | **guarded** — requires `PUSH=true` + human approval | Yes (human only) |
| `make olm-validate` | apply the catalog as a `CatalogSource` on a **local** kind+OLM cluster | No |
| `make helm-lint` / `helm-template` / `helm-package` / `helm-crds-sync` | chart checks + local package + CRD copy | No |

> **`VERSION` defaults to the sanitized branch name (the #1 footgun).** Always pass
> `VERSION=x.y.z` for any `release`/`build`/`bundle`/`catalog` action; the targets abort if
> `VERSION` is not a real `x.y.z`.

---

## 4. The twelve-location cross-repo checklist

The same numbers touch **twelve locations across three repos** with zero cross-repo sync
tooling. Two (`cr*.yaml` `crVersion` + image fields) are rewritten by `make release`; the
other ten are hand-edited. Use this for **every** release.

```
OPERATOR REPO (percona-valkey-operator)
  [ ] pkg/version/version.txt           = operator version           (SoT, operator axis)
  [ ] deploy/cr*.yaml  spec.crVersion   = operator major.minor        (written by `make release`)
  [ ] e2e-tests/release_versions        = all IMAGE_* engine pins     (SoT, engine axis)
  [ ] deploy/cr*.yaml  image fields     = GA percona/* engine tags    (written by `make release`)

HELM REPO (percona-helm-charts)
  [ ] charts/valkey-operator/Chart.yaml appVersion = operator version
  [ ] charts/valkey-db/Chart.yaml       appVersion = operator version
  [ ] both Chart.yaml                   version    = chart semver (PUBLISH TRIGGER; >= prior)
  [ ] charts/valkey-operator/crds/      = copied from operator deploy/crd.yaml (CRD-sync gate)
  [ ] charts/*/values.yaml              image tags = release_versions GA pins

DOCS REPO (k8svalkey-docs)
  [ ] variables.yml  release:           = operator version (git tag v<release> MUST exist)
  [ ] variables.yml  *recommended:      = release_versions pins
  [ ] docs/versions.md                  compatibility-matrix row added
```

**Merge order across repos: operator → helm-charts → docs.** Operator first (sets tag
`vx.y.z`, pins GA images); `chart-releaser` auto-publishes on the chart `version` bump; `mike`
docs publish needs the tag to already exist.

---

## 5. The eight Percona traps (called out explicitly)

1. **`VERSION` defaults to the branch name.** Always pass `VERSION=x.y.z` to
   `make release`/`build`/`bundle`/`catalog-*`. (#1 footgun; the targets guard for it.)
2. **`crVersion` mismatch breaks CR API compatibility.** `crVersion` is `major.minor` only;
   patch upgrades must not change it. `make update-version` writes only `version.txt` — it
   does **not** touch `crVersion`. `make check-version` asserts they agree.
3. **Helm `version` ≠ `appVersion`.** `version` (chart semver) is the only publish trigger and
   may run ahead of `appVersion`. Forgetting it = `chart-releaser` silently skips the chart.
4. **Three hand-edited image-pin copies drift.** `e2e-tests/release_versions`, chart
   `values.yaml`, docs `variables.yml` — no sync; all three must match after `make release`.
5. **Docs `release:` must equal an existing operator tag `v<release>`** or all
   `blob/v{{release}}/...` links 404. Tag the operator **before** publishing docs.
6. **`make after-release` repoints `cr.yaml` to `perconalab/*:main-*` dev tags.** Never
   build/ship a GA release from a tree in that state; cut GA from the release branch.
7. **CRD sync between operator and charts is manual.** Run `make helm-crds-sync` (copies
   `deploy/crd.yaml` → `charts/valkey-operator/crds/`) after every `make generate`; the
   CRD-sync CI gate catches drift.
8. **Operator and charts/docs are separate repos with separate PRs.** A version bump in the
   operator is **not done** until the matching `percona-helm-charts` and `k8svalkey-docs` PRs
   land. The chart repo typically lags the operator.

---

## 6. OLM bundle + catalog (PS-style)

Only this operator (like PS) builds OLM artifacts. Channels: `candidate` / `fast` / `stable`.

> **At `v1alpha1` (OQ-3), default to `candidate` only.** Open `fast`/`stable` at GA. Channel
> membership is baked into the CSV at `make bundle` time, not at catalog time.

```
make bundle VERSION=0.1.0 CHANNELS=candidate DEFAULT_CHANNEL=candidate   # v1alpha1
#   GA: CHANNELS=candidate,fast,stable DEFAULT_CHANNEL=stable
make bundle-build                       # local image, no push
make catalog-build VERSION=0.1.0        # opm index add --mode semver
make olm-validate                       # LOCAL kind+OLM CatalogSource validation
# bundle-push / catalog-push are HUMAN-ONLY and require PUSH=true
# submit ./bundle to operator-framework/community-operators  (HUMAN)
```

`opm index add --mode semver` orders the upgrade graph, so **bundle versions must be valid
semver** or the graph breaks. Use `CATALOG_BASE_IMG=<prior-catalog>` for incremental
(`--from-index`) catalogs.

---

## 7. Docs per-release checklist (k8svalkey-docs, one PR)

1. `variables.yml` — bump `release:`; update **every** `*recommended:` pin to match
   `release_versions`; add the new `date:` entry.
2. `docs/ReleaseNotes/` — add `Kubernetes-Operator-for-Valkey-RN<version>.md`.
3. `mkdocs-base.yml` — add the release-notes file to the `Release Notes:` nav (newest first).
4. `docs/versions.md` — add a compatibility-matrix row (duplicates `*recommended` — keep
   consistent).
5. `mike deploy <version>` — add the `latest` alias **only** for a GA release (not
   `v1alpha1`). (HUMAN; operator tag `v<version>` must already exist.)

---

## 8. Drift guards (cheap, in-repo)

- **`check-version`** — `version.txt major.minor == cr.yaml crVersion` (CI gate + `make`).
- **values-drift warner** — scheduled CI compares `release_versions` ↔ chart `values.yaml`
  (warning, not blocking).
- **docs-tag verifier** — docs CI fails if `variables.yml release:` has no matching `v<release>`
  operator tag.

Full cross-repo automation is deferred (OQ-6) — these are the high-value/low-cost guards.

---

## 9. CI/CD split

- **GitHub Actions (PR + `main`)** — `tests.yml` (unit + envtest), `lint.yml`
  (`golangci-lint` + `fmt`/`vet`), `check-generate.yml` (`make generate manifests` +
  `git diff --exit-code`), `check-version.yml`, `scan.yml` (Trivy/Grype), `publish.yml`
  (multi-arch buildx; **push only on `main`/tag**; `perconalab/` for `main`, `percona/` for
  GA tags). **Actions never spins up a Valkey cluster.**
- **Jenkins** — kuttl e2e on GKE from the `run-*.csv` matrices; self-destructing clusters.
