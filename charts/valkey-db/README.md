# valkey-db Helm chart (source copy)

Installs a `PerconaValkeyCluster` CR plus its referenced Secrets (ACL users, TLS) and backup
storages. Mirrors the `psmdb-db` chart. The operator (installed via the `valkey-operator`
chart) owns the actual workload; this chart only authors the CR + Secrets.

## Version contract

- **`appVersion` == `pkg/version/version.txt`** (operator axis). Metadata only.
- **`version`** = chart's own semver and the only publish trigger; bump on any change.
- **`image` / `exporter.image` / `backup.image` defaults = GA `percona/*` tags** that must
  match `e2e-tests/release_versions` after `make release`. This is the **third** hand-edited
  copy of the engine pins (operator `release_versions`, chart `values.yaml`, docs
  `variables.yml`) — no auto-sync (arch 10 §3.2, §6.1 trap 4). The scheduled values-drift
  warner surfaces divergence.

## values ↔ CR mapping (the leg's core job — OPS-7.5)

Every top-level key maps 1:1 to a `PerconaValkeyCluster` `spec.*` field:

Every top-level key maps to a `PerconaValkeyCluster` `spec.*` field (the chart authors the
CR; the operator owns the workload):

| values key | CR field |
|------------|----------|
| `mode`, `shards`, `replicas`, `workloadType`, `pause`, `crVersion` | `spec.mode`/`shards`/`replicas`/`workloadType`/`pause`/`crVersion` |
| `image`, `imagePullSecrets`, `resources` | `spec.image`/`imagePullSecrets`/`resources` |
| `persistence` (`enabled` gates the block; omitted for cache mode) | `spec.persistence` |
| `config` (map) | `spec.config` |
| `auth` (default-user requirepass; Secret-ref) | `spec.auth` |
| `tls` (`secretName` XOR `certManager`; `authClients`/`ciphers`/`cipherSuites`) | `spec.tls` |
| `users[]` | `spec.users[]` (+ externally-referenced Secrets) |
| `disableCommands[]` | `spec.disableCommands` |
| `exporter` (`port`/`scrapeInterval`/`tls`) | `spec.exporter` |
| `expose` (`type`/`perPod`/`loadBalancerSourceRanges`/`annotations`) | `spec.expose` |
| `networkPolicy` (`monitoringNamespace`/selectors) | `spec.networkPolicy` |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | the matching `spec.*` scheduling fields |
| `podDisruptionBudget` (`Managed`/`Disabled`) | `spec.podDisruptionBudget` |
| `backup` (`storages`/`schedule`) | `spec.backup` |
| `upgradeOptions` | `spec.upgradeOptions` |

`secrets.defaultUserPassword` is a **dev/test-only** convenience that creates the
`auth.passwordSecret` Secret from a literal; it is never written into the CR. Production
references a pre-existing, externally-managed Secret by name and leaves it empty.

### Reference-chart surface alignment

The values surface mirrors the user's existing `percona-valkey` chart **where it maps to a CR
field**: `auth` ↔ reference `auth`; `tls`/`certManager` ↔ reference `tls`; `expose` ↔
reference `externalAccess`; `networkPolicy` ↔ reference `networkPolicy`; `disableCommands` ↔
reference `disableCommands`; `exporter` ↔ reference `metrics`. Reference knobs with **no CR
analogue** are intentionally dropped because the operator owns them: the reference
`sentinel` mode (operator does `replication` failover without Sentinel), raw workload knobs
(`statefulset`, `securityContext`, probes, `sysctlInit`, `volumePermissions`, HPA/VPA,
`lifecycle`), and chart-managed Services/ConfigMaps — all are operator-reconciled, not CR
spec fields.

## What the leg must deliver

- `templates/`: the CR render + referenced Secret stubs (never inline real secrets — arch 07).
- `tests/db_test.yaml`: `helm-unittest` covering mode/shards/storage/exporter/backup paths
  (>= 80% templated paths).
- A `valkey-operator-crd-sync-check`-style gate lives in the helm repo (OPS-7.5).

## Local checks (no publish)

    helm lint charts/valkey-db
    helm template valkey-db charts/valkey-db
