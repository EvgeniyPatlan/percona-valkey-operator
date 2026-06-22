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

package perconavalkeyrestore

import (
	"k8s.io/apimachinery/pkg/api/meta"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// Cluster condition types the restore reads to gate Forming/Validating (kept in
// sync with pkg/controller/perconavalkeycluster status.go — duplicated as string
// literals here rather than importing the cluster controller package to avoid a
// controller→controller dependency).
const (
	condClusterReady   = "Ready"
	condSlotsAssigned  = "SlotsAssigned"
	condClusterFormed  = "ClusterFormed"
	condClusterDegrade = "Degraded"
)

// clusterFormed reports whether the target cluster has re-formed its topology — the
// cluster controller's ClusterFormed condition is True (all primaries met, slots
// assigned). The restore advances Forming→Validating on this (06 §7.5 step 1-3).
func clusterFormed(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	return cluster != nil && meta.IsStatusConditionTrue(cluster.Status.Conditions, condClusterFormed)
}

// clusterSlotsComplete reports whether the live target cluster reports all 16384
// slots assigned — the cluster controller's SlotsAssigned condition is True. This is
// the LIVE slot-coverage gate (06 §7.5 step 4: cluster_state:ok &&
// cluster_slots_assigned:16384) the restore requires before Succeeded.
func clusterSlotsComplete(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	return cluster != nil && meta.IsStatusConditionTrue(cluster.Status.Conditions, condSlotsAssigned)
}

// clusterReady reports whether the target cluster reached Ready — all shards,
// replicas and 16384 slots present with replica links up (the cluster controller's
// Ready condition / status.state). It is the strongest re-form completion signal:
// it implies SlotsAssigned and replica master_link_status up (06 §7.5 steps 4-5).
func clusterReady(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	if cluster == nil {
		return false
	}
	if cluster.Status.State == valkeyv1alpha1.StateReady {
		return true
	}
	return meta.IsStatusConditionTrue(cluster.Status.Conditions, condClusterReady)
}
