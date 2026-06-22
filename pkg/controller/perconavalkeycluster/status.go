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

package perconavalkeycluster

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
)

// Event type aliases (k8s Event Normal/Warning) used with the EventRecorder.
const (
	eventNormal  = corev1.EventTypeNormal
	eventWarning = corev1.EventTypeWarning
)

// Cluster condition types (04 §7 / 03 §3). Ready/Progressing/Degraded drive the
// derived status.state; ClusterFormed/SlotsAssigned are informational gates.
const (
	// CondReady is True when the cluster is healthy and serving (all shards,
	// replicas and 16384 slots present, links up).
	CondReady = "Ready"
	// CondProgressing is True while a create/update/scale/roll is in flight.
	CondProgressing = "Progressing"
	// CondDegraded is True when the cluster is impaired (primary lost, slots
	// uncovered, link down).
	CondDegraded = "Degraded"
	// CondClusterFormed is True once the cluster has ever formed (all nodes met
	// and the topology established).
	CondClusterFormed = "ClusterFormed"
	// CondSlotsAssigned is True once all 16384 slots are assigned.
	CondSlotsAssigned = "SlotsAssigned"
)

// Cluster condition reasons (04 §7). String values; the Go constants are
// Reason-prefixed to match the Ready=False/Reason... references in 04 §2.
const (
	ReasonClusterHealthy           = "ClusterHealthy"
	ReasonReconciling              = "Reconciling"
	ReasonInitializing             = "Initializing"
	ReasonUpdatingNodes            = "UpdatingNodes"
	ReasonAddingNodes              = "AddingNodes"
	ReasonMissingShards            = "MissingShards"
	ReasonMissingReplicas          = "MissingReplicas"
	ReasonSlotsUnassigned          = "SlotsUnassigned"
	ReasonRebalancingSlots         = "RebalancingSlots"
	ReasonReplicationNotInSync     = "ReplicationNotInSync"
	ReasonServiceError             = "ServiceError"
	ReasonConfigMapError           = "ConfigMapError"
	ReasonUsersACLError            = "UsersAclError"
	ReasonPodDisruptionBudgetError = "PodDisruptionBudgetError"
	ReasonValkeyNodeListError      = "ValkeyNodeListError"
	ReasonSystemUsersACLError      = "SystemUsersAclError"
	ReasonUnsupportedCRVersion     = "UnsupportedCRVersion"
	// ReasonCrVersionTooOld halts the reconcile when spec.crVersion is below the
	// operator's accepted floor (more than one released minor behind), per the
	// 09 §8 compatibility matrix — the user must step crVersion up one minor at a
	// time rather than jump (09 §7 unsupported-jump handling).
	ReasonCrVersionTooOld = "CrVersionTooOld"
	// ReasonCrVersionDowngradeRejected halts the reconcile when spec.crVersion is
	// lowered below status.lastObservedCrVersion. crVersion is monotonic within a
	// cluster's life (09 §7): the prior contract is kept in force and the decrease
	// refused at runtime (crVersion is deliberately not hard-immutable, 03 §4.3).
	ReasonCrVersionDowngradeRejected = "CrVersionDowngradeRejected"
	ReasonClusterMeet                = "ClusterMeet"
	ReasonScrapeError                = "ScrapeError"
	ReasonPaused                     = "Paused"
	// Wave 2b reasons (scale / roll / recovery, 04 §7).
	ReasonRebalanceFailed   = "RebalanceFailed"
	ReasonDrainFailed       = "DrainFailed"
	ReasonNodeAddFailed     = "NodeAddFailed"
	ReasonFailoverPending   = "FailoverPending"
	ReasonQuorumLost        = "QuorumLost"
	ReasonPrimaryLost       = "PrimaryLost"
	ReasonNodeForgetFailed  = "NodeForgetFailed"
	ReasonReplicasTakenOver = "ReplicasTakenOver"
)

// Cluster event reasons (04 §7 vocabulary). Normal unless noted.
const (
	EventServiceCreated             = "ServiceCreated"
	EventConfigMapCreated           = "ConfigMapCreated"
	EventPodDisruptionBudgetCreated = "PodDisruptionBudgetCreated"
	EventUsersACLCreated            = "UsersAclCreated"
	EventValkeyNodeCreated          = "ValkeyNodeCreated"
	EventValkeyNodeRolled           = "ValkeyNodeRolled"
	EventClusterMeetBatch           = "ClusterMeetBatch"
	EventPrimariesCreated           = "PrimariesCreated"
	EventReplicasAttached           = "ReplicasAttached"
	EventClusterReady               = "ClusterReady"
	EventSlotsRebalancing           = "SlotsRebalancing"
	EventSlotsRebalancePending      = "SlotsRebalancePending"
	EventSlotRebalanceFailed        = "SlotRebalanceFailed"
	EventSlotsDraining              = "SlotsDraining"
	EventDrainFailed                = "DrainFailed"
	EventValkeyNodeDeleted          = "ValkeyNodeDeleted"
	EventFailoverInitiated          = "FailoverInitiated"
	EventFailoverCompleted          = "FailoverCompleted"
	EventFailoverTimeout            = "FailoverTimeout"
	EventFailoverFailed             = "FailoverFailed"
	EventFailoverDeferred           = "FailoverDeferred"
	EventReplicasTakenOver          = "ReplicasTakenOver"
	EventStaleNodeForgotten         = "StaleNodeForgotten"
	EventNodeForgetFailed           = "NodeForgetFailed"
)

// setCondition sets a condition on the cluster with the cluster's generation as
// ObservedGeneration (so a stale condition is detectable). It is the single
// mutation point for the conditions array.
func setCondition(cluster *valkeyv1alpha1.PerconaValkeyCluster, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cluster.Generation,
	})
}

// conditionTrue reports whether the named condition is currently True.
func conditionTrue(cluster *valkeyv1alpha1.PerconaValkeyCluster, condType string) bool {
	return meta.IsStatusConditionTrue(cluster.Status.Conditions, condType)
}

// deriveState computes status.state as a PURE projection of the
// Degraded/Ready/Progressing conditions by the 04 §7 priority
// (deletionTimestamp -> Degraded -> Ready -> Progressing -> else Failed). It is
// never written directly anywhere else; this is the single source of truth so
// state can never disagree with the conditions array.
func deriveState(cluster *valkeyv1alpha1.PerconaValkeyCluster) valkeyv1alpha1.ClusterState {
	switch {
	case !cluster.DeletionTimestamp.IsZero():
		// Terminating is not in the v1alpha1 enum (the printcolumn surfaces the
		// reason); fall through to Degraded/Reconciling so the enum stays valid.
		return valkeyv1alpha1.StateReconciling
	case conditionTrue(cluster, CondDegraded):
		return valkeyv1alpha1.StateDegraded
	case conditionTrue(cluster, CondReady):
		return valkeyv1alpha1.StateReady
	case conditionTrue(cluster, CondProgressing):
		if !conditionTrue(cluster, CondClusterFormed) {
			return valkeyv1alpha1.StateInitializing
		}
		return valkeyv1alpha1.StateReconciling
	default:
		return valkeyv1alpha1.StateFailed
	}
}

// applyDerivedState recomputes status.state from the conditions and mirrors the
// Ready condition's reason/message onto status.{reason,message} for the printer
// columns. Always called before a status write so the projection holds.
func applyDerivedState(cluster *valkeyv1alpha1.PerconaValkeyCluster) {
	cluster.Status.State = deriveState(cluster)
	if c := meta.FindStatusCondition(cluster.Status.Conditions, CondReady); c != nil {
		cluster.Status.Reason = c.Reason
		cluster.Status.Message = c.Message
	}
}

// writeStatus recomputes the derived state then persists the status subresource
// via the shared re-fetch+patch helper (04 §9 re-fetch-before-update).
func (r *Reconciler) writeStatus(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	applyDerivedState(cluster)
	return k8s.WriteStatus(ctx, r.Client, cluster, func(c *valkeyv1alpha1.PerconaValkeyCluster) *valkeyv1alpha1.PerconaValkeyClusterStatus {
		return &c.Status
	})
}
