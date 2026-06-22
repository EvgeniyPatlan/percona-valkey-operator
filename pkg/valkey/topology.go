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

package valkey

import "slices"

// IsIsolated reports whether the node has not yet been introduced to any peer:
// cluster_known_nodes <= 1 (05 §3). Fresh bootstrap / scale-out nodes are
// isolated and must pass through MEET before they can own slots or replicate.
func (n *NodeState) IsIsolated() bool {
	return n.KnownNodes <= 1
}

// GetUnassignedSlots returns the slot ranges not owned by any shard primary —
// the gaps in 0..maxSlot (05 §2). An empty result means full coverage, the gate
// for marking the cluster Ready.
func (s *ClusterState) GetUnassignedSlots() []SlotRange {
	remaining := []SlotRange{{Start: 0, End: maxSlot}}
	for _, shard := range s.Shards {
		for _, owned := range shard.Slots {
			next := make([]SlotRange, 0, len(remaining)+1)
			for _, base := range remaining {
				next = append(next, subtractSlotRange(base, owned)...)
			}
			remaining = next
		}
	}
	if len(remaining) == 0 {
		// Full coverage: return nil (idiomatic empty) rather than a non-nil
		// empty slice, so callers can treat the gate uniformly.
		return nil
	}
	return remaining
}

// effectiveShards returns the slot-owning shard primaries plus any slot-less
// pending primaries, so a scaled-out (zero-slot) primary is surfaced for
// health/rebalancing before it has received slots (05 §4). Sorted by address
// for deterministic ordering.
func (s *ClusterState) effectiveShards() []*NodeState {
	primaries := make([]*NodeState, 0, len(s.Shards)+len(s.PendingNodes))
	for _, shard := range s.Shards {
		if p := shard.Primary(); p != nil {
			primaries = append(primaries, p)
		}
	}
	for _, p := range s.PendingNodes {
		if p.IsPrimary() {
			primaries = append(primaries, p)
		}
	}
	slices.SortFunc(primaries, compareByAddr)
	return primaries
}

// EffectivePrimaryCount returns the number of effective shard primaries (slot
// owners plus slot-less pending primaries) — the live shard count used to drive
// rebalance/scale decisions (05 §4).
func (s *ClusterState) EffectivePrimaryCount() int {
	return len(s.effectiveShards())
}

// IsReplicationInSync reports whether every replica across all shards has its
// replication link up (05 §3 verify step). Primaries are skipped.
func (s *ClusterState) IsReplicationInSync() bool {
	for _, shard := range s.Shards {
		for _, n := range shard.Nodes {
			if n.IsReplica() && !n.LinkUp {
				return false
			}
		}
	}
	return true
}

// GetSyncedReplicas returns the replicas of primaryID that are eligible for a
// graceful failover target: not flagged fail/pfail/fail? and master_link_status
// up (05 §6, §10). Order follows the underlying node order.
func (s *ClusterState) GetSyncedReplicas(primaryID string) []*NodeState {
	var replicas []*NodeState
	for _, n := range s.Nodes {
		if n.PrimaryID != primaryID || !n.IsReplica() {
			continue
		}
		if n.HasFlag(flagFail) || n.HasFlag(flagPFail) || n.HasFlag(flagPFailSoft) {
			continue
		}
		if !n.LinkUp {
			continue
		}
		replicas = append(replicas, n)
	}
	return replicas
}

// HighestOffsetReplica returns the synced replica of primaryID with the greatest
// replication offset — the most caught-up one, the best graceful-failover target
// (05 §6). Returns nil when there are no synced replicas. Ties keep slice order.
func (s *ClusterState) HighestOffsetReplica(primaryID string) *NodeState {
	return highestOffset(s.GetSyncedReplicas(primaryID))
}

// BestReplicaOf returns the highest-offset replica that lists deadPrimaryID as
// its primary, regardless of link state — the takeover candidate when the
// primary is gone (05 §7). Returns nil when no replica references it.
func (s *ClusterState) BestReplicaOf(deadPrimaryID string) *NodeState {
	var replicas []*NodeState
	for _, n := range s.Nodes {
		if n.PrimaryID == deadPrimaryID && n.IsReplica() {
			replicas = append(replicas, n)
		}
	}
	return highestOffset(replicas)
}

// highestOffset returns the replica with the greatest Offset. A node whose
// offset is unknown (-1) sorts below any known offset; ties keep slice order.
// Returns nil for an empty slice.
func highestOffset(replicas []*NodeState) *NodeState {
	var best *NodeState
	for _, r := range replicas {
		if best == nil || r.Offset > best.Offset {
			best = r
		}
	}
	return best
}

// IsNodeFailed reports whether any scraped node's gossip view flags nodeID as
// fail or fail? (05 §7). fail? (soft fail) is treated as failed because under a
// majority-down partition a pfail can never be promoted to a hard fail, yet the
// operator still must act. Falls back to the node's own myself flags.
func (s *ClusterState) IsNodeFailed(nodeID string) bool {
	for _, n := range s.Nodes {
		if peer, ok := n.peers[nodeID]; ok {
			if slices.Contains(peer.flags, flagFail) || slices.Contains(peer.flags, flagPFailSoft) {
				return true
			}
		}
	}
	if self := s.byID[nodeID]; self != nil {
		return self.IsFailed()
	}
	return false
}

// FailedPrimaries returns the shard primaries currently flagged failed (05 §7),
// the candidates for orphaned-replica takeover.
func (s *ClusterState) FailedPrimaries() []*NodeState {
	var failed []*NodeState
	for _, shard := range s.Shards {
		if p := shard.Primary(); p != nil && s.IsNodeFailed(p.ID) {
			failed = append(failed, p)
		}
	}
	return failed
}

// HasReplicaOf reports whether any node still lists nodeID as its primary
// (05 §7). FORGET must be suppressed while this holds: forgetting a failed
// primary strips it from peers' tables and prevents them voting in its replica's
// election.
func (s *ClusterState) HasReplicaOf(nodeID string) bool {
	return s.hasReplicaOf(nodeID, nil)
}

// hasReplicaOf is the shared scan over peer views: any peer that lists nodeID as
// its primaryID counts. When backingIDs is non-nil the scan is restricted to
// peers whose own ID is backed (a live replica with a real ValkeyNode); a nil
// backingIDs counts every replica (the broad HasReplicaOf semantics).
func (s *ClusterState) hasReplicaOf(nodeID string, backingIDs map[string]struct{}) bool {
	for _, n := range s.Nodes {
		for _, peer := range n.peers {
			if peer.primaryID != nodeID {
				continue
			}
			if backingIDs == nil {
				return true
			}
			if _, backed := backingIDs[peer.id]; backed {
				return true
			}
		}
	}
	return false
}

// HasFailoverQuorum reports whether a majority of slot-owning primaries are
// reachable (05 §7). Valkey requires a primary majority to vote in an election;
// without quorum no native failover can complete and the operator must consider
// a unilateral takeover. The denominator is cluster_size (slot-owning primaries
// including failed ones), taken as the max across nodes in case gossip has not
// fully propagated. Returns false when the cluster size is unknown.
func (s *ClusterState) HasFailoverQuorum() bool {
	if len(s.Shards) == 0 {
		return false
	}
	var live, clusterSize int
	for _, shard := range s.Shards {
		if p := shard.Primary(); p != nil && shard.NumSlots() > 0 && !s.IsNodeFailed(p.ID) {
			live++
		}
	}
	for _, n := range s.Nodes {
		if n.ClusterSize > clusterSize {
			clusterSize = n.ClusterSize
		}
	}
	if clusterSize == 0 {
		return false
	}
	return live > clusterSize/2
}

// FailoverDecision selects the failover mode for promoting a replica of a failed
// primary (05 §7, §8). It encodes the safety rule: a graceful FORCE election
// when a primary quorum still exists, but a unilateral TAKEOVER only as a last
// resort when quorum is lost (the replica cannot win a normal vote). The caller
// must already have established that the primary is failed and persistence is
// off (with persistence on, the same-node-ID pod returns and no takeover is
// issued at all — 05 §7, §9).
//
// A graceful (empty-mode) CLUSTER FAILOVER is never used for recovery of a dead
// primary because it requires the old primary to be alive to hand off; that mode
// is reserved for proactive failover before rolling a live primary.
func FailoverDecision(hasQuorum bool) FailoverMode {
	if hasQuorum {
		return FailoverForce
	}
	return FailoverTakeover
}

// StaleNodeIDs returns node IDs present in the cluster gossip but absent from
// backingIDs (the set of IDs that still have a backing ValkeyNode). These are
// the FORGET candidates — nodes left over from a scale-in deletion or a
// permanently dead pod (05 §5, §7). IDs for which a failover is still pending
// (HasReplicaOf) are excluded so FORGET never races an in-flight election.
func (s *ClusterState) StaleNodeIDs(backingIDs map[string]struct{}) []string {
	seen := map[string]struct{}{}
	var stale []string
	for _, n := range s.Nodes {
		for id := range n.peers {
			if id == "" {
				continue
			}
			if _, backed := backingIDs[id]; backed {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			if s.hasReplicaOf(id, backingIDs) {
				// A LIVE (backed) replica still points at this node — a failover
				// may be pending; do not forget it yet. A merely stale replica
				// (itself backing-less, e.g. a scale-in remnant) must NOT keep its
				// likewise-deleted primary pinned, or both linger forever (05 §5).
				continue
			}
			seen[id] = struct{}{}
			stale = append(stale, id)
		}
	}
	slices.Sort(stale)
	return stale
}

// AnyMigrationInProgress reports whether any of the given GETSLOTMIGRATIONS
// entries is still non-terminal (running). The controller calls this on the
// source primary before issuing a fresh CLUSTER MIGRATESLOTS so an in-flight
// range is never re-issued (CR-6, 05 §4).
func AnyMigrationInProgress(migrations []SlotMigration) bool {
	for _, m := range migrations {
		if !m.IsTerminal() {
			return true
		}
	}
	return false
}

// GossipVisible reports whether the source node's gossip view (its peers table)
// already knows dstID. The controller requires the destination to be
// gossip-visible from the source before migrating a slot range to it, otherwise
// MIGRATESLOTS would fail with "unknown node" (05 §4 destination-gossip guard).
func (s *ClusterState) GossipVisible(srcID, dstID string) bool {
	src := s.byID[srcID]
	if src == nil {
		return false
	}
	_, ok := src.peers[dstID]
	return ok
}

// PrimaryByShardID returns the live primary NodeState of the shard with the
// given shard ID, or nil. Used by proactive failover to locate the shard whose
// primary is about to be rolled (05 §6).
func (s *ClusterState) PrimaryByShardID(shardID string) *NodeState {
	for _, shard := range s.Shards {
		if shard.ID == shardID {
			return shard.Primary()
		}
	}
	return nil
}

// compareByAddr orders nodes by dial address for deterministic, reproducible
// planning (05 §4). Two reconcilers (or a re-election mid-rebalance) compute the
// identical ordering.
func compareByAddr(a, b *NodeState) int {
	switch {
	case a.Addr < b.Addr:
		return -1
	case a.Addr > b.Addr:
		return 1
	default:
		return 0
	}
}
