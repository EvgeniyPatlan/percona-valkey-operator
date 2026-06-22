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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackupState is the backup lifecycle state (03 §7.2). "" = New; terminal states
// are Succeeded/Failed/Error. The enum is additive-tolerant per 03 §10.
// +kubebuilder:validation:Enum="";Starting;Running;Succeeded;Failed;Error
type BackupState string

const (
	// BackupStateNew is the empty/New genesis state.
	BackupStateNew BackupState = ""
	// BackupStateStarting means the backup Job is starting.
	BackupStateStarting BackupState = "Starting"
	// BackupStateRunning means the backup is in progress.
	BackupStateRunning BackupState = "Running"
	// BackupStateSucceeded is a terminal success state.
	BackupStateSucceeded BackupState = "Succeeded"
	// BackupStateFailed is a terminal failure state.
	BackupStateFailed BackupState = "Failed"
	// BackupStateError is a terminal error state.
	BackupStateError BackupState = "Error"
)

// SlotCoverage indicates whether the captured shards' slot ranges union to all
// 16384 slots (complete) or not (partial). Gates Succeeded and is read by restore.
// +kubebuilder:validation:Enum=complete;partial
type SlotCoverage string

const (
	// SlotCoverageComplete means all 16384 slots are represented with no gap/overlap.
	SlotCoverageComplete SlotCoverage = "complete"
	// SlotCoveragePartial means coverage is incomplete.
	SlotCoveragePartial SlotCoverage = "partial"
)

// PerconaValkeyBackupSpec mirrors the Percona Backup CRD minimalism: it carries
// only the cluster name, the storage name and the type; the rest resolves from
// the cluster / lands in status. 03 §7.1.
type PerconaValkeyBackupSpec struct {
	// clusterName is the target PerconaValkeyCluster in the same namespace.
	// Immutable.
	ClusterName string `json:"clusterName"`
	// storageName is a key into the cluster's spec.backup.storages. No fallback;
	// a typo fails at execution (validated in CheckNSetDefaults). Immutable.
	StorageName string `json:"storageName"`
	// type is the snapshot type (RDB BGSAVE per shard). Immutable.
	// +kubebuilder:default=full
	// +optional
	Type BackupScheduleType `json:"type,omitempty"`
	// consistency is the snapshot-coordination mode across shards. Immutable.
	// +kubebuilder:default=strict
	// +optional
	Consistency BackupConsistency `json:"consistency,omitempty"`
	// startingDeadlineSeconds is the scheduled-backup missed-window tolerance:
	// if the Job cannot start within this window the backup is marked Failed.
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`
	// activeDeadlineSeconds is a hard cap on backup Job runtime before failing.
	// +kubebuilder:default=3600
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// retention is the count/age GC policy for this backup-set.
	// +optional
	Retention *BackupRetentionSpec `json:"retention,omitempty"`
	// containerOptions are extra args/env and tuning knobs for the backup tool.
	// Immutable.
	// +optional
	ContainerOptions *BackupContainerOptions `json:"containerOptions,omitempty"`
}

// PerconaValkeyBackupStatus is the backup status (03 §7.2).
type PerconaValkeyBackupStatus struct {
	// state is the backup lifecycle state. "" = New.
	// +optional
	State BackupState `json:"state,omitempty"`
	// stateDescription is human-readable detail.
	// +optional
	StateDescription string `json:"stateDescription,omitempty"`
	// destination is the backend-prefixed root (s3://bucket/prefix/<backup>, ...).
	// +optional
	Destination string `json:"destination,omitempty"`
	// storageName is echoed from spec (status hydration for restore).
	// +optional
	StorageName string `json:"storageName,omitempty"`
	// s3 holds resolved S3 storage details copied from the cluster at execution
	// time (so restore is self-contained).
	// +optional
	S3 *BackupStorageS3Spec `json:"s3,omitempty"`
	// gcs holds resolved GCS storage details copied from the cluster.
	// +optional
	GCS *BackupStorageGCSSpec `json:"gcs,omitempty"`
	// azure holds resolved Azure storage details copied from the cluster.
	// +optional
	Azure *BackupStorageAzureSpec `json:"azure,omitempty"`
	// shards is per-shard slot-coverage metadata so restore can verify all 16384
	// slots are represented.
	// +optional
	Shards []ShardBackupStatus `json:"shards,omitempty"`
	// slotCoverage indicates whether the captured shards cover all slots.
	// +optional
	SlotCoverage SlotCoverage `json:"slotCoverage,omitempty"`
	// start is the start timestamp.
	// +optional
	Start *metav1.Time `json:"start,omitempty"`
	// completed is the completion timestamp.
	// +optional
	Completed *metav1.Time `json:"completed,omitempty"`
	// valkeyVersion is the engine version at backup time (restore compatibility).
	// +optional
	ValkeyVersion string `json:"valkeyVersion,omitempty"`
}

// PerconaValkeyBackup is a user-facing on-demand backup CR. It references a
// PerconaValkeyCluster by name and is NOT owned by the cluster — the artifact
// outlives cluster deletion. 03 §7 / ADR-004.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pvk-backup,scope=Namespaced
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Storage",type=string,JSONPath=`.spec.storageName`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Coverage",type=string,JSONPath=`.status.slotCoverage`
// +kubebuilder:printcolumn:name="Destination",type=string,JSONPath=`.status.destination`,priority=1
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PerconaValkeyBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PerconaValkeyBackupSpec   `json:"spec,omitempty"`
	Status PerconaValkeyBackupStatus `json:"status,omitempty"`
}

// PerconaValkeyBackupList is a list of PerconaValkeyBackup.
//
// +kubebuilder:object:root=true
type PerconaValkeyBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PerconaValkeyBackup `json:"items"`
}
