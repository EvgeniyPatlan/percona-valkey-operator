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

// rebalanceTolerance is the ±1-slot slack tolerated before a shard counts as
// over- or under-target. Because the initial slot-assignment order may not match
// the address-sorted target order, a single-slot rounding difference must not
// trigger a ping-pong of moves (05 §4).
const rebalanceTolerance = 1

// Move is a single planned slot migration from one primary to another. It is the
// output of the deterministic planners; the controller turns it into one atomic
// CLUSTER MIGRATESLOTS per reconcile (05 §4-§5).
type Move struct {
	// SrcID is the source primary's node ID.
	SrcID string
	// DstID is the destination primary's node ID.
	DstID string
	// Ranges are the contiguous slot ranges to migrate (a compaction of a
	// BatchSize-capped slot batch).
	Ranges []SlotRange
}

// NumSlots returns the total number of slots the move migrates.
func (m *Move) NumSlots() int {
	return CountSlots(m.Ranges)
}

// shardSlots is a per-primary slot accounting used during rebalance planning.
type shardSlots struct {
	node     *NodeState
	ranges   []SlotRange
	numSlots int
	target   int
}

// PlanRebalanceMove computes the single next slot move that improves balance
// across shards primaries, or nil when the cluster is already balanced (within
// ±1 slot) or not ready to rebalance.
//
// It is a pure, deterministic function of the ClusterState (R4 mitigation):
//
//   - shards are sorted by primary address (a stable key for a pod's lifetime);
//   - per-shard targets come from targetSlotsPerShard (remainder to the
//     lowest-addressed shards);
//   - the first over-target shard (surplus > tolerance) is the source and the
//     first under-target shard (deficit > tolerance) is the destination;
//   - the batch is min(surplus, deficit, BatchSize) slots, taken low-to-high.
//
// Identical input topology always yields the identical (src, dst, ranges)
// decision — no map-iteration nondeterminism — so the move sequence is
// replayable and two operator instances converge on the same next move (05 §4).
// The plan is idempotent but non-transactional: after a crash the next pass
// re-scrapes live state and re-plans from wherever the slots actually landed.
func PlanRebalanceMove(s *ClusterState, shards int) *Move {
	if shards <= 0 {
		return nil
	}
	primaries := s.effectiveShards()
	if len(primaries) != shards {
		// Topology not yet at the target shard count (mid scale-out/in) — defer.
		return nil
	}

	allocs := make([]*shardSlots, 0, len(primaries))
	for _, p := range primaries {
		allocs = append(allocs, &shardSlots{
			node:     p,
			ranges:   append([]SlotRange(nil), p.Slots...),
			numSlots: p.NumSlots(),
		})
	}
	// effectiveShards already sorts by address; assign targets in that order.
	targets := targetSlotsPerShard(shards)

	var src, dst *shardSlots
	for i, a := range allocs {
		a.target = targets[i]
		if src == nil && a.numSlots-a.target > rebalanceTolerance {
			src = a
		}
		if dst == nil && a.target-a.numSlots > rebalanceTolerance {
			dst = a
		}
	}
	if src == nil || dst == nil {
		return nil
	}

	count := min(src.numSlots-src.target, dst.target-dst.numSlots, BatchSize)
	slots := takeSlots(src.ranges, count)
	return &Move{
		SrcID:  src.node.ID,
		DstID:  dst.node.ID,
		Ranges: SlotsToRanges(slots),
	}
}

// PlanDrainMove computes the next batch of up to batch slots to move off a
// draining shard's primary onto the first valid remaining primary, or nil when
// the draining shard already owns zero slots (05 §5).
//
// The destination choice is intentionally simple — the first remaining primary
// in address order — because a final PlanRebalanceMove pass re-equalises the
// survivors after the drain completes. Like PlanRebalanceMove it is pure and
// deterministic: remaining primaries are address-sorted, and the batch is taken
// low-to-high, so a crashed drain re-plans convergently on the next pass.
func PlanDrainMove(src *NodeState, remaining []*NodeState, batch int) *Move {
	if src == nil || batch <= 0 {
		return nil
	}
	srcCount := src.NumSlots()
	if srcCount == 0 {
		return nil
	}

	candidates := append([]*NodeState(nil), remaining...)
	slices.SortFunc(candidates, compareByAddr)
	var dst *NodeState
	for _, c := range candidates {
		if c != nil && c.ID != src.ID && c.IsPrimary() {
			dst = c
			break
		}
	}
	if dst == nil {
		return nil
	}

	slots := takeSlots(src.Slots, min(srcCount, batch))
	return &Move{
		SrcID:  src.ID,
		DstID:  dst.ID,
		Ranges: SlotsToRanges(slots),
	}
}
