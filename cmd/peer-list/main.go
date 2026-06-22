// Command peer-list is the peer-discovery SIDECAR baked into the DB image
// (percona/percona-valkey). It resolves the headless Service endpoints to
// stable pod DNS/IPs so a newly-started node can locate peers for the
// operator's batch CLUSTER MEET.
//
// STUB ONLY (M0): prints a not-implemented notice and exits non-zero. Real logic
// lands in M5 (docs/implementation/06-phase5-security-observability.md).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "peer-list: not implemented (Phase M5)")
	os.Exit(1)
}
