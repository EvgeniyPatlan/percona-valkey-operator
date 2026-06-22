# restore (kuttl TestCase — SKELETON, owner: OPS-8.1)

Restore a cluster from a backup (arch §6 backup-restore). Suggested flow:

1. Run a backup (reuse demand-backup-s3 steps) and capture the `PerconaValkeyBackup` name.
2. `05-restore` — apply a `PerconaValkeyRestore` with `backupName` (XOR `backupSource`);
   assert slot-coverage-aware bootstrap of a fresh cluster and terminal
   `status.state: Succeeded`.
3. `06-verify-data` — read back the seeded keys; data compare pre-backup == post-restore.

Validation to cover: `backupName` XOR `backupSource` (exactly one) — arch §2.2 restore.
