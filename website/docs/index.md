# Percona Operator for Valkey

The **Percona Operator for Valkey** runs, scales, secures, backs up, and upgrades
[Valkey](https://valkey.io) clusters on Kubernetes — declaratively. You describe the
cluster you want in a single `PerconaValkeyCluster` custom resource and the operator
drives the running cluster to match: forming the cluster, assigning hash slots,
attaching replicas, rebalancing on scale, failing over before a primary is rolled, and
reflecting health back into the resource status.

!!! note "API maturity"

    The current API group is `valkey.percona.com/v1alpha1` (graduating to `v1`). The
    API is shaped so it can reach `v1` without breaking changes.

## Custom resources

| Kind | Short name | Audience | Role |
|------|-----------|----------|------|
| **PerconaValkeyCluster** | `pvk` | user-facing | The single object you write: topology, config, users, TLS, backup schedule, upgrade policy. |
| **ValkeyNode** | `vkn` | internal | Operator-created, one per pod. You never edit it. |
| **PerconaValkeyBackup** | `pvk-backup` | user-facing | On-demand or scheduled RDB snapshot to object storage. |
| **PerconaValkeyRestore** | `pvk-restore` | user-facing | Restore from a backup set into a new or in-place cluster. |

## What it does

- **Cluster mode first.** Sharded Valkey (16384 slots, CRC16 hashing) — form, heal, scale,
  and rebalance end to end.
- **Replication mode.** 1 primary + N replicas with **operator-driven failover** (no Sentinel).
- **Backup and restore as first-class CRs.** RDB snapshot per shard to S3 / GCS / Azure,
  scheduled via cron, with retention and slot-coverage-aware restore.
- **Two version axes.** `spec.crVersion` gates CR API compatibility; engine image versions
  move independently, driven by `spec.upgradeOptions` against a version service.
- **Zero-downtime rolling updates.** One pod at a time, replicas before primary, proactive
  `CLUSTER FAILOVER` before rolling a primary.
- **Security by default.** TLS in transit (cert-manager or secret ref), ACL users as
  Secrets, least-privilege RBAC (namespaced + cluster-wide), NetworkPolicy, no hardcoded
  secrets.
- **Observability first.** Prometheus exporter sidecar, PodMonitor/ServiceMonitor,
  Conditions + Events, structured logs.

## What it does not do (v1alpha1)

- No Valkey Sentinel (failover is operator-driven).
- No client-side proxy / connection router (use the cluster protocol or a headless Service).
- No engine fork — Percona ships `percona/percona-valkey` builds of upstream Valkey.
- No Point-in-Time Recovery yet (RDB-snapshot granularity); the API is shaped to add it
  without a breaking change.

## Next steps

- [Quickstart](quickstart.md) — a working cluster in a few commands.
- [Install with Helm](install-helm.md) — the recommended install path.
- [Install with OLM / OperatorHub](install-olm.md) — for OLM-managed clusters.
- [Configuration](configuration.md) — the full `PerconaValkeyCluster` surface.
- [Backup and restore](backup-restore.md) and [Upgrades](upgrades.md).
