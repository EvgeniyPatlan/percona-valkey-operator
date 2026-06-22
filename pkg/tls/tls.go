// Package tls handles cert-manager Certificate issuance and secretRef cert
// consumption, plus the cert hash that triggers rolling restarts.
//
// M5 GO-5.6/5.7/5.8 (docs/architecture/07-security.md §3,
// docs/implementation/06-phase5-security-observability.md):
//
//   - hash.go      — ComputeTLSHash + ValidateSecretData (fail-closed, byte-stable).
//   - secretref.go — ValidateSecretRef / LoadSecret for bring-your-own Secrets.
//   - certificate.go — EnsureCertificate provisions a cert-manager.io/v1
//     Certificate via UNSTRUCTURED objects (no typed cert-manager Go dependency,
//     frozen M5 decision).
//   - sans.go      — DNSNames builds the deterministic SAN list (headless Service,
//     wildcard, and per-pod FQDNs).
//   - webhookcert.go — reusable webhook-cert bootstrap scaffold for M6 (cert-manager
//     pre-flight and a manager startup gate); no conversion logic ships in M5.
package tls
