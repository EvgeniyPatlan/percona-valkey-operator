// Package valkey is the operator's domain/protocol engine: a valkey-go client
// wrapper plus cluster-state inspection (GetClusterState), rebalance/migration
// planning, config rendering with the roll hash, and ACL rendering.
//
// EMPTY in M0 — the engine is ported from the upstream internal/valkey starting
// in M3 (see docs/implementation/04-phase3-valkeycluster.md). This package is
// treated as operator-internal and does not commit to API stability. See
// docs/architecture/02-repo-layout.md §1, §3.
package valkey
