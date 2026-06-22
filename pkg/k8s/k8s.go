// Package k8s holds Kubernetes-client helpers shared across controllers:
// CreateOrUpdate, fresh-read status writeback, and Lease-based serialisation.
//
// Empty seam in M0 — helpers are added as controllers need them (M2+). See
// docs/architecture/02-repo-layout.md §3.
package k8s
