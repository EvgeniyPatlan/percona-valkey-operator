/*
Copyright Percona LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterMode is the Valkey topology mode (03 §2.3). Immutable once set.
// +kubebuilder:validation:Enum=cluster;replication;standalone
type ClusterMode string

const (
	// ModeCluster is sharded 16384-slot mode (the primary v1alpha1 target).
	ModeCluster ClusterMode = "cluster"
	// ModeReplication is 1 primary + N replicas with operator-driven failover
	// (no Sentinel).
	ModeReplication ClusterMode = "replication"
	// ModeStandalone is a single node (future).
	ModeStandalone ClusterMode = "standalone"
)

// WorkloadType is the per-node workload kind (03 §2.3). Immutable once set.
// +kubebuilder:validation:Enum=StatefulSet;Deployment
type WorkloadType string

const (
	// WorkloadStatefulSet is durable (required with persistence).
	WorkloadStatefulSet WorkloadType = "StatefulSet"
	// WorkloadDeployment is cache (no PVCs).
	WorkloadDeployment WorkloadType = "Deployment"
)

// PDBPolicy controls the PodDisruptionBudget (03 §2.10). The enum is additive-
// tolerant per 03 §10 (clients must tolerate future values).
// +kubebuilder:validation:Enum=Managed;Disabled
type PDBPolicy string

const (
	// PDBManaged tells the operator to create a quorum-sized PDB per shard.
	PDBManaged PDBPolicy = "Managed"
	// PDBDisabled suppresses operator PDB management.
	PDBDisabled PDBPolicy = "Disabled"
)

// ClusterState is the high-level cluster summary derived from conditions (03 §3).
// The enum is additive-tolerant per 03 §10.
// +kubebuilder:validation:Enum=Initializing;Reconciling;Ready;Degraded;Failed
type ClusterState string

const (
	// StateInitializing is the genesis state.
	StateInitializing ClusterState = "Initializing"
	// StateReconciling means a create/update/scale is in flight.
	StateReconciling ClusterState = "Reconciling"
	// StateReady means the cluster is healthy and serving traffic.
	StateReady ClusterState = "Ready"
	// StateDegraded means the cluster is impaired but possibly partially functional.
	StateDegraded ClusterState = "Degraded"
	// StateFailed means the cluster is failed.
	StateFailed ClusterState = "Failed"
)

// PerconaValkeyClusterSpec is the top-level cluster spec. See 03 §2 for the
// authoritative field catalogue.
//
// CEL immutability + has()-guard rules (03 §4.1):
// +kubebuilder:validation:XValidation:rule="!(has(self.persistence) && self.workloadType == 'Deployment')",message="persistence requires workloadType StatefulSet"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.persistence) || has(self.persistence)",message="persistence cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.persistence) || !has(self.persistence)",message="persistence cannot be added after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || quantity(self.persistence.size).compareTo(quantity(oldSelf.persistence.size)) >= 0",message="persistence.size may only be expanded"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || ((!has(self.persistence.storageClassName) && !has(oldSelf.persistence.storageClassName)) || (has(self.persistence.storageClassName) && has(oldSelf.persistence.storageClassName) && self.persistence.storageClassName == oldSelf.persistence.storageClassName))",message="persistence.storageClassName is immutable"
// +kubebuilder:validation:XValidation:rule="self.mode == oldSelf.mode",message="mode is immutable"
// +kubebuilder:validation:XValidation:rule="self.mode == 'cluster' || !has(self.shards) || self.shards == 1",message="shards must be 1 unless mode is cluster"
type PerconaValkeyClusterSpec struct {
	// crVersion is the operator API contract version. Auto-stamped to the
	// operator major.minor on first reconcile if empty (PSMDB/PS convention).
	// Gated via CompareVersion — NOT CEL-immutable (intentionally allows upgrades,
	// 03 §4.3 / ADR-005).
	// +optional
	CrVersion string `json:"crVersion,omitempty"`
	// image is the Valkey server image. Independent of crVersion. Defaulted in
	// CheckNSetDefaults; mutated by the version service when upgradeOptions.apply
	// != Disabled.
	// +optional
	Image string `json:"image,omitempty"`
	// imagePullSecrets are secrets for pulling images from private registries.
	// Propagated to every ValkeyNode.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// pause scales workloads to zero and stops topology reconciliation without
	// deleting the CR or PVCs.
	// +kubebuilder:default=false
	// +optional
	Pause bool `json:"pause,omitempty"`

	// mode is the topology mode. Immutable (CEL self == oldSelf): changing
	// topology requires a full rebuild.
	// +kubebuilder:default=cluster
	// +optional
	Mode ClusterMode `json:"mode,omitempty"`
	// shards is the number of shard groups. Defaulted in CheckNSetDefaults to 3
	// (cluster) / 1 (other). Non-cluster modes require shards==1 (CEL). Scalable.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Shards int32 `json:"shards,omitempty"`
	// replicas is the replica count per shard. Total Valkey pods =
	// shards * (1 + replicas). Scalable.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// workloadType is the per-node workload kind. Immutable (CEL self == oldSelf).
	// StatefulSet is required with persistence; Deployment is cache.
	// +kubebuilder:default=StatefulSet
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workloadType is immutable"
	// +optional
	WorkloadType WorkloadType `json:"workloadType,omitempty"`

	// resources are requests/limits for the Valkey container. Propagated to every
	// ValkeyNode.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// affinity is pod affinity/anti-affinity. Overrides nodeSelector when set.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// nodeSelector constrains node selection.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// tolerations are pod tolerations.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// topologySpreadConstraints are spread constraints (augmented with shard-aware
	// selectors by the operator).
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// persistence is durable storage propagated to each ValkeyNode. Pointer so
	// absence is meaningful (cache mode). Add/remove forbidden; size expand-only;
	// storageClassName immutable; forbidden with workloadType=Deployment.
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// config holds additional valkey.conf parameters. Operator-managed base config
	// wins on conflict. The live-settable subset is applied via CONFIG SET.
	// +optional
	Config map[string]string `json:"config,omitempty"`
	// containers is a strategic-merge patch of the default containers
	// (server, metrics-exporter); unknown names are appended.
	// +optional
	Containers []corev1.Container `json:"containers,omitempty"`

	// users is the ACL user list, keyed by name. Mirrors the upstream UserACLSpec.
	// +listType=map
	// +listMapKey=name
	// +optional
	Users []UserACLSpec `json:"users,omitempty"`
	// auth configures the default-user password (requirepass). Distinct from
	// users[] (named ACL users); this is the chart's primary auth knob. When
	// enabled (the default) the operator sets the default user's password from
	// auth.passwordSecret. nil => defaulted (enabled, <cluster>-users secret).
	// +optional
	Auth *AuthSpec `json:"auth,omitempty"`
	// tls is the TLS-in-transit configuration. nil => TLS off.
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`
	// disableCommands lists Valkey commands the operator renders as
	// rename-command <CMD> "" so they are unavailable (e.g. FLUSHALL, FLUSHDB).
	// Defaults to [FLUSHALL, FLUSHDB] (the chart's safe default) when nil.
	// +optional
	DisableCommands []string `json:"disableCommands,omitempty"`
	// expose controls external client access (Service type, source ranges,
	// annotations, per-pod cluster access). nil => in-cluster headless Service only.
	// +optional
	Expose *ExposeSpec `json:"expose,omitempty"`
	// networkPolicy toggles and customizes the operator-managed default-deny
	// perimeter (07 §7). nil => no policy (opt-in; recommended true in production).
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`
	// env is a map of simple key/value environment variables added to the Valkey
	// server container (the common "just add an env var" escape hatch).
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// extraEnvVars are full corev1.EnvVar entries (valueFrom etc.) added to the
	// Valkey server container, layered after env.
	// +optional
	ExtraEnvVars []corev1.EnvVar `json:"extraEnvVars,omitempty"`
	// serviceAccountName is the ServiceAccount for the data pods. Empty => the
	// operator-created default SA.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// automountServiceAccountToken controls SA token automount on the data pods.
	// Defaults to false (hardened; the chart's default).
	// +kubebuilder:default=false
	// +optional
	AutomountServiceAccountToken *bool `json:"automountServiceAccountToken,omitempty"`
	// podSecurityContext is the pod-level security context for the data pods. The
	// operator applies a hardened default; this overrides it.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	// containerSecurityContext is the container-level security context for the
	// Valkey server container. Overrides the operator's hardened default.
	// +optional
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`
	// exporter configures the Prometheus exporter sidecar.
	// +optional
	Exporter ExporterSpec `json:"exporter,omitempty"`
	// podDisruptionBudget controls the operator-managed PDB.
	// +kubebuilder:default=Managed
	// +optional
	PodDisruptionBudget PDBPolicy `json:"podDisruptionBudget,omitempty"`
	// backup holds storage definitions and schedules. Individual backups are
	// separate PerconaValkeyBackup CRs referencing a storage by name.
	// +optional
	Backup BackupSpec `json:"backup,omitempty"`
	// upgradeOptions configures Percona smart updates.
	// +optional
	UpgradeOptions UpgradeOptions `json:"upgradeOptions,omitempty"`
}

// PerconaValkeyClusterStatus is the cluster status (03 §3). state is derived
// from conditions.
type PerconaValkeyClusterStatus struct {
	// state is the high-level summary derived from conditions.
	// +kubebuilder:default=Initializing
	// +optional
	State ClusterState `json:"state,omitempty"`
	// reason is a machine-readable reason for the current state.
	// +optional
	Reason string `json:"reason,omitempty"`
	// message is human-readable detail.
	// +optional
	Message string `json:"message,omitempty"`
	// host is the client connection endpoint (headless Service DNS). In cluster
	// mode this is a seed/bootstrap endpoint.
	// +optional
	Host string `json:"host,omitempty"`
	// shards is the number of shards currently formed.
	// +optional
	Shards int32 `json:"shards,omitempty"`
	// readyShards is the number of shards fully healthy.
	// +optional
	ReadyShards int32 `json:"readyShards,omitempty"`
	// observedGeneration is the last metadata.generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// conditions are standard Kubernetes conditions (keyed by type).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PerconaValkeyCluster is the top-level user-facing cluster CR. It owns all
// ValkeyNodes plus the headless Service, ConfigMap, Secrets and PDB.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pvk,scope=Namespaced
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Shards",type=integer,JSONPath=`.status.shards`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyShards`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.status.host`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PerconaValkeyCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PerconaValkeyClusterSpec   `json:"spec,omitempty"`
	Status PerconaValkeyClusterStatus `json:"status,omitempty"`
}

// PerconaValkeyClusterList is a list of PerconaValkeyCluster.
//
// +kubebuilder:object:root=true
type PerconaValkeyClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PerconaValkeyCluster `json:"items"`
}
