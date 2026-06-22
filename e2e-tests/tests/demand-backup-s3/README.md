# demand-backup-s3 (kuttl TestCase — SKELETON, owner: OPS-8.1)

On-demand RDB backup to S3 (arch §2.2 backup, §6 backup-restore). RDB-only in v1alpha1
(PITR deferred). Suggested flow:

1. `00`–`02` reuse init-cluster (cluster Ready), seed data.
2. `03-storage-secret` — create the S3 credentials Secret + a `backup.storages[s3-bucket]`
   entry on the CR.
3. `04-create-backup` — apply a `PerconaValkeyBackup` referencing `storageName: s3-bucket`.
4. `04-assert` — `status.state: Succeeded`, `status.destination` carries the `s3://`
   prefix, per-shard `BGSAVE` RDBs uploaded, slot union covers 0..16383.

Negative variant to consider: a `storageName` typo must yield an explicit error
(no silent skip) — arch §2.2 backup item 1.
