# charts/ — Helm chart source copies (M7 Helm leg)

In-operator **source copies** of the Helm charts. The **canonical, published** charts live in
the sibling `percona-helm-charts` repo (served from `gh-pages` at
`https://percona.github.io/percona-helm-charts/`); these copies are where the M7 Helm leg
(OPS-7.4/7.5) authors and validates templates before mirroring them across.

| Chart | Installs | Analogue |
|-------|----------|----------|
| `valkey-operator` | operator Deployment, RBAC (namespaced + cluster-wide), ServiceAccount, CRDs | `psmdb-operator` |
| `valkey-db` | a `PerconaValkeyCluster` CR + referenced Secrets + backup storages | `psmdb-db` |
| `valkey-operator-crds` | CRDs only (GitOps; optional, OQ-4) | `psmdb-operator-crds` |

## Single version-source rule (READ FIRST)

There is **one** source of truth per axis in the operator repo; charts **derive** from it,
they never originate version numbers:

- **Operator axis** — `pkg/version/version.txt`. Each chart's `Chart.yaml` `appVersion` is set
  equal to it on an operator release. (Chart `version` is the chart's own semver and the only
  publish trigger — bump on any change; may run ahead of `appVersion`.)
- **Engine axis** — `e2e-tests/release_versions`. Each chart's `values.yaml` image tags are a
  hand-edited copy of those GA pins. No auto-sync; the scheduled values-drift warner flags
  divergence.
- **CRDs** — `deploy/crd.yaml` (operator repo, generated). Each chart's `crds/` is a **copy**;
  `make helm-crds-sync` refreshes it; the CRD-sync CI gate fails on drift.

See `RELEASE.md` (repo root) for the full twelve-location cross-repo checklist and the eight
Percona traps.

## Local checks (never publish from here)

    make helm-lint        # helm lint valkey-operator + valkey-db
    make helm-template     # render both
    make helm-crds-sync    # refresh valkey-operator/crds/crd.yaml from deploy/crd.yaml
    make helm-package      # local .tgz into ./bin only

Publishing is fully automated by `chart-releaser` in `percona-helm-charts` on a `Chart.yaml`
`version:` bump merged to `main` — **never** invoked from this repo.
