// Command valkey-backup is the backup/restore SIDECAR that runs in a Kubernetes
// Job pod inside the DB image (percona/percona-valkey), not the operator image.
// It issues BGSAVE on a shard primary and ships the RDB to object storage
// (S3/GCS/Azure), and streams an RDB set back on restore.
//
// STUB ONLY (M0): prints a not-implemented notice and exits non-zero. Real logic
// lands in M4 (docs/implementation/05-phase4-backup-restore.md).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "valkey-backup: not implemented (Phase M4)")
	os.Exit(1)
}
