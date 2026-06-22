# Percona Operator for Valkey

A production-grade Kubernetes operator that declaratively runs, scales, secures, backs up, and upgrades [Valkey](https://valkey.io) clusters.

---

> ## Status: design / pre-implementation (v1alpha1 architecture)
>
> **This repository currently contains the architecture and design specification only — there is no operator binary, CRDs, or Helm charts to install yet.** The complete subsystem design lives under [`docs/architecture/`](docs/architecture/), with the master overview in [`ARCHITECTURE.md`](ARCHITECTURE.md). Commands, manifests, and images referenced below describe the **intended** v1alpha1 behaviour; treat anything marked *TBD* as not yet shipped. The API (group `valkey.percona.com/v1alpha1`) is shaped to graduate to `v1` without breaking changes.

---

## What this operator will do

The operator manages the full lifecycle of Valkey deployments through four custom resources in API group **`valkey.percona.com/v1alpha1`**:

| Kind | Short name | Audience | Role |
|------|-----------|----------|------|
| `PerconaValkeyCluster` | `pvk` | user-facing | The single object you write: topology, config, users, TLS, backup schedule, upgrade policy. |
| `ValkeyNode` | `vkn` | internal | Operator-created, one per pod; wraps a 1-replica StatefulSet (durable) or Deployment (cache). **Never edit directly.** |
| `PerconaValkeyBackup` | `pvk-backup` | user-facing | On-demand or scheduled RDB snapshot to object storage. |
| `PerconaValkeyRestore` | `pvk-restore` | user-facing | Restore a cluster from a backup set. |

Planned v1alpha1 capabilities:

- **Cluster mode first.** Sharded Valkey (16384 hash slots, CRC16) formed, healed, scaled, and rebalanced end to end; **replication mode** (1 primary + N replicas, operator-driven failover, **no Sentinel**) as a secondary target.
- **Two-CRD topology model.** A user-facing `PerconaValkeyCluster` incrementally drives one internal `ValkeyNode` per pod (one created at a time, replicas before primary), with owner references for automatic garbage collection.
- **Zero-downtime rolling updates.** One pod at a time, replicas before primary, with proactive `CLUSTER FAILOVER` before any primary is rolled; a config hash triggers rolls only when restart-required keys change.
- **Backup & restore as first-class CRs.** Per-shard RDB snapshots (`BGSAVE`) shipped to S3 / GCS / Azure, scheduled via cron, retention via finalizers, slot-coverage-aware restore. (Point-in-time recovery via AOF streaming is **explicitly deferred** beyond v1alpha1 — see [Backup & Restore](docs/architecture/06-backup-restore.md).)
- **Security by default.** TLS in transit (client `tls-port` + cluster-bus TLS) via cert-manager or a secret ref; ACL users as Secrets with internal `_operator`/`_exporter`/`_backup` system users; least-privilege RBAC (namespaced + cluster-wide); NetworkPolicy. Ports: client `6379`, cluster bus `16379`, metrics `9121`.
- **Version discipline on two axes.** `spec.crVersion` gates CR API compatibility; engine image versions move independently, driven by `spec.upgradeOptions` against a Percona-style version service (smart updates).
- **Observability first.** Prometheus exporter sidecar, PodMonitor/ServiceMonitor, `metav1.Condition`s + Kubernetes Events, structured logs, Grafana dashboards, and alert rules.
- **Production distribution.** Helm charts (`valkey-operator` + `valkey-db`), an OLM bundle + catalog (channels `stable`/`fast`/`candidate`, OperatorHub), and a `k8svalkey-docs` MkDocs site.

---

## Quickstart (commands TBD)

> The install flow is not yet implemented. Once the operator ships, the intended paths will be:

```bash
# Helm (intended) — TBD
# helm repo add percona https://percona.github.io/percona-helm-charts/
# helm install valkey-operator percona/valkey-operator
# helm install my-valkey       percona/valkey-db

# Plain manifests (intended) — TBD
# kubectl apply -f deploy/bundle.yaml          # namespaced install
# kubectl apply -f deploy/cr-minimal.yaml      # a minimal PerconaValkeyCluster

# OLM / OperatorHub (intended) — TBD
```

A minimal cluster will look like:

```yaml
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyCluster
metadata:
  name: cache
  namespace: valkey
spec:
  mode: cluster
  shards: 3
  replicas: 1
  workloadType: Deployment   # cache: no PVCs
```

See [API & CRD Design](docs/architecture/03-api-design.md) §11 for full sample manifests (minimal cache, full production cluster with TLS/ACL/backups, on-demand backup, restore).

---

## Documentation

| Document | What it covers |
|----------|----------------|
| **[ARCHITECTURE.md](ARCHITECTURE.md)** | Master architecture: big-picture diagrams, key decisions, document map, topology modes, roadmap. **Start here.** |
| [docs/architecture/00-overview.md](docs/architecture/00-overview.md) | Mission, personas, high-level architecture, design principles |
| [docs/architecture/01-decisions.md](docs/architecture/01-decisions.md) | The twelve architecture decision records (ADRs) |
| [docs/architecture/02-repo-layout.md](docs/architecture/02-repo-layout.md) | Repository layout, build system, Makefile targets, sidecar binaries |
| [docs/architecture/03-api-design.md](docs/architecture/03-api-design.md) | Authoritative CRD field tables, CEL validation, defaults, samples |
| [docs/architecture/04-control-plane.md](docs/architecture/04-control-plane.md) | Reconcile pipelines, finalizers, conditions, leader election |
| [docs/architecture/05-data-plane.md](docs/architecture/05-data-plane.md) | Topology, sharding, replication, bootstrap, scale, failover |
| [docs/architecture/06-backup-restore.md](docs/architecture/06-backup-restore.md) | RDB backup/restore model, storage, scheduling, PITR deferral |
| [docs/architecture/07-security.md](docs/architecture/07-security.md) | Threat model, TLS, ACL/users, RBAC, NetworkPolicy, secrets |
| [docs/architecture/08-observability.md](docs/architecture/08-observability.md) | Exporter, Prometheus, conditions/events, dashboards, alerts, SLOs |
| [docs/architecture/09-upgrades-versioning.md](docs/architecture/09-upgrades-versioning.md) | Two version axes, smart updates, `v1alpha1→v1` conversion |
| [docs/architecture/10-distribution-release.md](docs/architecture/10-distribution-release.md) | Images, Helm, OLM, docs site, CI/CD, cross-repo version sync |
| [docs/architecture/11-testing-qa.md](docs/architecture/11-testing-qa.md) | Test pyramid, CI gates, Kind repro harness, QA runbook |
| [docs/architecture/glossary.md](docs/architecture/glossary.md) | Definitions of terms used across the docs |

---

## Relationship to other projects

### Upstream `valkey-operator`

This operator **adopts the upstream `valkey-operator`'s two-CRD `cluster → node` topology model wholesale** — the proven slot/meet/replicate/rebalance machinery, the `ClusterState` model, the one-move-per-reconcile rebalancer, and the per-node config-hash rolling restart — and ports its internals into the Percona skeleton. Kinds are renamed to the Percona-prefixed set (`PerconaValkeyCluster`, plus the backup/restore pair) under the `valkey.percona.com` group, so this operator **coexists** with the upstream operator (which lives under `valkey.io`) in the same cluster. The internal `ValkeyNode` keeps its bare name but, under a different group, does not collide. See [ADR-001](docs/architecture/01-decisions.md#adr-001--adopt-the-two-crd-clusternode-model-from-upstream).

### The Percona operator family

`percona-valkey-operator` follows the conventions of the **Percona Operator-SDK trio** — [Percona XtraDB Cluster](https://github.com/percona/percona-xtradb-cluster-operator), [Percona Server for MongoDB](https://github.com/percona/percona-server-mongodb-operator), and [Percona Server for MySQL](https://github.com/percona/percona-server-mysql-operator) operators. It layers on the production discipline the upstream Valkey operator lacks: the `Cluster`/`Backup`/`Restore` CRD trio, `crVersion` API-compatibility gating, a Percona-style version service for smart updates, OLM + Helm distribution, centralised naming, and a kuttl/envtest quality bar — so engineers fluent in the trio can navigate this repository without re-learning a layout. See [ADR-002](docs/architecture/01-decisions.md#adr-002--operator-sdk--pkgapis--pkgcontroller-layout) and [ADR-010](docs/architecture/01-decisions.md#adr-010--distribution-via-helm--olm).

---

## Building (M0 bootstrap)

The repository is now scaffolded in the Percona SDK-trio layout with an **empty
controller-runtime manager** (no CRDs, no controllers yet — those land in M1+).
The toolchain (`controller-gen`, `kustomize`, `setup-envtest`, `mockgen`,
`golangci-lint`) auto-downloads pinned versions into a gitignored `./bin/` on
first use; only Go 1.26, `docker`/`buildx`, and (for the local loop) `kind` +
`kubectl` need to be preinstalled.

```bash
make help            # list the Percona-family targets
make build-manager   # compile the manager binary into ./bin
make test            # unit + envtest (downloads KUBEBUILDER_ASSETS)
make lint            # golangci-lint v2
make manifests       # render config/ -> deploy/{crd,rbac,operator,bundle,cw-*}.yaml
make check-generate  # CRD/deepcopy/RBAC drift gate (green on a clean tree)
make build           # single-arch operator image (--load); PUSH=true => multi-arch --push
make deploy          # kustomize-apply the empty manager to the current kube-context
```

> ### ⚠ The `VERSION` footgun (inherited from the Percona trio)
>
> `VERSION` **defaults to the sanitised git branch name**
> (`VERSION ?= $(git rev-parse --abbrev-ref HEAD | tr / -)`), and
> `IMAGE_TAG_OWNER` defaults to `perconalab`. **Always pass `VERSION=x.y.z`** to
> any build/release action, or images get tagged with the branch name and (once
> wired in M7) `crVersion`/`version.txt` are written wrong. The `release` /
> `after-release` / `bundle` / `catalog-*` targets are present but **guarded
> no-ops until M7**.

Leader election is **on by default** (`--leader-elect=true`; production posture
is `replicas: 2+`). Pass `--leader-elect=false` only for off-cluster `make run`.
`WATCH_NAMESPACE` empty means cluster-wide watch; the namespaced `deploy/`
install scopes it to the operator's own namespace (the `cw-*` artifacts are the
cluster-wide opt-in).

## Contributing

*Contribution guidelines are TBD.* Until then, the design contracts a contributor needs are in [Repository Layout & Build System](docs/architecture/02-repo-layout.md) (directory tree, `make generate`/`make manifests` regeneration discipline, package boundaries) and [Testing & Quality Assurance](docs/architecture/11-testing-qa.md) (the test pyramid, the `check-generate` CI gate, and the 80%+ coverage bar). The cardinal rule: **edit `*_types.go` and regenerate; never hand-edit generated output.**

## License

*TBD.* The operator is intended to ship under the Percona open-source licensing used across the operator family; the exact license file will be added with the first code drop.

---

*This README is the repository entry point for the design phase. For the full architecture, read [ARCHITECTURE.md](ARCHITECTURE.md).*
