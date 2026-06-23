// Command healthcheck is the liveness/readiness probe SIDECAR baked into the DB
// image (percona/valkey). Readiness checks cluster_state:ok and (for
// replicas) master_link_status:up via local INFO/CLUSTER INFO; liveness checks
// the server responds.
//
// STUB ONLY (M0): prints a not-implemented notice and exits non-zero. Real logic
// lands in M5 (docs/implementation/06-phase5-security-observability.md).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "healthcheck: not implemented (Phase M5)")
	os.Exit(1)
}
