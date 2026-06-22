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

// NodeRole is the live Valkey role of a node, always read from INFO /
// CLUSTER NODES — never inferred from nodeIndex (03 §6).
// +kubebuilder:validation:Enum=primary;replica
type NodeRole string

const (
	// NodeRolePrimary is a primary node.
	NodeRolePrimary NodeRole = "primary"
	// NodeRoleReplica is a replica node.
	NodeRoleReplica NodeRole = "replica"
)

// ValkeyNodeSpec is the parent-written node spec (the directional parent↔node
// contract, 03 §6). The PerconaValkeyCluster controller WRITES every field
// here; the ValkeyNode controller only READS them. The persistence immutability
// CEL mirrors the cluster (03 §6 note).
//
// +kubebuilder:validation:XValidation:rule="!(has(self.persistence) && self.workloadType == 'Deployment')",message="persistence requires workloadType StatefulSet"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.persistence) || has(self.persistence)",message="persistence cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.persistence) || !has(self.persistence)",message="persistence cannot be added after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || quantity(self.persistence.size).compareTo(quantity(oldSelf.persistence.size)) >= 0",message="persistence.size may only be expanded"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || ((!has(self.persistence.storageClassName) && !has(oldSelf.persistence.storageClassName)) || (has(self.persistence.storageClassName) && has(oldSelf.persistence.storageClassName) && self.persistence.storageClassName == oldSelf.persistence.storageClassName))",message="persistence.storageClassName is immutable"
type ValkeyNodeSpec struct {
	// image is the resolved engine image (from cluster spec.image).
	// +optional
	Image string `json:"image,omitempty"`
	// imagePullSecrets are propagated from the cluster.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// workloadType is the per-node workload kind. Immutable (CEL self == oldSelf).
	// +kubebuilder:default=StatefulSet
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workloadType is immutable"
	// +optional
	WorkloadType WorkloadType `json:"workloadType,omitempty"`
	// persistence is propagated from the cluster (same immutability rules).
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// resources are propagated from the cluster.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// nodeSelector is propagated from the cluster.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// affinity is propagated from the cluster.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// tolerations are propagated from the cluster.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// topologySpreadConstraints are propagated from the cluster (augmented with
	// shard-aware selectors).
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// exporter is the sidecar config propagated from the cluster.
	// +optional
	Exporter ExporterSpec `json:"exporter,omitempty"`
	// containers is the strategic-merge patch propagated from the cluster.
	// +optional
	Containers []corev1.Container `json:"containers,omitempty"`
	// tls is the cert-secret reference propagated from the cluster.
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`
	// config is the verbatim copy of cluster spec.config. The node applies the
	// live-settable subset via CONFIG SET.
	// +optional
	Config map[string]string `json:"config,omitempty"`

	// env is the simple key/value env map propagated from the cluster.
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// extraEnvVars are the full corev1.EnvVar entries propagated from the cluster.
	// +optional
	ExtraEnvVars []corev1.EnvVar `json:"extraEnvVars,omitempty"`
	// serviceAccountName is propagated from the cluster.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// automountServiceAccountToken is propagated from the cluster (default false).
	// +optional
	AutomountServiceAccountToken *bool `json:"automountServiceAccountToken,omitempty"`
	// podSecurityContext is propagated from the cluster.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	// containerSecurityContext is propagated from the cluster.
	// +optional
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`

	// serverConfigMapName is the name of the rendered valkey-<cluster> ConfigMap
	// (scripts + config). Parent writes; node reads.
	// +optional
	ServerConfigMapName string `json:"serverConfigMapName,omitempty"`
	// serverConfigHash is the SHA-256 of the rendered valkey.conf excluding
	// live-settable keys. Stamped into the pod-template annotation to drive
	// rolling restart on change. Parent writes; node reads.
	// +optional
	ServerConfigHash string `json:"serverConfigHash,omitempty"`
	// aclSecretName is the name of the rendered internal-<cluster>-acl Secret
	// (type valkey.io/acl) holding users.acl. Parent writes; node reads.
	// +optional
	ACLSecretName string `json:"aclSecretName,omitempty"`
}

// ValkeyNodeStatus is the node-written status (parent reads). 03 §6.
type ValkeyNodeStatus struct {
	// observedGeneration is the last spec generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ready is true when the pod is Ready and all conditions are true. The parent
	// gates one-at-a-time progress on this.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// podName is the managed pod name.
	// +optional
	PodName string `json:"podName,omitempty"`
	// podIP is the live pod IP (read fresh; may lag on reschedule).
	// +optional
	PodIP string `json:"podIP,omitempty"`
	// role is the live Valkey role from INFO/CLUSTER NODES (never from nodeIndex).
	// +optional
	Role NodeRole `json:"role,omitempty"`
	// conditions are standard Kubernetes conditions: Ready,
	// PersistentVolumeClaimReady, PersistentVolumeClaimSizeReady, LiveConfigApplied.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Node condition types (03 §6). The cluster controller blocks one-at-a-time
// rolling progress until LiveConfigApplied=True so a bad config stalls the
// rollout visibly.
const (
	// NodeConditionReady is set True when the pod is Ready and all conditions hold.
	NodeConditionReady = "Ready"
	// NodeConditionPVCReady tracks PVC bind progress.
	NodeConditionPVCReady = "PersistentVolumeClaimReady"
	// NodeConditionPVCSizeReady tracks PVC resize/expansion progress.
	NodeConditionPVCSizeReady = "PersistentVolumeClaimSizeReady"
	// NodeConditionLiveConfigApplied is set True only after the live-settable
	// config subset is applied via CONFIG SET.
	NodeConditionLiveConfigApplied = "LiveConfigApplied"
)

// ValkeyNode is the operator's INTERNAL per-pod control point. Users MUST NOT
// create it directly — it is created and driven only by the PerconaValkeyCluster
// controller. A ValkeyNode maps 1:1 to a single Valkey pod and wraps a
// one-replica StatefulSet (durable) or Deployment (cache). See 03 §6 / ADR-001.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vkn,scope=Namespaced
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.status.role`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.podIP`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ValkeyNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ValkeyNodeSpec   `json:"spec,omitempty"`
	Status ValkeyNodeStatus `json:"status,omitempty"`
}

// ValkeyNodeList is a list of ValkeyNode.
//
// +kubebuilder:object:root=true
type ValkeyNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ValkeyNode `json:"items"`
}
