// Package platform detects the underlying Kubernetes distribution so defaults
// can adapt (e.g. OpenShift SecurityContextConstraints vs vanilla
// PodSecurityStandards). The result feeds CheckNSetDefaults in M1+.
//
// In M0 Detect() always returns Vanilla; real OpenShift detection (probing the
// config.openshift.io API group) lands in M5 (see
// docs/implementation/06-phase5-security-observability.md).
package platform

// Platform identifies the Kubernetes distribution flavour.
type Platform string

const (
	// Vanilla is upstream Kubernetes (the M0 default).
	Vanilla Platform = "vanilla"
	// OpenShift is Red Hat OpenShift. Detection is stubbed until M5.
	OpenShift Platform = "openshift"
)

// String returns the platform name.
func (p Platform) String() string { return string(p) }

// Detect returns the detected platform. In M0 it always returns Vanilla;
// OpenShift detection is implemented in M5.
func Detect() Platform {
	return Vanilla
}
