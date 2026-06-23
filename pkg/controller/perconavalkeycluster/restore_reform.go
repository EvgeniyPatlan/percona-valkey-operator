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
	"cmp"
	"context"
	"fmt"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// reformRestoreTarget re-forms a restore-seeded cluster (CR-8 / 06 §7.5). It is the
// missing leg the restore controller's Forming phase waits on: a freshly-seeded
// cluster cannot self-form via the generic bootstrap because each seed boot loads a
// shard's dump.rdb and the engine AUTO-CLAIMS ownership of ONLY the slots that
// happen to hold keys (the scattered key-slots in nodes.conf), so:
//
//   - the seeded primaries are NOT in PendingNodes (they own a few slots), so
//     assignSlotsToPendingPrimaries skips them and the ~16k empty slots in each
//     shard's range are never assigned -> cluster_state stays fail; and
//   - each seed boots with a single-node nodes.conf (KnownNodes==1) but is not an
//     "isolated PENDING" node (it owns slots), so meetIsolatedNodes skips it and the
//     shards never gossip into one cluster.
//
// CLUSTER RESET/FLUSHSLOTS cannot repair this post-load — the engine refuses both
// once the keyspace is non-empty ("can't be called with master nodes containing
// keys" / "DB must be empty"). So the operator must, for a restore target only,
// (1) MEET every node into one cluster, and (2) ADDSLOTS the GAP slots in each
// shard's canonical even-split range to that shard's primary (the seeded keys
// already sit in that range, so the union reproduces the source topology exactly for
// the operator's even-split clusters). Keys are never touched.
//
// It is gated on isRestoreTarget and a no-op once all 16384 slots are assigned, so a
// re-formed cluster falls through to the normal steady-state reconcile; the restore
// markers are cleared by the restore controller once it observes Ready (06 §7.5).
// Returns done=true (with a fast requeue) when it made progress so the next pass
// observes fresh gossip/slot state.
func (r *Reconciler) reformRestoreTarget(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, error) {
	if !isRestoreTarget(cluster) {
		return false, nil
	}
	if len(state.GetUnassignedSlots()) == 0 {
		// All slots assigned: re-form complete, defer to steady-state reconcile.
		return false, nil
	}

	met := r.meetAllRestoreNodes(ctx, cluster, state)
	if met > 0 {
		// Let the next pass observe converged gossip before assigning slots.
		return true, nil
	}
	return r.assignRestoreSlotRanges(ctx, cluster, state, nodes)
}

// meetAllRestoreNodes MEETs every scraped node to a single deterministic target
// (bidirectionally) so the seeded shards — each booted with a single-node
// nodes.conf — gossip into one cluster. Unlike meetIsolatedNodes it does NOT require
// the nodes to be zero-slot/pending (seeded primaries own scattered key-slots), and
// it MEETs at the LIVE scrape address so stale RDB-recorded peers are bypassed.
// Returns the number of nodes introduced this pass.
func (r *Reconciler) meetAllRestoreNodes(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
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
	slices.SortFunc(reachable, func(a, b *valkey.NodeState) int { return cmp.Compare(a.Addr, b.Addr) })

	// If every node already sees every peer, gossip has converged — nothing to MEET.
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

	target := reachable[0]
	met := 0
	for _, n := range reachable[1:] {
		nodeIP := hostFromAddr(n.Addr)
		targetIP := hostFromAddr(target.Addr)
		if err := target.Client().ClusterMeet(ctx, nodeIP, valkey.ClientPort, valkey.BusPort); err != nil {
			log.V(1).Info("restore re-form MEET target->node failed", "node", n.Addr, "err", err.Error())
			continue
		}
		if err := n.Client().ClusterMeet(ctx, targetIP, valkey.ClientPort, valkey.BusPort); err != nil {
			log.V(1).Info("restore re-form MEET node->target failed", "node", n.Addr, "err", err.Error())
			continue
		}
		met++
	}
	if met > 0 {
		r.recorder.Eventf(cluster, nil, eventNormal, EventClusterMeetBatch, "ClusterMeet",
			"restore re-form: MEET introduced %d node(s)", met)
	}
	return met
}

// assignRestoreSlotRanges assigns each shard primary the GAP slots in its canonical
// even-split range. The seed boot already claimed the key-bearing slots (which fall
// inside that range for the operator's even-split clusters); this fills the empty
// slots so the shard owns its full contiguous range and the union covers all 16384.
// ADDSLOTSRANGE rejects an already-owned slot for the WHOLE call, so only the unowned
// gap (full range minus owned) is assigned. Returns done=true when it assigned any.
func (r *Reconciler) assignRestoreSlotRanges(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, error) {
	log := logf.FromContext(ctx)
	positions := positionsByIP(nodes)
	shardRanges := evenSplitRanges(int(cluster.Spec.Shards))

	assigned, primariesSeen := 0, 0
	for _, n := range state.Nodes {
		if n.Client() == nil {
			continue
		}
		pos, ok := positionFor(positions, n)
		if !ok || !pos.isPrimary() || pos.shard >= len(shardRanges) {
			continue
		}
		primariesSeen++
		want := shardRanges[pos.shard]
		gap := valkey.SubtractRanges([]valkey.SlotRange{want}, n.Slots)
		if len(gap) == 0 {
			continue
		}
		if err := n.Client().ClusterAddSlotsRange(ctx, gap); err != nil {
			return false, fmt.Errorf("restore re-form CLUSTER ADDSLOTSRANGE %s -> shard %d (%s): %w",
				valkey.FormatSlotRanges(gap), pos.shard, n.Addr, err)
		}
		assigned++
	}
	// Slots remain unassigned but no seeded primary could be position-resolved (its
	// live IP not yet in any ValkeyNode status). Surface it so a stuck restore is
	// diagnosable instead of silently requeuing (the next pass retries once the node
	// controller populates pod IPs). Not an error — gossip/IP propagation is async.
	if assigned == 0 && primariesSeen == 0 {
		log.V(1).Info("restore re-form: slots still unassigned but no primary resolved yet (awaiting pod IP propagation)",
			"cluster", client.ObjectKeyFromObject(cluster).String())
	}
	if assigned > 0 {
		r.recorder.Eventf(cluster, nil, eventNormal, EventPrimariesCreated, "AssignSlots",
			"restore re-form: assigned slot-range gaps to %d primary(ies)", assigned)
		return true, nil
	}
	return false, nil
}

// evenSplitRanges returns the canonical contiguous even-split slot range for each of
// n shards (shard 0 = 0..k-1, ...), matching SplitUnassignedEvenly's
// remainder-to-lowest distribution (the layout the operator's bootstrap and backups
// always use, so a restore reproduces the source topology exactly).
func evenSplitRanges(n int) []valkey.SlotRange {
	out := make([]valkey.SlotRange, 0, n)
	if n <= 0 {
		return out
	}
	base, rem := valkey.TotalSlots/n, valkey.TotalSlots%n
	start := 0
	for i := 0; i < n; i++ {
		size := base
		if i < rem {
			size++
		}
		out = append(out, valkey.SlotRange{Start: start, End: start + size - 1})
		start += size
	}
	return out
}
