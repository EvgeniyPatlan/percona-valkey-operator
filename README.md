# Percona Operator for Valkey

A production-grade Kubernetes operator that declaratively runs, scales, secures, backs up, and upgrades [Valkey](https://valkey.io) clusters.

---

> ## Status: v1alpha1 implemented (M0–M8 complete) — not yet GA
>
> **The operator is fully implemented and committed on `main` (8 milestone commits, M0–M8).** All four CRDs are built with live reconciliation, and the operator ships through three distribution paths — kustomize (`make deploy`), the `valkey-operator` + `valkey-db` Helm charts, and an OLM bundle/catalog. The architecture design set lives under [`docs/architecture/`](docs/architecture/) (written during design; reconciled here to the as-built code), with the master overview in [`ARCHITECTURE.md`](ARCHITECTURE.md). The API (group `valkey.percona.com/v1alpha1`) is shaped to graduate to `v1` without breaking changes.
>
> **Not GA yet.** The fast, hermetic quality layer is green (build, envtest suites, ≥80% merged coverage, CI gates), but GA is gated on cluster-only validation (kuttl e2e on Jenkins/GKE, chaos, perf) and a handful of process items, plus two known deferrals (the `v1` conversion webhook and `expose.perPod` cluster-announce-ip). See [`docs/implementation/GA-readiness.md`](docs/implementation/GA-readiness.md) for the full sign-off gate.

---

## What this operator does

The operator manages the full lifecycle of Valkey deployments through four custom resources in API group **`valkey.percona.com/v1alpha1`**:

| Kind | Short name | Audience | Role |
|------|-----------|----------|------|
| `PerconaValkeyCluster` | `pvk` | user-facing | The single object you write: topology, config, users, auth, TLS, security context, expose, NetworkPolicy, backup schedule, upgrade policy. |
| `ValkeyNode` | `vkn` | internal | Operator-created, one per pod; wraps a 1-replica StatefulSet (durable) or Deployment (cache). **Never edit directly.** |
| `PerconaValkeyBackup` | `pvk-backup` | user-facing | On-demand or scheduled RDB snapshot to object storage. |
| `PerconaValkeyRestore` | `pvk-restore` | user-facing | Restore a cluster from a backup set. |

v1alpha1 capabilities (implemented):

- **Cluster mode first.** Sharded Valkey (16384 hash slots, CRC16) formed, healed, scaled, and rebalanced end to end; **replication mode** (1 primary + N replicas, operator-driven failover, **no Sentinel**) as a secondary target.
- **Two-CRD topology model.** A user-facing `PerconaValkeyCluster` incrementally drives one internal `ValkeyNode` per pod (one created at a time, replicas before primary), with owner references for automatic garbage collection.
- **Zero-downtime rolling updates.** One pod at a time, replicas before primary, with proactive `CLUSTER FAILOVER` before any primary is rolled; a config hash triggers rolls only when restart-required keys change.
- **Backup & restore as first-class CRs.** Per-shard RDB snapshots (`BGSAVE`) shipped to S3 / GCS / Azure, scheduled via cron, retention via finalizers, slot-coverage-aware restore. (Point-in-time recovery via AOF streaming is **explicitly deferred** beyond v1alpha1 — see [Backup & Restore](docs/architecture/06-backup-restore.md).)
- **Security by default.** Default-user `requirepass` auth (`spec.auth`); TLS in transit (client `tls-port` + cluster-bus TLS) via cert-manager or a secret ref, with mTLS policy (`spec.tls.authClients` off/optional/require), cipher/cipherSuites/DH-params controls; ACL users as Secrets with internal `_operator`/`_exporter`/`_backup` system users (the `_operator` user is orchestration-only; `_backup` carries the snapshot + replication grants — see [Security](docs/architecture/07-security.md) §4.3); hardened pod/container security contexts; `disableCommands` (e.g. `FLUSHALL`/`FLUSHDB`); least-privilege RBAC (namespaced + cluster-wide); operator-managed default-deny NetworkPolicy (`spec.networkPolicy`). Ports: client `6379`, cluster bus `16379`, metrics `9121`.
- **Flexible exposure & config.** `spec.expose` (ClusterIP/NodePort/LoadBalancer, source ranges, annotations); `spec.env` / `spec.extraEnvVars`; `spec.serviceAccountName` / `automountServiceAccountToken`.
- **Version discipline on two axes.** `spec.crVersion` gates CR API compatibility; engine image versions move independently, driven by `spec.upgradeOptions` against a Percona-style version service (smart updates).
- **Observability first.** Prometheus exporter sidecar (configurable `port` (9121), `scrapeInterval` (20s), metrics-over-TLS), PodMonitor/ServiceMonitor, `metav1.Condition`s + Kubernetes Events, structured logs, Grafana dashboards, and alert rules.
- **Production distribution.** Helm charts (`valkey-operator` + `valkey-db`, plus a standalone `valkey-operator-crds`), an OLM bundle + catalog (channels `stable`/`fast`/`candidate`, OperatorHub), and a `k8svalkey-docs` MkDocs site.

### Known deferrals (v1alpha1)

- **`v1` conversion webhook is deferred** to GA graduation. The webhook serving-cert startup gate is wired in `cmd/manager`, but the `v1`-conversion logic (`ConvertTo`/`ConvertFrom`/`Hub`) is not built in v1alpha1 — the operator ships v1alpha1-only with no in-place API conversion. See [Upgrades & Version Management](docs/architecture/09-upgrades-versioning.md) §6.
- **`expose.perPod` cluster-announce-ip is partial.** Per-pod external Services are created, but the per-node `cluster-announce-ip` wiring that cluster-mode clients need to follow MOVED/ASK redirects to per-pod addresses is incomplete. Whole-cluster NodePort/LoadBalancer expose is unaffected.

Full sign-off status, coverage numbers, and the remaining GA gates are tracked in [`docs/implementation/GA-readiness.md`](docs/implementation/GA-readiness.md).

### Milestone status

| Milestone | Scope | Status |
|-----------|-------|--------|
| **M0** Bootstrap | Repo scaffold, Makefile, codegen toolchain, manager entrypoint, baseline CI | Done |
| **M1** API & CRDs | Four CRDs, `CheckNSetDefaults`, CEL validation, `crVersion`/`CompareVersion` | Done |
| **M2** `ValkeyNode` | Workload/PVC/ConfigMap, live-config, config-hash roll, client factory seam | Done |
| **M3** Cluster | Reconcile pipeline, `ClusterState`, failover, rebalance/drain planning, conditions/status | Done |
| **M4** Backup/Restore | Backup/Restore controllers, `cmd/valkey-backup`, S3/GCS/Azure backends (RDB-only) | Done |
| **M5** Security/Observability | ACL/users, TLS, RBAC, exporter sidecar + PodMonitor | Done |
| **M6** Upgrades/Versioning | `upgradeOptions`, version service, smart update; ACL refactor (`_backup`); `v1` conversion deferred | Done (v1alpha1 scope) |
| **M7** Distribution | Helm charts, OLM bundle/catalog, docs site, release pipeline | Done (author-only; publish gated) |
| **M8** Testing/QA/Hardening | Four-layer test pyramid, kuttl suite + CSV matrices, chaos/perf, CI scan gates, GA-readiness | Done (hermetic); cluster-only runs pending |

---

## Quickstart

The operator installs three ways. Pin an explicit `VERSION=x.y.z` for any image-tagging action (see the `VERSION` footgun below).

```bash
# 1. kustomize / plain manifests (in-tree deploy/ + config/)
make deploy                                   # kustomize-apply the operator to the current kube-context
kubectl apply -f deploy/bundle.yaml           # OR the flat namespaced install bundle
kubectl apply -f deploy/cw-bundle.yaml        # OR the cluster-wide-watch variant
kubectl apply -f deploy/cr-minimal.yaml       # a minimal PerconaValkeyCluster

# 2. Helm (charts/ in this repo; published to percona-helm-charts)
helm install valkey-operator ./charts/valkey-operator
helm install my-valkey       ./charts/valkey-db
# (a standalone ./charts/valkey-operator-crds chart ships the CRDs on their own)

# 3. OLM / OperatorHub (bundle/ built via the operator-sdk/opm flow)
make bundle bundle-build bundle-push VERSION=x.y.z CHANNELS=stable DEFAULT_CHANNEL=stable
make catalog-build catalog-push     VERSION=x.y.z
```

A minimal cluster looks like:

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

## Building

The repository is built out in the Percona SDK-trio layout: the
controller-runtime manager (`cmd/manager`) hosts all four controllers, with the
CRD types under `pkg/apis/valkey/v1alpha1`, the protocol/domain layer under
`pkg/valkey`, and generated manifests in `deploy/`. The toolchain
(`controller-gen`, `kustomize`, `setup-envtest`, `mockgen`, `golangci-lint`)
auto-downloads pinned versions into a gitignored `./bin/` on first use; only
Go 1.26, `docker`/`buildx`, and (for the local loop) `kind` + `kubectl` need to
be preinstalled.

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
> any build/release action, or images get tagged with the branch name and
> `crVersion`/`version.txt` are written wrong. The `release` / `after-release` /
> `bundle` / `catalog-*` targets are wired (M7); publishing stays gated behind
> `workflow_dispatch` + a protected environment (no auto-publish on push).

Leader election is **on by default** (`--leader-elect=true`; production posture
is `replicas: 2+`). Pass `--leader-elect=false` only for off-cluster `make run`.
`WATCH_NAMESPACE` empty means cluster-wide watch; the namespaced `deploy/`
install scopes it to the operator's own namespace (the `cw-*` artifacts are the
cluster-wide opt-in).

## Contributing

*A dedicated `CONTRIBUTING.md` is not yet in the tree.* Until it lands, the contracts a contributor needs are in [Repository Layout & Build System](docs/architecture/02-repo-layout.md) (directory tree, `make generate`/`make manifests` regeneration discipline, package boundaries) and [Testing & Quality Assurance](docs/architecture/11-testing-qa.md) (the test pyramid, the `check-generate` CI gate, and the 80%+ coverage bar). The cardinal rule: **edit `*_types.go` and regenerate; never hand-edit generated output.**

## License

All source files carry **Apache License 2.0** headers, consistent with the Percona open-source licensing used across the operator family. A top-level `LICENSE` file is not yet committed; it will be added before GA.

---

*This README is the repository entry point for the implemented (M0–M8, not-yet-GA) operator. For the full architecture, read [ARCHITECTURE.md](ARCHITECTURE.md); for GA sign-off status, see [docs/implementation/GA-readiness.md](docs/implementation/GA-readiness.md).*
