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

import (
	"reflect"
	"testing"
)

// primary builds a slot-owning primary NodeState at a given address.
func primary(id, addr string, ranges ...SlotRange) *NodeState {
	return &NodeState{ID: id, Addr: addr, Role: RolePrimary, ShardID: "s-" + id, Slots: ranges}
}

// stateFrom assembles a ClusterState directly from primaries (each its own
// shard) — enough for planner tests, which only read slot ownership + address.
func stateFrom(primaries ...*NodeState) *ClusterState {
	nodes := make([]*NodeState, 0, len(primaries))
	nodes = append(nodes, primaries...)
	return NewClusterState(nodes)
}

func TestPlanRebalanceMove_BalancedReturnsNil(t *testing.T) {
	// Already balanced 3-shard 5462/5461/5461 -> no move.
	s := stateFrom(
		primary("p0", "10.0.0.1", SlotRange{0, 5461}),
		primary("p1", "10.0.0.2", SlotRange{5462, 10922}),
		primary("p2", "10.0.0.3", SlotRange{10923, 16383}),
	)
	if m := PlanRebalanceMove(s, 3); m != nil {
		t.Errorf("balanced cluster should yield nil move, got %+v", m)
	}
}

func TestPlanRebalanceMove_WrongShardCountDefers(t *testing.T) {
	s := stateFrom(
		primary("p0", "10.0.0.1", SlotRange{0, 8191}),
		primary("p1", "10.0.0.2", SlotRange{8192, 16383}),
	)
	// Two effective shards but caller expects 3 (mid scale-out) -> defer.
	if m := PlanRebalanceMove(s, 3); m != nil {
		t.Errorf("shard-count mismatch should defer, got %+v", m)
	}
	if m := PlanRebalanceMove(s, 0); m != nil {
		t.Errorf("zero shards -> nil, got %+v", m)
	}
}

func TestPlanRebalanceMove_ScaleOut3to4(t *testing.T) {
	// Three full shards (~5461 each) plus a fresh 4th primary owning zero slots.
	// Target for 4 shards is 4096 each. The planner must move one BatchSize batch
	// from the first over-target shard (p0) to the first under-target (p3).
	s := stateFrom(
		primary("p0", "10.0.0.1", SlotRange{0, 5461}),      // 5462, surplus
		primary("p1", "10.0.0.2", SlotRange{5462, 10922}),  // 5461
		primary("p2", "10.0.0.3", SlotRange{10923, 16383}), // 5461
		primary("p3", "10.0.0.4"),                          // 0, deficit
	)
	m := PlanRebalanceMove(s, 4)
	if m == nil {
		t.Fatal("expected a move for 3->4")
	}
	if m.SrcID != "p0" || m.DstID != "p3" {
		t.Errorf("move src/dst = %s->%s, want p0->p3", m.SrcID, m.DstID)
	}
	if m.NumSlots() != BatchSize {
		t.Errorf("move size = %d, want %d", m.NumSlots(), BatchSize)
	}
	// Slots taken low-to-high from p0.
	want := []SlotRange{{0, BatchSize - 1}}
	if !reflect.DeepEqual(m.Ranges, want) {
		t.Errorf("move ranges = %v, want %v", m.Ranges, want)
	}
}

func TestPlanRebalanceMove_Deterministic(t *testing.T) {
	build := func() *ClusterState {
		return stateFrom(
			primary("p2", "10.0.0.3", SlotRange{10923, 16383}),
			primary("p0", "10.0.0.1", SlotRange{0, 5461}),
			primary("p3", "10.0.0.4"),
			primary("p1", "10.0.0.2", SlotRange{5462, 10922}),
		)
	}
	// Build two states with the primaries in different insertion order; the
	// address sort must make the plan identical (R4 — no map-iteration nondet).
	first := PlanRebalanceMove(build(), 4)
	for i := 0; i < 20; i++ {
		got := PlanRebalanceMove(build(), 4)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("non-deterministic plan: pass %d gave %+v, want %+v", i, got, first)
		}
	}
}

func TestPlanRebalanceMove_IdempotentReplan(t *testing.T) {
	// Simulate applying the first 3->4 move, then re-planning: the planner must
	// converge (pick up from where slots landed) and eventually reach nil.
	s := stateFrom(
		primary("p0", "10.0.0.1", SlotRange{0, 5461}),
		primary("p1", "10.0.0.2", SlotRange{5462, 10922}),
		primary("p2", "10.0.0.3", SlotRange{10923, 16383}),
		primary("p3", "10.0.0.4"),
	)
	moves := 0
	for {
		m := PlanRebalanceMove(s, 4)
		if m == nil {
			break
		}
		moves++
		if moves > 100 {
			t.Fatal("rebalance did not converge within 100 moves")
		}
		applyMove(t, s, m)
	}
	// After convergence every shard must be within ±1 of 4096.
	for _, sh := range s.Shards {
		n := sh.NumSlots()
		if n < 4095 || n > 4097 {
			t.Errorf("shard %s ended with %d slots, want ~4096", sh.ID, n)
		}
	}
	// And full coverage preserved.
	if got := s.GetUnassignedSlots(); len(got) != 0 {
		t.Errorf("coverage lost during rebalance: %v", got)
	}
}

func TestPlanDrainMove(t *testing.T) {
	src := primary("p3", "10.0.0.4", SlotRange{12000, 16383}) // draining, owns slots
	remaining := []*NodeState{
		primary("p2", "10.0.0.3", SlotRange{8000, 11999}),
		primary("p0", "10.0.0.1", SlotRange{0, 3999}),
		primary("p1", "10.0.0.2", SlotRange{4000, 7999}),
	}
	m := PlanDrainMove(src, remaining, BatchSize)
	if m == nil {
		t.Fatal("expected a drain move")
	}
	if m.SrcID != "p3" {
		t.Errorf("drain src = %s, want p3", m.SrcID)
	}
	// Destination is the first remaining primary in address order (p0 @ .1).
	if m.DstID != "p0" {
		t.Errorf("drain dst = %s, want p0 (lowest address)", m.DstID)
	}
	if m.NumSlots() != BatchSize {
		t.Errorf("drain size = %d, want %d", m.NumSlots(), BatchSize)
	}
	want := []SlotRange{{12000, 12000 + BatchSize - 1}}
	if !reflect.DeepEqual(m.Ranges, want) {
		t.Errorf("drain ranges = %v, want %v", m.Ranges, want)
	}
}

func TestPlanDrainMove_NoSlotsReturnsNil(t *testing.T) {
	src := primary("p3", "10.0.0.4") // already drained
	remaining := []*NodeState{primary("p0", "10.0.0.1", SlotRange{0, 16383})}
	if m := PlanDrainMove(src, remaining, BatchSize); m != nil {
		t.Errorf("drained shard should yield nil, got %+v", m)
	}
}

func TestPlanDrainMove_NoDestinationReturnsNil(t *testing.T) {
	src := primary("p3", "10.0.0.4", SlotRange{0, 100})
	// Only candidate is the source itself / a non-primary.
	if m := PlanDrainMove(src, []*NodeState{src}, BatchSize); m != nil {
		t.Errorf("no valid dst should yield nil, got %+v", m)
	}
	if m := PlanDrainMove(nil, nil, BatchSize); m != nil {
		t.Errorf("nil src should yield nil, got %+v", m)
	}
	if m := PlanDrainMove(src, []*NodeState{primary("d", "10.0.0.1")}, 0); m != nil {
		t.Errorf("zero batch should yield nil, got %+v", m)
	}
}

func TestPlanDrainMove_DrainToZeroConverges(t *testing.T) {
	// A draining shard with 4384 slots must drain to zero in batches.
	src := primary("p3", "10.0.0.4", SlotRange{12000, 16383})
	dst := primary("p0", "10.0.0.1", SlotRange{0, 11999})
	moves := 0
	for {
		m := PlanDrainMove(src, []*NodeState{dst}, BatchSize)
		if m == nil {
			break
		}
		moves++
		if moves > 100 {
			t.Fatal("drain did not converge")
		}
		moveSlotsBetween(src, dst, m)
	}
	if src.NumSlots() != 0 {
		t.Errorf("src ended with %d slots, want 0", src.NumSlots())
	}
}

// applyMove mutates the ClusterState's shards to reflect a completed migration:
// the src loses the moved slots, the dst gains them. Used to drive the
// converge/idempotency tests.
func applyMove(t *testing.T, s *ClusterState, m *Move) {
	t.Helper()
	src := s.NodeByID(m.SrcID)
	dst := s.NodeByID(m.DstID)
	if src == nil || dst == nil {
		t.Fatalf("applyMove: missing node src=%v dst=%v", src, dst)
	}
	moveSlotsBetween(src, dst, m)
	// Rebuild shard views so NumSlots/effectiveShards reflect the new ownership.
	rebuilt := NewClusterState(s.Nodes)
	s.Shards = rebuilt.Shards
	s.PendingNodes = rebuilt.PendingNodes
}

// moveSlotsBetween transfers the move's slots from src to dst NodeStates.
func moveSlotsBetween(src, dst *NodeState, m *Move) {
	moved := map[int]struct{}{}
	for _, r := range m.Ranges {
		for slot := r.Start; slot <= r.End; slot++ {
			moved[slot] = struct{}{}
		}
	}
	var remaining []int
	for _, r := range src.Slots {
		for slot := r.Start; slot <= r.End; slot++ {
			if _, gone := moved[slot]; !gone {
				remaining = append(remaining, slot)
			}
		}
	}
	src.Slots = SlotsToRanges(remaining)

	var dstSlots []int
	for _, r := range dst.Slots {
		for slot := r.Start; slot <= r.End; slot++ {
			dstSlots = append(dstSlots, slot)
		}
	}
	for slot := range moved {
		dstSlots = append(dstSlots, slot)
	}
	dst.Slots = SlotsToRanges(dstSlots)
}
