# valkey-operator Helm chart (source copy)

Installs the Percona Operator for Valkey: the operator Deployment, RBAC (namespaced +
cluster-wide variants), ServiceAccount, and the CRDs under `crds/`. Mirrors the
`psmdb-operator` chart. Canonical home is the sibling `percona-helm-charts` repo; this is the
in-operator source copy the M7 Helm leg (OPS-7.4) fleshes out and keeps in sync.

## Version contract (READ THIS — single source of truth)

- **`appVersion` == `pkg/version/version.txt`** (the operator-version axis). Metadata only.
- **`version`** is the chart's own semver and the **only** publish trigger (`chart-releaser`
  `skip_existing`). Bump on **any** chart change; it may legitimately run ahead of
  `appVersion`. Forgetting it = the chart is silently skipped.
  (docs/architecture/10 §3.1, §6.1 trap 3)
- **`image.repository` default = `percona/valkey-operator`**, GA registry. Dev/main builds use
  `perconalab/valkey-operator`. `image.tag` defaults to `.Chart.AppVersion` when empty.
- **`crds/crd.yaml` is a COPY of the operator repo's `deploy/crd.yaml`.** Run
  `make helm-crds-sync` after every `make generate && make manifests`. The
  `valkey-operator-crd-sync-check` CI gate (helm repo) fails on drift.
  (docs/architecture/10 §3.3, §6.1 trap 7)

## What the leg must deliver (OPS-7.4)

- `templates/`: Deployment, ServiceAccount, and **both** RBAC scopes toggled by
  `watchAllNamespaces` (namespaced Role/RoleBinding vs cluster-wide ClusterRole/
  ClusterRoleBinding, `WATCH_NAMESPACE=""`). No wildcard `*` resources (arch 07 §6).
- Conversion-webhook plumbing matching the CRD's `conversion.strategy: Webhook` (GO-7.3).
- `tests/operator_test.yaml`: `helm-unittest` suite asserting RBAC scope toggles, image pin,
  watch-namespace env, and webhook wiring; target >= 80% of templated paths (arch 10 §3.4).
- `helm lint charts/valkey-operator` and `helm template` clean.

## Local checks (no publish)

    make helm-lint        # helm lint both charts
    make helm-template    # render to stdout
    make helm-crds-sync   # refresh crds/crd.yaml from deploy/crd.yaml
    make helm-package     # local .tgz into ./bin (NEVER pushes)

Publishing is `chart-releaser` on a `Chart.yaml` `version:` bump merged to `main` in
`percona-helm-charts` — never from this repo.
