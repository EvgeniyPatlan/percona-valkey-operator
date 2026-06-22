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
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// handleScaleIn drains slots off the excess (highest-index) shards and, once a
// shard owns zero slots, deletes its ValkeyNodes replicas-before-primary (05 §5 /
// 04 §2.1 step13, GO-3.15). The strict order is drain -> delete -> forget:
// CLUSTER FORGET of the now-backing-less node IDs happens in forgetStaleNodes on
// a subsequent pass (recovery.go). Excess replicas (node-index >= 1+replicas) are
// reaped symmetrically. One batch/effect per reconcile so a mid-flight failure is
// recoverable. Emits SlotsDraining / ValkeyNodeDeleted / DrainFailed.
//
// It never deletes a slot-owning node: deletion is gated on the draining shard
// owning zero slots, so a premature delete can never orphan slots (R10).
func (r *Reconciler) handleScaleIn(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, error) {
	wantShards := int(cluster.Spec.Shards)

	// Reap excess replica nodes (node-index >= 1+replicas) regardless of shard:
	// symmetric replica scale-in. A shard whose replica count shrank keeps its
	// primary; only the surplus replicas are deleted.
	if acted, err := r.deleteExcessReplicas(ctx, cluster, nodes); err != nil || acted {
		return acted, err
	}

	// Draining shards are those whose pod shard-index >= spec.shards.
	drainingPrimaries := drainingShardPrimaries(state, positionsByIP(nodes), wantShards)
	if len(drainingPrimaries) == 0 {
		// No draining shard left with slots — delete any fully-drained excess
		// shard's ValkeyNodes (replicas before primary).
		return r.deleteDrainedShardNodes(ctx, cluster, state, nodes)
	}

	// Drain one batch off the first draining shard's primary into a survivor.
	return r.drainShard(ctx, cluster, state, nodes, drainingPrimaries[0], wantShards)
}

// drainShard moves up to one BatchSize slot batch off a draining primary onto a
// surviving primary via the guarded atomic MIGRATESLOTS, emitting SlotsDraining.
func (r *Reconciler) drainShard(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList, src *valkey.NodeState, wantShards int,
) (bool, error) {
	remaining := remainingPrimaries(state, positionsByIP(nodes), wantShards)
	move := valkey.PlanDrainMove(src, remaining, valkey.BatchSize)
	if move == nil {
		return false, nil // already drained, or no valid destination yet.
	}
	moved, err := r.migrateSlots(ctx, cluster, state, move)
	if err != nil {
		r.recorder.Eventf(cluster, nil, eventWarning, EventDrainFailed, "Drain",
			"draining %s from %s failed: %s", valkey.FormatSlotRanges(move.Ranges), move.SrcID, err.Error())
		return false, fmt.Errorf("scale-in drain: %w", err)
	}
	if !moved {
		return true, nil // a guard deferred; requeue and re-plan.
	}
	r.recorder.Eventf(cluster, nil, eventNormal, EventSlotsDraining, "Drain",
		"draining %s slot(s) off %s into a survivor", valkey.FormatSlotRanges(move.Ranges), move.SrcID)
	return true, nil
}

// deleteDrainedShardNodes deletes the ValkeyNodes of any shard whose pod
// shard-index >= spec.shards once that shard owns zero slots, replicas before the
// primary, one node per call. The stale node IDs are CLUSTER FORGET-ten by
// forgetStaleNodes on a later pass (05 §5). Returns acted=true after deleting one
// node so the caller requeues.
func (r *Reconciler) deleteDrainedShardNodes(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, error) {
	wantShards := int(cluster.Spec.Shards)

	// Excess (draining) ValkeyNodes ordered replicas-before-primary.
	excess := excessShardNodes(nodes, wantShards)
	if len(excess) == 0 {
		return false, nil
	}

	// Safety: never delete a node whose backing primary still owns slots (R10).
	// The drain phase above keeps running until every draining shard is empty, so
	// by here the draining shards own zero slots — but double-check live state.
	if drainingShardsStillOwnSlots(state, positionsByIP(nodes), wantShards) {
		return false, nil // still draining; let drainShard finish first.
	}

	target := excess[0] // replicas first (excessShardNodes sorts node-index desc).
	if err := r.Delete(ctx, target); err != nil && !apierrorsIsNotFound(err) {
		return false, fmt.Errorf("delete excess ValkeyNode %s: %w", target.Name, err)
	}
	r.recorder.Eventf(cluster, target, eventNormal, EventValkeyNodeDeleted, "ScaleIn",
		"deleted drained ValkeyNode %s", target.Name)
	return true, nil
}

// deleteExcessReplicas deletes ValkeyNodes whose node-index >= 1+spec.replicas
// for shards that are NOT being removed (a pure replica scale-down), replicas in
// descending node-index order, one per call. Returns acted=true after a delete.
func (r *Reconciler) deleteExcessReplicas(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, error) {
	wantShards := int(cluster.Spec.Shards)
	wantPerShard := 1 + int(cluster.Spec.Replicas)

	var surplus []*valkeyv1alpha1.ValkeyNode
	for i := range nodes.Items {
		node := &nodes.Items[i]
		shard, errS := strconv.Atoi(node.Labels[naming.LabelShardIndex])
		idx, errN := strconv.Atoi(node.Labels[naming.LabelNodeIndex])
		if errS != nil || errN != nil {
			continue
		}
		// Only shards that survive (shard < wantShards); draining shards are
		// handled by deleteDrainedShardNodes.
		if shard < wantShards && idx >= wantPerShard {
			surplus = append(surplus, node)
		}
	}
	if len(surplus) == 0 {
		return false, nil
	}
	// Highest node-index first (drop the newest replica first).
	slices.SortFunc(surplus, func(a, b *valkeyv1alpha1.ValkeyNode) int {
		return nodeIndexOf(b) - nodeIndexOf(a)
	})
	target := surplus[0]
	if err := r.Delete(ctx, target); err != nil && !apierrorsIsNotFound(err) {
		return false, fmt.Errorf("delete surplus replica %s: %w", target.Name, err)
	}
	r.recorder.Eventf(cluster, target, eventNormal, EventValkeyNodeDeleted, "ScaleIn",
		"deleted surplus replica %s", target.Name)
	return true, nil
}

// drainingShardPrimaries returns the live primaries of shards whose pod
// shard-index >= wantShards that still own slots, address-sorted via the planner.
func drainingShardPrimaries(state *valkey.ClusterState, positions map[string]nodePosition, wantShards int) []*valkey.NodeState {
	var draining []*valkey.NodeState
	for _, shard := range state.Shards {
		p := shard.Primary()
		if p == nil || p.NumSlots() == 0 {
			continue
		}
		pos, ok := positions[hostFromAddr(p.Addr)]
		if !ok || pos.shard < wantShards {
			continue
		}
		draining = append(draining, p)
	}
	sortByAddr(draining)
	return draining
}

// drainingShardsStillOwnSlots reports whether any draining shard still owns slots.
func drainingShardsStillOwnSlots(state *valkey.ClusterState, positions map[string]nodePosition, wantShards int) bool {
	return len(drainingShardPrimaries(state, positions, wantShards)) > 0
}

// remainingPrimaries returns the live primaries of surviving shards
// (shard-index < wantShards) — the drain destinations.
func remainingPrimaries(state *valkey.ClusterState, positions map[string]nodePosition, wantShards int) []*valkey.NodeState {
	var remaining []*valkey.NodeState
	for _, shard := range state.Shards {
		p := shard.Primary()
		if p == nil {
			continue
		}
		pos, ok := positions[hostFromAddr(p.Addr)]
		if ok && pos.shard >= wantShards {
			continue // a draining shard, not a destination.
		}
		remaining = append(remaining, p)
	}
	return remaining
}

// excessShardNodes returns the ValkeyNodes of shards being removed
// (shard-index >= wantShards), ordered replicas-before-primary (node-index
// descending, so node 0 — the primary — is deleted last).
func excessShardNodes(nodes *valkeyv1alpha1.ValkeyNodeList, wantShards int) []*valkeyv1alpha1.ValkeyNode {
	var excess []*valkeyv1alpha1.ValkeyNode
	for i := range nodes.Items {
		node := &nodes.Items[i]
		shard, err := strconv.Atoi(node.Labels[naming.LabelShardIndex])
		if err != nil || shard < wantShards {
			continue
		}
		excess = append(excess, node)
	}
	slices.SortFunc(excess, func(a, b *valkeyv1alpha1.ValkeyNode) int {
		if sa, sb := shardIndexOf(a), shardIndexOf(b); sa != sb {
			return sa - sb
		}
		return nodeIndexOf(b) - nodeIndexOf(a) // replicas first.
	})
	return excess
}

// nodeIndexOf reads a ValkeyNode's node-index label as an int (0 on parse error).
func nodeIndexOf(node *valkeyv1alpha1.ValkeyNode) int {
	v, _ := strconv.Atoi(node.Labels[naming.LabelNodeIndex])
	return v
}

// shardIndexOf reads a ValkeyNode's shard-index label as an int (0 on parse error).
func shardIndexOf(node *valkeyv1alpha1.ValkeyNode) int {
	v, _ := strconv.Atoi(node.Labels[naming.LabelShardIndex])
	return v
}

// sortByAddr orders scraped NodeStates by dial address for determinism.
func sortByAddr(ns []*valkey.NodeState) {
	slices.SortFunc(ns, func(a, b *valkey.NodeState) int { return strings.Compare(a.Addr, b.Addr) })
}

// apierrorsIsNotFound reports whether err is a Kubernetes NotFound — used in the
// delete paths so a concurrently-GC'd object is treated as already gone.
func apierrorsIsNotFound(err error) bool {
	return err != nil && client.IgnoreNotFound(err) == nil
}
