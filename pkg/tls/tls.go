// Package tls handles cert-manager Certificate issuance and secretRef cert
// consumption, plus the cert hash that triggers rolling restarts.
//
// Empty seam in M0 — implemented in M5 (see
// docs/implementation/06-phase5-security-observability.md).
package tls
