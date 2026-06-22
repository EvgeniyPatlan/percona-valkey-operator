# Backup and restore

The operator backs up Valkey with **RDB snapshots** shipped to object storage (S3, GCS, or
Azure), driven by two custom resources:

- **`PerconaValkeyBackup`** — an on-demand or scheduled snapshot.
- **`PerconaValkeyRestore`** — a restore from a backup set.

!!! note "v1alpha1 granularity"

    Backup granularity is the RDB snapshot per shard (`BGSAVE`). Point-in-Time Recovery
    (streaming AOF) is deferred beyond v1alpha1. For a sharded cluster, a backup set covers
    all shards so restore can reconstruct full slot coverage.

## Configure storage on the cluster

Backup storages and schedules live on the `PerconaValkeyCluster` (so the `valkey-db` chart
configures them through `values.yaml`):

```yaml
backup:
  enabled: true
  image: "percona/valkey-backup:{{ backuprecommended }}"
  storages:
    s3-primary:
      type: s3
      s3:
        bucket: percona-valkey-backups
        region: eu-central-1
        credentialsSecret: prod-s3-creds   # referenced, never inlined
  schedule:
    - name: nightly-full
      schedule: "0 2 * * *"
      storageName: s3-primary
      keep: 7            # retention: keep the most recent 7
      type: full
```

Storage credentials are always a Secret reference (`credentialsSecret`); the operator never
stores credentials in the CR.

## On-demand backup

```yaml
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyBackup
metadata:
  name: adhoc-2026-06-22
  namespace: valkey
spec:
  clusterName: my-valkey
  storageName: s3-primary
```

```bash
kubectl apply -f backup.yaml
kubectl -n valkey get pvk-backup -w
```

The operator runs a per-shard `BGSAVE`, ships each RDB to the storage, and writes a
backup-set manifest tying the shards together. Status reports the storage destination and
completion state.

## Scheduled backups

Scheduled backups are defined in `spec.backup.schedule` (above). The operator creates the
`PerconaValkeyBackup` objects on the cron schedule and garbage-collects old backup sets per
the `keep` retention, including the object-storage artifacts (via finalizers).

## Restore

```yaml
apiVersion: valkey.percona.com/v1alpha1
kind: PerconaValkeyRestore
metadata:
  name: restore-from-nightly
  namespace: valkey
spec:
  clusterName: my-valkey
  backupName: adhoc-2026-06-22
```

```bash
kubectl apply -f restore.yaml
kubectl -n valkey get pvk-restore -w
```

The restore seeds the RDB into the nodes and re-forms the cluster topology so slot coverage
is complete. Restore is slot-coverage-aware: an incomplete backup set is rejected rather
than silently producing a cluster with missing slots.

!!! warning "Engine downgrades require restore"

    Upgrades are forward-only and version-gated. To move to an older engine you restore from
    a backup into a cluster running the target version.

## Limitations

- RDB snapshots are point-in-time per shard; cross-shard atomicity is best-effort within the
  coordination window (no global snapshot lock across the whole cluster).
- PITR is not implemented in v1alpha1.
