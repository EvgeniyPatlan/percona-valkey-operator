// Package naming centralises all label/annotation/finalizer key constants and
// (from M1) resource-name builders. No string literal for a Valkey label,
// finalizer, or resource name should appear anywhere else in the codebase.
//
// In M0 only the key constants and a Labels() helper exist; the name builders
// (valkey-<cluster>-<shard>-<node>, PVC valkey-<node>-data, ...) need the CRD
// types and land in M1. Keeping this package type-free until then preserves the
// pkg/apis leaf rule (doc 02 §3, §9).
package naming

const (
	// labelPrefix is the operator's own label/annotation domain.
	labelPrefix = "valkey.percona.com/"

	// Recommended Kubernetes app labels (app.kubernetes.io/*).

	// LabelAppName is the app.kubernetes.io/name recommended label key.
	LabelAppName = "app.kubernetes.io/name"
	// LabelAppInstance is the app.kubernetes.io/instance recommended label key.
	LabelAppInstance = "app.kubernetes.io/instance"
	// LabelAppComponent is the app.kubernetes.io/component recommended label key.
	LabelAppComponent = "app.kubernetes.io/component"
	// LabelAppManagedBy is the app.kubernetes.io/managed-by recommended label key.
	LabelAppManagedBy = "app.kubernetes.io/managed-by"
	// LabelAppPartOf is the app.kubernetes.io/part-of recommended label key.
	LabelAppPartOf = "app.kubernetes.io/part-of"

	// Operator-specific labels (valkey.percona.com/*).

	// LabelCluster is the valkey.percona.com/cluster label key.
	LabelCluster = labelPrefix + "cluster"
	// LabelShardIndex is the valkey.percona.com/shard-index label key.
	LabelShardIndex = labelPrefix + "shard-index"
	// LabelNodeIndex is the valkey.percona.com/node-index label key.
	LabelNodeIndex = labelPrefix + "node-index"
	// LabelComponent is the valkey.percona.com/component label key.
	LabelComponent = labelPrefix + "component"

	// ManagedByValue is the value stamped on app.kubernetes.io/managed-by.
	ManagedByValue = "percona-valkey-operator"
	// AppNameValue is the value stamped on app.kubernetes.io/name.
	AppNameValue = "percona-valkey"
)

// Finalizer keys (valkey.percona.com/ prefix). See doc 04 §6.
const (
	FinalizerDeletePodsInOrder = labelPrefix + "delete-pods-in-order"
	FinalizerDeleteSSL         = labelPrefix + "delete-ssl"
	FinalizerDeleteBackup      = labelPrefix + "delete-backup"
	FinalizerPVCCleanup        = labelPrefix + "persistent-volume-cleanup"
)

// Labels returns the base recommended-label set for resources belonging to the
// named cluster, with the supplied component. The returned map is a fresh copy
// (no shared backing) so callers may safely add to it without mutating shared
// state. All values are intended to be DNS-safe; callers must ensure cluster
// and component are valid label values (M1 builders enforce the 63-char limit).
func Labels(cluster, component string) map[string]string {
	l := map[string]string{
		LabelAppName:      AppNameValue,
		LabelAppInstance:  cluster,
		LabelAppManagedBy: ManagedByValue,
		LabelCluster:      cluster,
	}
	if component != "" {
		l[LabelAppComponent] = component
		l[LabelComponent] = component
	}
	return l
}
