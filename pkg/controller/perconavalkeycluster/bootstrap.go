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

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// nodePosition is the (shard, node) topology position resolved from a
// ValkeyNode's labels. node==0 is the initial primary.
type nodePosition struct {
	shard int
	node  int
}

// isPrimary reports whether the position is the shard's initial primary
// (node-index 0). Bootstrap assigns slots to node-index-0 primaries and attaches
// node-index>=1 replicas (05 §3).
func (p nodePosition) isPrimary() bool { return p.node == 0 }

// positionsByIP maps each ValkeyNode's status.podIP to its (shard,node) position
// from the topology labels, so a scraped NodeState (keyed by <podIP>:6379) can
// be classified as a bootstrap primary or replica (05 §3 step5/6).
func positionsByIP(nodes *valkeyv1alpha1.ValkeyNodeList) map[string]nodePosition {
	out := make(map[string]nodePosition, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Status.PodIP == "" {
			continue
		}
		shard, errS := strconv.Atoi(node.Labels[naming.LabelShardIndex])
		idx, errN := strconv.Atoi(node.Labels[naming.LabelNodeIndex])
		if errS != nil || errN != nil {
			continue
		}
		out[node.Status.PodIP] = nodePosition{shard: shard, node: idx}
	}
	return out
}

// positionFor returns the topology position of a scraped NodeState by matching
// its dial address back to a ValkeyNode's podIP.
func positionFor(positions map[string]nodePosition, n *valkey.NodeState) (nodePosition, bool) {
	ip := strings.TrimSuffix(n.Addr, ":"+strconv.Itoa(valkey.ClientPort))
	p, ok := positions[ip]
	return p, ok
}

// meetIsolatedNodes batch-introduces every isolated pending node
// (cluster_known_nodes<=1) against a single deterministic meet target,
// bumping each isolated node's config epoch before MEET, and MEETing
// BIDIRECTIONALLY (node->target and target->node) to avoid gossip
// fragmentation. Returns the number of nodes met. Emits ClusterMeetBatch. 04
// §2.1 step10 / 05 §3 step4.
func (r *Reconciler) meetIsolatedNodes(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
) (int, error) {
	log := logf.FromContext(ctx)

	isolated := isolatedPendingNodes(state)
	if len(isolated) == 0 {
		return 0, nil
	}
	target := meetTarget(state, isolated)
	if target == nil || target.Client() == nil {
		return 0, nil
	}

	// Bump config epochs on isolated nodes before MEET so each joins with
	// authority above any dead node it may replace (05 §3 step4).
	for i, n := range isolated {
		if n.Client() == nil {
			continue
		}
		epoch := target.CurrentEpoch + int64(i) + 1
		if err := n.Client().ClusterSetConfigEpoch(ctx, epoch); err != nil {
			log.V(1).Info("CLUSTER SET-CONFIG-EPOCH skipped", "node", n.Addr, "err", err.Error())
		}
	}

	// During a fresh bootstrap the target is itself an isolated seed; exclude it
	// from the MEET loop.
	loop := isolated
	if target == isolated[0] {
		loop = isolated[1:]
	}

	met := 0
	for _, n := range loop {
		if n.Client() == nil {
			continue
		}
		targetIP := hostFromAddr(target.Addr)
		nodeIP := hostFromAddr(n.Addr)
		// node -> target
		if err := n.Client().ClusterMeet(ctx, targetIP, valkey.ClientPort, valkey.BusPort); err != nil {
			return met, fmt.Errorf("CLUSTER MEET %s -> %s: %w", n.Addr, target.Addr, err)
		}
		// target -> node (bidirectional)
		if err := target.Client().ClusterMeet(ctx, nodeIP, valkey.ClientPort, valkey.BusPort); err != nil {
			return met, fmt.Errorf("CLUSTER MEET %s -> %s: %w", target.Addr, n.Addr, err)
		}
		met++
	}
	if met > 0 {
		r.recorder.Eventf(cluster, nil, eventNormal, EventClusterMeetBatch, "ClusterMeet", "MEET introduced %d node(s)", met)
	}
	return met, nil
}

// repairStaleGossip re-MEETs every reachable node to a single deterministic
// target at its CURRENT scrape address when the live cluster is NOT converged
// (some node reports cluster_state != ok) while every backing ValkeyNode is
// Ready — the stale-gossip partition (Bug #1).
//
// The failure it heals: when ALL pods restart and change IPs simultaneously,
// each engine reloads its nodes.conf and keeps every peer in its gossip table
// (so KnownNodes stays > 1 and IsIsolated is false — meetIsolatedNodes skips
// them) but only at the now-dead pre-restart IPs, so the bus links stay down,
// gossip never re-converges, and cluster_state sticks at fail with zero
// messages received. The operator HAS the fresh IPs (ValkeyNode.Status.PodIP,
// which the scrape dialed), so re-introducing every node to one target at its
// real address rebuilds the live links and gossip repairs itself.
//
// It is bounded so a HEALTHY cluster NEVER re-MEETs: the cluster_state:ok gate
// (checked by the caller) is the steady-state guard, and meetAllReachable
// itself is additionally a no-op once gossip has converged (every node already
// sees every reachable peer). Together they guarantee no steady-state MEET
// churn. Returns the number of nodes re-introduced this pass.
func (r *Reconciler) repairStaleGossip(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
) int {
	// skipKnownNodesGuard=true: in a stale-gossip partition every node still KNOWS
	// every peer (KnownNodes==N) so the KnownNodes-based "converged" guard would
	// wrongly suppress the repair. The caller has already established genuine
	// non-convergence via cluster_state != ok (the steady-state ok-guard is what
	// prevents churn here), so the re-MEET at current addresses must proceed.
	return r.meetAllReachable(ctx, cluster, state, true, "gossip repair: re-MEET %d node(s) at current addresses")
}

// meetAllReachable MEETs every reachable scraped node to a single deterministic
// target (bidirectionally, address-sorted) at its CURRENT scrape address, so a
// set of nodes that each booted with a single-node or stale gossip view rejoins
// one cluster. It is the shared core of the restore re-form (seeded shards) and
// the stale-gossip repair (simultaneous IP-changing restart) MEET-all paths.
//
// Unlike meetIsolatedNodes it does NOT require the nodes to be zero-slot/pending
// (seeded primaries own scattered slots; a restarted primary owns its full
// range) and it dials the LIVE address so stale RDB/nodes.conf-recorded peers
// are bypassed. When skipKnownNodesGuard is false it is a NO-OP once gossip has
// converged — every reachable node already sees every reachable peer (the
// restore-reform path, where high KnownNodes genuinely means converged). The
// stale-gossip repair passes skipKnownNodesGuard=true because there KnownNodes
// stays high while the bus links are dead; that caller is instead gated on
// cluster_state != ok so it still never churns a healthy cluster. The
// progressMsg is the recorder format string (one %d for the count). Returns the
// number of nodes introduced this pass.
func (r *Reconciler) meetAllReachable(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
	skipKnownNodesGuard bool, progressMsg string,
) int {
	log := logf.FromContext(ctx)

	reachable := make([]*valkey.NodeState, 0, len(state.Nodes))
	for _, n := range state.Nodes {
		if n.Client() != nil {
			reachable = append(reachable, n)
		}
	}
	if len(reachable) < 2 {
		return 0
	}
	slices.SortFunc(reachable, func(a, b *valkey.NodeState) int { return strings.Compare(a.Addr, b.Addr) })

	// If every node already sees every peer, gossip has converged — nothing to
	// MEET. This is the steady-state guard that prevents churn on a healthy
	// cluster even if the caller ever invoked us spuriously. The stale-gossip
	// repair skips it (KnownNodes stays high there) and relies on its own
	// cluster_state != ok gate instead.
	if !skipKnownNodesGuard {
		allConverged := true
		for _, n := range reachable {
			if n.KnownNodes < len(reachable) {
				allConverged = false
				break
			}
		}
		if allConverged {
			return 0
		}
	}

	target := reachable[0]
	met := 0
	for _, n := range reachable[1:] {
		nodeIP := hostFromAddr(n.Addr)
		targetIP := hostFromAddr(target.Addr)
		if err := target.Client().ClusterMeet(ctx, nodeIP, valkey.ClientPort, valkey.BusPort); err != nil {
			log.V(1).Info("meet-all target->node failed", "node", n.Addr, "err", err.Error())
			continue
		}
		if err := n.Client().ClusterMeet(ctx, targetIP, valkey.ClientPort, valkey.BusPort); err != nil {
			log.V(1).Info("meet-all node->target failed", "node", n.Addr, "err", err.Error())
			continue
		}
		met++
	}
	if met > 0 {
		r.recorder.Eventf(cluster, nil, eventNormal, EventClusterMeetBatch, "ClusterMeet", progressMsg, met)
	}
	return met
}

// isolatedPendingNodes returns the pending nodes still isolated (need MEET).
func isolatedPendingNodes(state *valkey.ClusterState) []*valkey.NodeState {
	var isolated []*valkey.NodeState
	for _, n := range state.PendingNodes {
		if n.IsIsolated() {
			isolated = append(isolated, n)
		}
	}
	// Deterministic order by address.
	slices.SortFunc(isolated, func(a, b *valkey.NodeState) int { return strings.Compare(a.Addr, b.Addr) })
	return isolated
}

// meetTarget picks the node to MEET all isolated nodes against (deterministic):
// (1) an existing slot-owning shard primary; (2) a non-isolated pending node;
// (3) the first isolated node as a bootstrap seed. 05 §3 step4 (OQ-3.D: ties
// broken by address order via isolatedPendingNodes sort).
func meetTarget(state *valkey.ClusterState, isolated []*valkey.NodeState) *valkey.NodeState {
	for _, shard := range state.Shards {
		if p := shard.Primary(); p != nil {
			return p
		}
	}
	for _, n := range state.PendingNodes {
		if !n.IsIsolated() {
			return n
		}
	}
	if len(isolated) == 0 {
		return nil
	}
	return isolated[0]
}

// assignSlotsToPendingPrimaries assigns each primary-labeled (node-index 0)
// non-isolated zero-slot pending node its even share of the UNASSIGNED slots via
// a single CLUSTER ADDSLOTSRANGE (idempotency guard: only zero-slot primaries,
// so re-issue never hits "slot busy"). Ranges are address-sorted deterministic.
// During scale-out (no unassigned slots) it returns 0 and leaves new primaries
// for the rebalancer (Wave 2b). Emits PrimariesCreated. 04 §2.1 step11 / 05 §3
// step5.
func (r *Reconciler) assignSlotsToPendingPrimaries(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (int, error) {
	log := logf.FromContext(ctx)
	positions := positionsByIP(nodes)

	// A single-node cluster (1 shard, 0 replicas) has nobody to MEET, so its lone
	// primary is permanently "isolated" — that is the correct terminal state, and
	// the isolation guard in pendingPrimaries must be relaxed for it (05 §3 step5
	// exception).
	singleNode := cluster.Spec.Shards == 1 && cluster.Spec.Replicas == 0

	primaries := pendingPrimaries(state, positions, singleNode)
	if len(primaries) == 0 {
		return 0, nil
	}
	unassigned := state.GetUnassignedSlots()
	if len(unassigned) == 0 {
		// Scale-out: slots already owned. New primaries wait for the rebalancer
		// (GO-3.14, Wave 2b).
		log.V(1).Info("no unassigned slots; new primaries await rebalance", "count", len(primaries))
		return 0, nil
	}

	plans := valkey.SplitUnassignedEvenly(unassigned, len(primaries))
	assigned := 0
	for i, p := range primaries {
		ranges := plans[i]
		if len(ranges) == 0 || p.Client() == nil {
			continue
		}
		if err := p.Client().ClusterAddSlotsRange(ctx, ranges); err != nil {
			return assigned, fmt.Errorf("CLUSTER ADDSLOTSRANGE %s -> %s: %w", valkey.FormatSlotRanges(ranges), p.Addr, err)
		}
		assigned++
	}
	if assigned > 0 {
		r.recorder.Eventf(cluster, nil, eventNormal, EventPrimariesCreated, "AssignSlots", "Assigned slots to %d primary(ies)", assigned)
	}
	return assigned, nil
}

// pendingPrimaries returns the non-isolated, zero-slot pending nodes whose
// topology label marks them the shard primary (node-index 0), address-sorted for
// deterministic slot ranges. When allowIsolated is true (a single-node cluster)
// the isolation guard is relaxed since the lone primary has nobody to MEET.
func pendingPrimaries(state *valkey.ClusterState, positions map[string]nodePosition, allowIsolated bool) []*valkey.NodeState {
	var primaries []*valkey.NodeState
	for _, n := range state.PendingNodes {
		if n.IsIsolated() && !allowIsolated {
			continue
		}
		pos, ok := positionFor(positions, n)
		if !ok || !pos.isPrimary() {
			continue
		}
		primaries = append(primaries, n)
	}
	slices.SortFunc(primaries, func(a, b *valkey.NodeState) int { return strings.Compare(a.Addr, b.Addr) })
	return primaries
}

// replicatePendingReplicas attaches each replica-labeled (node-index>=1) pending
// node to its shard's primary via CLUSTER REPLICATE <primaryId>, matched by the
// shard-index label. Isolated replicas and replicas whose primary is not yet
// gossip-visible are skipped (retried next reconcile). Emits ReplicasAttached.
// 04 §2.1 step12 / 05 §3 step6.
func (r *Reconciler) replicatePendingReplicas(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (int, error) {
	log := logf.FromContext(ctx)
	positions := positionsByIP(nodes)
	primaryIDByShard := shardPrimaryIDs(state, positions)

	replicated := 0
	for _, n := range state.PendingNodes {
		if n.IsIsolated() || n.Client() == nil {
			continue
		}
		pos, ok := positionFor(positions, n)
		if !ok || pos.isPrimary() {
			continue // genuine new primary handled in step 11.
		}
		primaryID, ok := primaryIDByShard[pos.shard]
		if !ok || primaryID == "" {
			log.V(1).Info("primary not yet known for shard, skipping replica", "shard", pos.shard, "node", n.Addr)
			continue
		}
		if err := n.Client().ClusterReplicate(ctx, primaryID); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unknown node") {
				// Gossip hasn't propagated the primary ID yet; retry next pass.
				log.V(1).Info("replica does not yet know primary (gossip pending)", "node", n.Addr, "primaryId", primaryID)
				continue
			}
			return replicated, fmt.Errorf("CLUSTER REPLICATE %s -> %s: %w", n.Addr, primaryID, err)
		}
		replicated++
	}
	if replicated > 0 {
		r.recorder.Eventf(cluster, nil, eventNormal, EventReplicasAttached, "AttachReplicas", "Attached %d replica(s)", replicated)
	}
	return replicated, nil
}

// shardPrimaryIDs maps each shard index to its live primary's node ID, derived
// from the slot-owning shards' primaries matched back to topology positions, AND
// from the zero-slot pending primaries (a freshly scaled-out shard's primary owns
// no slots yet but a replica must still attach to it so the shard is whole before
// the rebalancer hands it slots — 05 §4). A genuine slot-owning primary wins over
// a pending one for the same shard index.
func shardPrimaryIDs(state *valkey.ClusterState, positions map[string]nodePosition) map[int]string {
	out := map[int]string{}
	// Pending (zero-slot) primaries first; slot-owning primaries overwrite them.
	for _, p := range state.PendingNodes {
		if !p.IsPrimary() {
			continue
		}
		if pos, ok := positions[hostFromAddr(p.Addr)]; ok && pos.isPrimary() {
			out[pos.shard] = p.ID
		}
	}
	for _, shard := range state.Shards {
		p := shard.Primary()
		if p == nil {
			continue
		}
		if pos, ok := positions[hostFromAddr(p.Addr)]; ok {
			out[pos.shard] = p.ID
		}
	}
	return out
}

// hostFromAddr strips the ":<port>" suffix from a dial address.
func hostFromAddr(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[:i]
	}
	return addr
}
