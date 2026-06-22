// Package v1alpha1 contains the public API types for the valkey.percona.com
// API group, version v1alpha1.
//
// M1 declares the four CRD Go types (PerconaValkeyCluster, ValkeyNode,
// PerconaValkeyBackup, PerconaValkeyRestore) and registers them into the scheme
// via SchemeBuilder.Register below (see docs/implementation/02-phase1-api.md).
// The SchemeBuilder / AddToScheme seam was wired in M0 so cmd/manager registers
// the group from day one; no reconcilers are registered here (M2+).
//
// Dependency rule (docs/architecture/02-repo-layout.md §3): this package is a
// LEAF. It imports only k8s apimachinery/apis — never pkg/controller,
// pkg/valkey, pkg/naming or pkg/platform. The one permitted exception is
// pkg/version (a near-leaf, stdlib+http only), imported by *_defaults.go.
//
// +kubebuilder:object:generate=true
// +groupName=valkey.percona.com
// +versionName=v1alpha1
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const (
	// GroupName is the API group for all Valkey custom resources.
	GroupName = "valkey.percona.com"
	// Version is the API version of this package.
	Version = "v1alpha1"
)

// SchemeGroupVersion is the group-version used to register these objects.
// +k8s:deepcopy-gen=false
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// GroupVersion is an alias for SchemeGroupVersion kept for kubebuilder-marker
// compatibility (markers and generated code reference GroupVersion).
var GroupVersion = SchemeGroupVersion

var (
	// SchemeBuilder collects the functions that add the types in this group to
	// a runtime.Scheme. M1 registers concrete kinds via SchemeBuilder.Register.
	SchemeBuilder = &scheme.Builder{GroupVersion: SchemeGroupVersion}

	// AddToScheme adds all registered types of this group-version to a Scheme.
	// Empty (no kinds) in M0; callable and non-nil so cmd/manager can wire it.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Resource takes an unqualified resource and returns a Group-qualified
// GroupResource. Useful for API machinery error construction in later phases.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

// init registers the four M1 kinds (and their List types) with the group's
// SchemeBuilder so cmd/manager's AddToScheme serves them. No reconcilers are
// registered here — controllers arrive in M2+.
func init() {
	SchemeBuilder.Register(
		&PerconaValkeyCluster{}, &PerconaValkeyClusterList{},
		&ValkeyNode{}, &ValkeyNodeList{},
		&PerconaValkeyBackup{}, &PerconaValkeyBackupList{},
		&PerconaValkeyRestore{}, &PerconaValkeyRestoreList{},
	)
}
