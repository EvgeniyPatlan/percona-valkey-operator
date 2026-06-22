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
	"fmt"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// promoteOrphanedReplicas promotes orphaned replicas of failed primaries via
// CLUSTER FAILOVER TAKEOVER, but ONLY when quorum is lost AND persistence is off
// (04 §2.1 step8 / 05 §7, GO-3.17). This is the last-resort path (CR-5):
//   - with quorum intact, Valkey's native election (or a FORCE) handles it — the
//     operator only observes here;
//   - with persistence on, the restarted pod returns with the SAME node ID and
//     reclaims its slots, so a unilateral takeover would risk a split.
//
// TAKEOVER is issued BEFORE any CLUSTER FORGET (forgetStaleNodes runs in the next
// phase) so slots stay continuously owned (no coverage gap). For a failed primary
// with no replica at all, the shard is a genuine outage — surfaced as Degraded by
// the caller, never fabricating data. Emits ReplicasTakenOver / FailoverInitiated.
func (r *Reconciler) promoteOrphanedReplicas(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
) (bool, error) {
	// Persistence on => same-node-ID pod returns; never takeover (05 §7, §9).
	if cluster.Spec.Persistence != nil {
		return false, nil
	}
	// Quorum intact => native election / FORCE handles it; takeover is reserved
	// for quorum loss where a normal vote cannot complete (CR-5 safety rule).
	if state.HasFailoverQuorum() {
		return false, nil
	}

	failed := state.FailedPrimaries()
	if len(failed) == 0 {
		return false, nil
	}

	acted := false
	for _, primary := range failed {
		rep := state.BestReplicaOf(primary.ID)
		if rep == nil || rep.Client() == nil {
			// Total replica loss for this shard — genuine outage, manual recovery.
			continue
		}
		// FailoverDecision encodes the FORCE-vs-TAKEOVER rule; with quorum lost it
		// yields TAKEOVER (the only promotion that succeeds without a quorum vote).
		mode := valkey.FailoverDecision(state.HasFailoverQuorum())
		if err := rep.Client().ClusterFailover(ctx, mode); err != nil {
			return acted, fmt.Errorf("CLUSTER FAILOVER %s on %s: %w", string(mode), rep.ID, err)
		}
		r.recorder.Eventf(cluster, nil, eventNormal, EventReplicasTakenOver, "Recovery",
			"promoted replica %s of failed primary %s via %s", rep.ID, primary.ID, failoverModeLabel(mode))
		r.recorder.Eventf(cluster, nil, eventNormal, EventFailoverInitiated, "Recovery",
			"failover initiated for failed primary %s", primary.ID)
		acted = true
	}
	return acted, nil
}

// forgetStaleNodes CLUSTER FORGETs node IDs present in gossip but with no backing
// ValkeyNode (deleted during scale-in, or a permanently dead pod), broadcasting
// the FORGET to every surviving node so the ban window is cluster-wide consistent
// (04 §2.1 step9 / 05 §7, GO-3.17). It is suppressed while a failover is pending
// for a node (HasReplicaOf, handled inside StaleNodeIDs) and when persistence is
// on or quorum is intact for a *failed* node (the pod will return / native
// election handles it). An "unknown node" reply is treated as success by the
// client. Emits StaleNodeForgotten / NodeForgetFailed.
func (r *Reconciler) forgetStaleNodes(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, error) {
	log := logf.FromContext(ctx)

	backing := backingNodeIDs(state, nodes)
	stale := state.StaleNodeIDs(backing)
	if len(stale) == 0 {
		return false, nil
	}

	acted := false
	for _, id := range stale {
		// Suppress FORGET of a still-failed node while the cluster could recover it
		// itself: persistence-on (same-ID pod returns) or quorum-intact (native
		// election). A genuinely stale node (scale-in remnant) is not failed, so it
		// is always eligible.
		if state.IsNodeFailed(id) && (cluster.Spec.Persistence != nil || state.HasFailoverQuorum()) {
			log.V(1).Info("suppressing FORGET of recoverable failed node", "id", id)
			continue
		}
		if err := r.broadcastForget(ctx, cluster, state, id); err != nil {
			r.recorder.Eventf(cluster, nil, eventWarning, EventNodeForgetFailed, "Recovery",
				"CLUSTER FORGET %s failed: %s", id, err.Error())
			return acted, err
		}
		r.recorder.Eventf(cluster, nil, eventNormal, EventStaleNodeForgotten, "Recovery",
			"forgot stale node %s (no backing ValkeyNode)", id)
		acted = true
	}
	return acted, nil
}

// broadcastForget issues CLUSTER FORGET id on every surviving node that knows it,
// skipping the node being forgotten itself. An "unknown node" reply is benign
// (handled by the client) so a partially-propagated forget converges.
func (r *Reconciler) broadcastForget(
	ctx context.Context, _ *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState, id string,
) error {
	for _, n := range state.Nodes {
		if n.ID == id || n.Client() == nil {
			continue
		}
		if err := n.Client().ClusterForget(ctx, id); err != nil {
			return fmt.Errorf("forget %s on %s: %w", id, n.Addr, err)
		}
	}
	return nil
}

// backingNodeIDs maps each scraped node's live ID to a struct{} when a backing
// ValkeyNode still exists for its address — the set passed to StaleNodeIDs. A
// scraped node whose podIP no longer matches any ValkeyNode is treated as
// backing-less (a deleted scale-in remnant).
func backingNodeIDs(state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList) map[string]struct{} {
	liveIPs := make(map[string]struct{}, len(nodes.Items))
	for i := range nodes.Items {
		if ip := nodes.Items[i].Status.PodIP; ip != "" {
			liveIPs[ip] = struct{}{}
		}
	}
	backing := map[string]struct{}{}
	for _, n := range state.Nodes {
		if n.ID == "" {
			continue
		}
		if _, ok := liveIPs[hostFromAddr(n.Addr)]; ok {
			backing[n.ID] = struct{}{}
		}
	}
	return backing
}

// failoverModeLabel renders a FailoverMode for an event message.
func failoverModeLabel(mode valkey.FailoverMode) string {
	if mode == valkey.FailoverGraceful {
		return "FAILOVER"
	}
	return "FAILOVER " + string(mode)
}
