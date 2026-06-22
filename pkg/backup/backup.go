// Package backup is the storage-backend abstraction: destination-prefix parsing
// (s3://, gs://, azure://, pvc/) and the per-backend client used by the
// backup/restore controllers and the cmd/valkey-backup sidecar.
//
// Empty seam in M0 — implemented in M4 (see
// docs/implementation/05-phase4-backup-restore.md).
package backup
