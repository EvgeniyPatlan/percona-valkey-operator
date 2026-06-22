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

// RestoreState is the restore lifecycle state (03 §8.2). "" = New; terminal
// states are Succeeded/Failed/Error. The enum is additive-tolerant per 03 §10.
// +kubebuilder:validation:Enum="";Starting;Running;Succeeded;Failed;Error
type RestoreState string

const (
	// RestoreStateNew is the empty/New genesis state.
	RestoreStateNew RestoreState = ""
	// RestoreStateStarting means the restore Job is starting.
	RestoreStateStarting RestoreState = "Starting"
	// RestoreStateRunning means the restore is in progress.
	RestoreStateRunning RestoreState = "Running"
	// RestoreStateSucceeded is a terminal success state.
	RestoreStateSucceeded RestoreState = "Succeeded"
	// RestoreStateFailed is a terminal failure state.
	RestoreStateFailed RestoreState = "Failed"
	// RestoreStateError is a terminal error state.
	RestoreStateError RestoreState = "Error"
)

// RestoreStrategy selects how the restore lands the data. 03 §8.1.
// +kubebuilder:validation:Enum=NewCluster;InPlace
type RestoreStrategy string

const (
	// RestoreStrategyNewCluster bootstraps a fresh cluster from the backup set
	// (recommended, safest; slot-coverage verified).
	RestoreStrategyNewCluster RestoreStrategy = "NewCluster"
	// RestoreStrategyInPlace loads the RDB into the existing cluster (data-loss
	// risk; flushes the existing keyspace).
	RestoreStrategyInPlace RestoreStrategy = "InPlace"
)

// PerconaValkeyRestoreSpec references a backup by name XOR an inline backupSource,
// targeting a cluster. 03 §8.1.
//
// +kubebuilder:validation:XValidation:rule="has(self.backupName) != has(self.backupSource)",message="set exactly one of backupName or backupSource"
type PerconaValkeyRestoreSpec struct {
	// clusterName is the target PerconaValkeyCluster. Immutable.
	ClusterName string `json:"clusterName"`
	// backupName is a PerconaValkeyBackup in the same namespace; storage details
	// are hydrated from it. Set this XOR backupSource. Immutable.
	// +optional
	BackupName string `json:"backupName,omitempty"`
	// backupSource is an inline source (hydrated from a PerconaValkeyBackup.status)
	// for restoring from an artifact whose Backup CR no longer exists. Set this
	// XOR backupName. Immutable.
	// +optional
	BackupSource *BackupSource `json:"backupSource,omitempty"`
	// strategy selects the restore mechanism. Immutable.
	// +kubebuilder:default=NewCluster
	// +optional
	Strategy RestoreStrategy `json:"strategy,omitempty"`
}

// PerconaValkeyRestoreStatus is the restore status (03 §8.2).
type PerconaValkeyRestoreStatus struct {
	// state is the restore lifecycle state. "" = New.
	// +optional
	State RestoreState `json:"state,omitempty"`
	// stateDescription is human-readable detail.
	// +optional
	StateDescription string `json:"stateDescription,omitempty"`
	// completed is the completion timestamp.
	// +optional
	Completed *metav1.Time `json:"completed,omitempty"`
}

// PerconaValkeyRestore is a user-facing restore CR. It references a
// PerconaValkeyBackup (or inline source) and a target cluster. 03 §8.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pvk-restore,scope=Namespaced
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.spec.backupName`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PerconaValkeyRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PerconaValkeyRestoreSpec   `json:"spec,omitempty"`
	Status PerconaValkeyRestoreStatus `json:"status,omitempty"`
}

// PerconaValkeyRestoreList is a list of PerconaValkeyRestore.
//
// +kubebuilder:object:root=true
type PerconaValkeyRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PerconaValkeyRestore `json:"items"`
}
