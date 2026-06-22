# Percona Operator for Valkey 0.1.0

**Release date:** {{ date["0_1_0"] }}

The first `v1alpha1` release of the Percona Operator for Valkey.

## Highlights

- **Cluster mode** — sharded Valkey (16384 slots, CRC16 hashing): the operator forms, heals,
  scales, and rebalances the cluster end to end.
- **Replication mode** — 1 primary + N replicas with operator-driven failover (no Sentinel).
- **Two-CRD topology** — `PerconaValkeyCluster` (intent) drives internal `ValkeyNode` CRs
  (one per pod) via owner references.
- **Backup and restore** — `PerconaValkeyBackup` / `PerconaValkeyRestore`: RDB snapshots to
  S3 / GCS / Azure, scheduled via cron, with retention and slot-coverage-aware restore.
- **Two version axes** — `spec.crVersion` gates CR API compatibility; engine image versions
  move independently via `spec.upgradeOptions` and a version service.
- **Zero-downtime rolling updates** — replicas before primary, proactive `CLUSTER FAILOVER`
  before rolling a primary.
- **Security by default** — TLS via cert-manager or secret ref, ACL users as Secrets,
  least-privilege RBAC (namespaced + cluster-wide), NetworkPolicy.
- **Observability** — Prometheus exporter sidecar, PodMonitor/ServiceMonitor, Conditions +
  Events, structured logs.
- **Distribution** — Helm charts (`valkey-operator` + `valkey-db`), an OLM bundle + catalog
  (`candidate` channel at `v1alpha1`), and this documentation site.

## Components

| Component | Version |
|-----------|---------|
| Operator | {{ release }} |
| Valkey server (default) | {{ valkeydefaultrecommended }} |
| Valkey server (lines) | {{ valkey80recommended }}, {{ valkey90recommended }} |
| Backup tool | {{ backuprecommended }} |
| Exporter | {{ exporterrecommended }} |
| cert-manager (tested) | {{ certmanagerrecommended }}+ |
| Kubernetes (tested) | {{ kubernetesmin }}+ |

## Known limitations (v1alpha1)

- No Valkey Sentinel; failover is operator-driven.
- No Point-in-Time Recovery (RDB-snapshot backup granularity).
- No standalone mode, multi-region replication, or managed Valkey modules.
- Engine downgrades require restore-from-backup (upgrades are forward-only).
- TLS certificate rotation triggers a config-hash-driven rolling restart (no live hot-swap).

## Installation

See [Install with Helm](../install-helm.md) or [Install with OLM](../install-olm.md).
