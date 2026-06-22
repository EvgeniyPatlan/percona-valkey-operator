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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// verifyAndMarkReady confirms the four bootstrap-completion checks (04 §2.1
// step15 / 05 §3 step7): shard count == spec.shards, each shard has
// 1+spec.replicas nodes, all 16384 slots assigned, and every replica's link is
// up. On success it sets Ready/ClusterFormed/SlotsAssigned=True, clears
// Progressing, sets status.{shards,readyShards,observedGeneration} and returns
// ready=true (caller requeues steady). On any failure it sets the specific
// False reason and returns ready=false (caller requeues fast).
func (r *Reconciler) verifyAndMarkReady(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
) (bool, error) {
	wantShards := int(cluster.Spec.Shards)
	wantPerShard := 1 + int(cluster.Spec.Replicas)

	shards := slotOwningShards(state)
	cluster.Status.Shards = int32(len(shards))
	cluster.Status.ReadyShards = countReadyShards(state, wantPerShard)

	slotsCovered := len(state.GetUnassignedSlots()) == 0
	setCondition(cluster, CondSlotsAssigned, boolToStatus(slotsCovered), slotsReason(slotsCovered), slotsMessage(slotsCovered))

	formed := len(shards) >= wantShards && slotsCovered
	if formed {
		setCondition(cluster, CondClusterFormed, metav1.ConditionTrue, ReasonClusterHealthy, "all shards formed and slots assigned")
	}

	switch {
	case len(shards) < wantShards:
		return r.notReady(ctx, cluster, ReasonMissingShards, "fewer shards than desired")
	case !slotsCovered:
		return r.notReady(ctx, cluster, ReasonSlotsUnassigned, "not all 16384 slots assigned")
	case int(cluster.Status.ReadyShards) < wantShards:
		return r.notReady(ctx, cluster, ReasonMissingReplicas, "a shard is missing its replicas")
	case !state.IsReplicationInSync():
		return r.notReady(ctx, cluster, ReasonReplicationNotInSync, "a replica's master_link_status is not up")
	}

	// All checks pass — mark Ready.
	setCondition(cluster, CondReady, metav1.ConditionTrue, ReasonClusterHealthy,
		"cluster healthy: all shards, replicas and 16384 slots present")
	setCondition(cluster, CondProgressing, metav1.ConditionFalse, ReasonClusterHealthy, "converged")
	setCondition(cluster, CondDegraded, metav1.ConditionFalse, ReasonClusterHealthy, "healthy")
	cluster.Status.ObservedGeneration = cluster.Generation
	// Mirror the accepted contract at the successful reconcile tail (GO-6.3): this
	// is the runtime anchor the next pass compares against to reject a crVersion
	// downgrade. Written only once the cluster has converged on this crVersion so a
	// rejected/in-flight contract never advances the monotonic floor (09 §7).
	cluster.Status.LastObservedCrVersion = cluster.Spec.CrVersion
	if err := r.writeStatus(ctx, cluster); err != nil {
		return false, err
	}
	r.recorder.Eventf(cluster, nil, eventNormal, EventClusterReady, "ClusterReady",
		"Cluster is healthy: %d shard(s), 16384 slots assigned", len(shards))
	return true, nil
}

// notReady sets Ready=False with the reason + Progressing=True and writes
// status, returning ready=false so the caller requeues fast.
func (r *Reconciler) notReady(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, reason, msg string) (bool, error) {
	setCondition(cluster, CondReady, metav1.ConditionFalse, reason, msg)
	setCondition(cluster, CondProgressing, metav1.ConditionTrue, progressingReason(cluster), msg)
	if err := r.writeStatus(ctx, cluster); err != nil {
		return false, err
	}
	return false, nil
}

// slotOwningShards returns the shards that own at least one slot (a genuine
// formed shard, excluding empty/pending primaries).
func slotOwningShards(state *valkey.ClusterState) []*valkey.ShardState {
	var out []*valkey.ShardState
	for _, shard := range state.Shards {
		if shard.NumSlots() > 0 {
			out = append(out, shard)
		}
	}
	return out
}

// countReadyShards counts the slot-owning shards that have the full
// 1+replicas node count with every replica link up.
func countReadyShards(state *valkey.ClusterState, wantPerShard int) int32 {
	var ready int32
	for _, shard := range state.Shards {
		if shard.NumSlots() == 0 {
			continue
		}
		if len(shard.Nodes) < wantPerShard {
			continue
		}
		linksUp := true
		for _, n := range shard.Nodes {
			if n.IsReplica() && !n.LinkUp {
				linksUp = false
				break
			}
		}
		if linksUp {
			ready++
		}
	}
	return ready
}

// boolToStatus maps a bool to a metav1.ConditionStatus.
func boolToStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func slotsReason(covered bool) string {
	if covered {
		return ReasonClusterHealthy
	}
	return ReasonSlotsUnassigned
}

func slotsMessage(covered bool) string {
	if covered {
		return "all 16384 slots assigned"
	}
	return "slots not fully assigned"
}
