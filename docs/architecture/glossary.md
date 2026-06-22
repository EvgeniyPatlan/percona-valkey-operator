# Glossary

Definitions of the terms used across the **Percona Operator for Valkey** architecture set. Each entry links to the document that covers it in depth. Engine tokens (`master`/`slave`) are quoted verbatim where the engine emits them, but this operator's API and prose use **primary**/**replica**. See the master map in [ARCHITECTURE.md](../../ARCHITECTURE.md).

---

### ACL user
A Valkey access-control entry (`user <name> ...` rendered via `ACL SETUSER` syntax) defining a username, enable flag, password hash(es), and command/key/channel grants. User-defined ACL users come from `spec.users[]`; the operator renders them plus the system users into a single `users.acl` file. See [Security Architecture](07-security.md) §4 and [API & CRD Design](03-api-design.md) §2.7.

### AOF (append-only file)
Valkey's continuous-durability log, which appends every write to disk. Enabling it (`appendonly yes`) is **not** live-settable, so a change triggers a pod roll. AOF is the basis of the *deferred* point-in-time-recovery design and interacts with restore (an RDB seed boots with `appendonly no`). See [Backup & Restore](06-backup-restore.md) §7.4, §11 and [Data Plane](05-data-plane.md) §9.

### BGSAVE
The non-blocking Valkey snapshot command: it forks the server and writes `dump.rdb` to disk without blocking the foreground. The backup Job issues `BGSAVE` against each shard's live primary; the operator itself never issues `SAVE`/`BGSAVE`. See [Backup & Restore](06-backup-restore.md) §4.2.

### cluster bus
The binary gossip/control channel between Valkey cluster nodes on port **16379** (client port + 10000). It carries node discovery, health (PING/PONG, `pfail`/`fail` propagation), config-epoch reconciliation, and failover voting — **not** the replication payload. Secured by `tls-cluster yes` when TLS is on. See [Data Plane](05-data-plane.md) §2 and [Security Architecture](07-security.md) §2.2 (Boundary B4).

### condition
A standard `metav1.Condition` on a CR's `status.conditions[]` (`Ready`, `Progressing`, `Degraded`, `ClusterFormed`, `SlotsAssigned`, plus node-level conditions). Conditions are the source of truth from which `status.state` is derived; consumers should watch both `status.state` and `observedGeneration`. See [Control Plane & Reconciliation](04-control-plane.md) §7 and [Observability](08-observability.md) §4.

### CRC16 / hash slot
The hashing scheme that maps a key to one of the **16384** hash slots: `slot = CRC16(key) mod 16384` (only the substring inside `{…}` is hashed when a hash tag is present). Each primary owns a disjoint set of slots; full 16384-slot coverage is required before a cluster is `Ready`. See [Data Plane](05-data-plane.md) §2 and **slot (16384)** below.

### crVersion
`PerconaValkeyCluster.spec.crVersion` — the operator-API contract version (`major.minor`, e.g. `1.1`) the CR was written against. It **must equal** the reconciling operator's `major.minor`, is auto-stamped on first reconcile, and gates behaviour via `cr.CompareVersion()`. It is one of the two version axes and is orthogonal to the `apiVersion`. See [Upgrades & Version Management](09-upgrades-versioning.md) §2 and [API & CRD Design](03-api-design.md) §2.2.

### envtest
The controller-runtime test harness that runs a real kube-apiserver + etcd (no kubelet, so pods never run) for fast, hermetic integration tests of reconcile logic with a mocked Valkey client. Used with Ginkgo/Gomega; PR-blocking. See [Testing & Quality Assurance](11-testing-qa.md) §2.

### finalizer
A `valkey.percona.com/`-prefixed key on `metadata.finalizers` that blocks garbage collection until the operator completes ordered cleanup (cluster teardown, TLS material, backup-artifact deletion, PVC reclaim). Cleanups are idempotent and re-entrant; a wedged finalizer leaves the object `Terminating` until retried. See [Control Plane & Reconciliation](04-control-plane.md) §6 and [Backup & Restore](06-backup-restore.md) §6.

### gossip
The peer-to-peer protocol Valkey nodes use over the cluster bus to exchange membership and health state and to elect failover winners. The operator waits a beat after `CLUSTER MEET` for gossip to propagate before assigning slots. See [Data Plane](05-data-plane.md) §2–§3.

### hash slot / CRC16
See **CRC16 / hash slot** and **slot (16384)**.

### headless Service
The `valkey-<cluster>` Kubernetes Service (clusterIP `None`) that gives each pod stable DNS for the client and cluster-bus ports. Clients reach the cluster through it and follow `MOVED`/`ASK` redirects; the operator addresses individual nodes by pod IP, not the Service VIP. See [Data Plane](05-data-plane.md) §1, §10 and [API & CRD Design](03-api-design.md) §6.1.

### kuttl
The KUbernetes Test TooL — a declarative e2e harness using numbered `NN-step.yaml` + `NN-assert.yaml` TestCase pairs that diff live objects against expected partial state. This operator uses kuttl (matching PS/PG) for end-to-end suites, selected via the `run-*.csv` matrix and run on GKE by Jenkins. See [Testing & Quality Assurance](11-testing-qa.md) §3.

### node / ValkeyNode
A **node** is a single Valkey server process in one pod. **ValkeyNode** (`vkn`) is the internal CR that represents exactly one such node — it wraps a 1-replica StatefulSet (durable) or Deployment (cache) and owns that pod's PVC and config mount. Operator-created and never edited by users; named `<cluster>-<shardIndex>-<nodeIndex>`. See [API & CRD Design](03-api-design.md) §6 and [Control Plane & Reconciliation](04-control-plane.md) §3.

### OLM bundle / catalog
**OLM bundle:** an image carrying a `ClusterServiceVersion` (CSV) + CRDs + metadata/annotations — the OperatorHub artifact (distinct from the flat `deploy/bundle.yaml` install manifest). **Catalog:** an index image (built with `opm`) aggregating one or more bundles into an upgrade graph. Channel membership (`stable`/`fast`/`candidate`) is baked into the CSV at `make bundle` time. See [Distribution & Release](10-distribution-release.md) §4.

### owner reference
A `metadata.ownerReferences` entry (with `controller: true`, `blockOwnerDeletion: true`) that ties an operator-created object to its parent, driving cascading garbage collection (deleting a `PerconaValkeyCluster` cascades to its `ValkeyNode`s, their workloads, and PVCs). Backups/restores are *not* owned by the cluster — they only reference it by name. See [API & CRD Design](03-api-design.md) §1 and [Control Plane & Reconciliation](04-control-plane.md) §5.

### PDB (PodDisruptionBudget)
A `policy/v1` object the operator creates (when `spec.podDisruptionBudget: Managed`) sized to keep a quorum of primaries per shard during voluntary disruptions (node drains, upgrades). See [Control Plane & Reconciliation](04-control-plane.md) §2.1 and [API & CRD Design](03-api-design.md) §2.10.

### primary
The Valkey node currently owning a shard's slots and accepting writes (the engine reports it as `role:master`). The operator maps the engine token `master` to the API term **primary**; the live primary is read from `CLUSTER NODES` / `INFO`, never from the `node-index` label. See [Data Plane](05-data-plane.md) and **replica** below.

### RDB
The Valkey point-in-time snapshot file (`dump.rdb`) produced by `BGSAVE`. It is the v1alpha1 backup unit: one RDB per shard, shipped to object storage, loaded at node startup on restore. RDB is per-shard-consistent, not globally transactional across shards. See [Backup & Restore](06-backup-restore.md) §1, §4.

### reconcile
One pass of a controller's `Reconcile` loop: fetch current state, compute one safe step toward desired state, write status, requeue. Each phase is idempotent and re-fetches before writes; "one effect per reconcile, then requeue" makes the loop crash-safe. See [Control Plane & Reconciliation](04-control-plane.md) §2, §9.

### replica
A Valkey node that asynchronously copies a primary's dataset and can be promoted on failover (the engine reports it as `role:slave`). The operator maps the engine token `slave` to the API term **replica**; replicas serve reads only when the client opts in (`READONLY`) and add read capacity, not write capacity. See [Data Plane](05-data-plane.md) and **primary** above.

### shard / shard-group
A **shard** (a.k.a. **shard-group**) is one primary plus its `replicas` replicas, owning a contiguous portion of the 16384 hash slots. A cluster has `shards` shard-groups; a shard is expressed as a label dimension (`valkey.percona.com/shard-index`), not its own CR. See [Data Plane](05-data-plane.md) §1 and [ADR-001](01-decisions.md#adr-001--adopt-the-two-crd-clusternode-model-from-upstream).

### slot (16384)
One of the fixed **16384** hash slots (0–16383) into which Valkey cluster mode partitions the keyspace via CRC16. Each primary owns a disjoint range; the union must cover all 16384 before the cluster is `Ready`. Slots move between shards via atomic `CLUSTER MIGRATESLOTS` (Valkey 9.0+) during rebalance/scale. See [Data Plane](05-data-plane.md) §2, §4 and **CRC16 / hash slot** above.

### smart update
The Percona-style, failover-aware engine rolling upgrade: one `ValkeyNode` at a time, replicas before primary, one shard at a time, with a proactive graceful `CLUSTER FAILOVER` before rolling a primary, gated on cluster health and blocked while a backup runs. Triggered by a config/image-hash change. See [Upgrades & Version Management](09-upgrades-versioning.md) §5 and [Control Plane & Reconciliation](04-control-plane.md) §2.1 step 6.

### system user (`_operator` / `_exporter` / `_backup`)
The three operator-owned, least-privilege ACL users auto-created in `internal-<cluster>-system-passwords` (with `_`-prefixed names reserved by CEL). `_operator` orchestrates the cluster (bare `+cluster`, scoped `config`/`info`, no keyspace); `_exporter` scrapes metrics read-only; `_backup` runs server-side snapshots (`+bgsave`/`+save`/`+lastsave`). See [Security Architecture](07-security.md) §4.3, [Data Plane](05-data-plane.md) §10, and [Control Plane & Reconciliation](04-control-plane.md) §2.1 step 3.

### version service
The Percona-hosted (or self-hosted) HTTP endpoint (`check.percona.com` by default) that returns recommended/latest, mutually-compatible engine + exporter + backup image tags for a given operator/`crVersion`/platform. Polled per cluster once per `spec.upgradeOptions.schedule` window; downtime is tolerated (skip and retry next window). See [Upgrades & Version Management](09-upgrades-versioning.md) §3.
