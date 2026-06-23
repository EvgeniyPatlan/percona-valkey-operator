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

import "testing"

// healthyState builds a healthy 3-shard/1-replica ClusterState from a CLUSTER
// NODES dump so the peer maps (used by IsNodeFailed/HasReplicaOf/StaleNodeIDs)
// are populated exactly as the parser would build them live.
func healthyState(t *testing.T) *ClusterState {
	t.Helper()
	raw := "" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5461\n" +
		"r0 10.0.0.2:6379@16379 slave p0 0 100 1 connected\n" +
		"p1 10.0.0.3:6379@16379 master - 0 0 2 connected 5462-10922\n" +
		"r1 10.0.0.4:6379@16379 slave p1 0 100 2 connected\n" +
		"p2 10.0.0.5:6379@16379 master - 0 0 3 connected 10923-16383\n" +
		"r2 10.0.0.6:6379@16379 slave p2 0 100 3 connected\n"
	nodes, err := ParseClusterNodes(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Stamp live role + link + shard + cluster_size as the scrape would.
	for _, n := range nodes {
		n.ShardID = "shard-" + n.ID
		n.ClusterSize = 3
		n.KnownNodes = 6
		if n.IsReplica() {
			n.LinkUp = true
		}
	}
	return NewClusterState(nodes)
}

func TestIsIsolated(t *testing.T) {
	fresh := &NodeState{KnownNodes: 1}
	if !fresh.IsIsolated() {
		t.Error("a node with cluster_known_nodes=1 must be isolated")
	}
	joined := &NodeState{KnownNodes: 6}
	if joined.IsIsolated() {
		t.Error("a node with cluster_known_nodes=6 must not be isolated")
	}
}

func TestIsReplicationInSync(t *testing.T) {
	state := healthyState(t)
	if !state.IsReplicationInSync() {
		t.Error("healthy state should be in sync")
	}
	// Knock one replica's link down.
	state.NodeByID("r1").LinkUp = false
	if state.IsReplicationInSync() {
		t.Error("a replica with link down breaks in-sync")
	}
}

func TestGetSyncedReplicas(t *testing.T) {
	state := healthyState(t)
	synced := state.GetSyncedReplicas("p0")
	if len(synced) != 1 || synced[0].ID != "r0" {
		t.Fatalf("synced replicas of p0 = %v, want [r0]", synced)
	}
	// A failed replica is excluded.
	r0 := state.NodeByID("r0")
	r0.Flags = append(r0.Flags, flagFail)
	if got := state.GetSyncedReplicas("p0"); len(got) != 0 {
		t.Errorf("failed replica must be excluded, got %v", got)
	}
}

func TestHighestOffsetReplica(t *testing.T) {
	state := healthyState(t)
	// Give p0 two synced replicas with different offsets.
	r0 := state.NodeByID("r0")
	r0.Offset = 100
	extra := &NodeState{ID: "rx", PrimaryID: "p0", Role: RoleReplica, LinkUp: true, Offset: 500, Addr: "10.0.0.7"}
	state.Nodes = append(state.Nodes, extra)

	best := state.HighestOffsetReplica("p0")
	if best == nil || best.ID != "rx" {
		t.Errorf("HighestOffsetReplica = %v, want rx (offset 500)", best)
	}
	// No synced replicas -> nil.
	if got := state.HighestOffsetReplica("nonexistent"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestHighestOffset_UnknownSortsLast(t *testing.T) {
	// Offset -1 (unknown) must lose to any known offset; ties keep order.
	a := &NodeState{ID: "a", Offset: -1}
	b := &NodeState{ID: "b", Offset: 5}
	if got := highestOffset([]*NodeState{a, b}); got.ID != "b" {
		t.Errorf("got %s, want b (known offset beats unknown)", got.ID)
	}
	c := &NodeState{ID: "c", Offset: 10}
	d := &NodeState{ID: "d", Offset: 10}
	if got := highestOffset([]*NodeState{c, d}); got.ID != "c" {
		t.Errorf("ties should keep slice order, got %s", got.ID)
	}
	if got := highestOffset(nil); got != nil {
		t.Errorf("empty slice should yield nil, got %v", got)
	}
}

func TestBestReplicaOf(t *testing.T) {
	state := healthyState(t)
	r0 := state.NodeByID("r0")
	r0.Offset = 100
	// A second, more-caught-up replica of p0, even if its link is down (takeover
	// candidate ignores link state).
	extra := &NodeState{ID: "rx", PrimaryID: "p0", Role: RoleReplica, Offset: 900, Addr: "10.0.0.8"}
	state.Nodes = append(state.Nodes, extra)
	best := state.BestReplicaOf("p0")
	if best == nil || best.ID != "rx" {
		t.Errorf("BestReplicaOf(p0) = %v, want rx (offset 900)", best)
	}
	if got := state.BestReplicaOf("unknown"); got != nil {
		t.Errorf("BestReplicaOf(unknown) = %v, want nil", got)
	}
}

func TestIsNodeFailed(t *testing.T) {
	raw := "" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5461\n" +
		"dead 10.0.0.9:6379@16379 master,fail - 0 0 2 disconnected 5462-10922\n" +
		"soft 10.0.0.8:6379@16379 master,fail? - 0 0 3 connected 10923-16383\n"
	nodes, _ := ParseClusterNodes(raw)
	state := NewClusterState(nodes)

	if !state.IsNodeFailed("dead") {
		t.Error("dead (fail) should be failed")
	}
	if !state.IsNodeFailed("soft") {
		t.Error("soft (fail?) should be treated as failed (majority-down case)")
	}
	if state.IsNodeFailed("p0") {
		t.Error("p0 should not be failed")
	}
	if state.IsNodeFailed("ghost") {
		t.Error("unknown node should not be failed")
	}
}

func TestHasReplicaOf(t *testing.T) {
	state := healthyState(t)
	if !state.HasReplicaOf("p0") {
		t.Error("p0 has replica r0 — HasReplicaOf should be true")
	}
	if state.HasReplicaOf("nobody") {
		t.Error("nobody has no replica — HasReplicaOf should be false")
	}
}

func TestHasFailoverQuorum(t *testing.T) {
	tests := []struct {
		name        string
		livePrim    int
		clusterSize int
		want        bool
	}{
		{"2 of 3 — quorum", 2, 3, true},
		{"1 of 3 — no quorum", 1, 3, false},
		{"3 of 3 — quorum", 3, 3, true},
		{"2 of 4 — no quorum (exactly half)", 2, 4, false},
		{"3 of 4 — quorum", 3, 4, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := buildQuorumState(tt.livePrim, tt.clusterSize)
			if got := state.HasFailoverQuorum(); got != tt.want {
				t.Errorf("HasFailoverQuorum() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasFailoverQuorum_Edges(t *testing.T) {
	if (&ClusterState{}).HasFailoverQuorum() {
		t.Error("empty state has no quorum")
	}
	// cluster_size unknown -> conservatively false.
	state := NewClusterState([]*NodeState{
		{ID: "p0", Role: RolePrimary, Slots: []SlotRange{{0, 16383}}},
	})
	if state.HasFailoverQuorum() {
		t.Error("missing cluster_size must yield no quorum")
	}
}

// buildQuorumState builds a state with livePrim healthy slot-owning primaries
// and (clusterSize-livePrim) failed ones, all advertising cluster_size.
func buildQuorumState(livePrim, clusterSize int) *ClusterState {
	var nodes []*NodeState
	slot := 0
	for i := 0; i < clusterSize; i++ {
		n := &NodeState{
			ID:          string(rune('a' + i)),
			ShardID:     "s" + string(rune('a'+i)),
			Role:        RolePrimary,
			Slots:       []SlotRange{{Start: slot, End: slot + 99}},
			ClusterSize: clusterSize,
		}
		slot += 100
		if i >= livePrim {
			n.Flags = []string{flagMaster, flagFail}
		}
		nodes = append(nodes, n)
	}
	state := NewClusterState(nodes)
	// Populate peer views so IsNodeFailed sees the fail flags.
	peers := map[string]*peerView{}
	for _, n := range nodes {
		peers[n.ID] = &peerView{id: n.ID, flags: n.Flags}
	}
	for _, n := range state.Nodes {
		n.peers = peers
	}
	return state
}

func TestFailoverDecision(t *testing.T) {
	// The FORCE-vs-TAKEOVER matrix: FORCE while a quorum exists, TAKEOVER (last
	// resort) only on quorum loss (05 §7).
	if got := FailoverDecision(true); got != FailoverForce {
		t.Errorf("quorum intact -> %q, want FORCE", got)
	}
	if got := FailoverDecision(false); got != FailoverTakeover {
		t.Errorf("quorum lost -> %q, want TAKEOVER", got)
	}
}

func TestStaleNodeIDs(t *testing.T) {
	// p0,r0 are backed; "ghost" appears in gossip but has no backing node and no
	// live replica, so it is stale. "deadprimary" has a live replica pointing at
	// it (failover pending) so it must be suppressed.
	raw := "" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
		"r0 10.0.0.2:6379@16379 slave p0 0 100 1 connected\n" +
		"ghost 10.0.0.9:6379@16379 master,fail - 0 0 2 disconnected 8192-16383\n" +
		"deadprimary 10.0.0.8:6379@16379 master,fail - 0 0 3 disconnected\n" +
		"orphan 10.0.0.7:6379@16379 slave deadprimary 0 0 3 connected\n"
	nodes, _ := ParseClusterNodes(raw)
	for _, n := range nodes {
		n.ShardID = "s-" + n.ID
	}
	state := NewClusterState(nodes)

	backing := map[string]struct{}{"p0": {}, "r0": {}, "orphan": {}}
	stale := state.StaleNodeIDs(backing)

	// ghost is stale; deadprimary is suppressed (orphan replicates it).
	if len(stale) != 1 || stale[0] != "ghost" {
		t.Errorf("StaleNodeIDs = %v, want [ghost] (deadprimary suppressed by pending failover)", stale)
	}
}

func TestFailedPrimaries(t *testing.T) {
	raw := "" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
		"dead 10.0.0.9:6379@16379 master,fail - 0 0 2 disconnected 8192-16383\n"
	nodes, _ := ParseClusterNodes(raw)
	for _, n := range nodes {
		n.ShardID = "s-" + n.ID
	}
	state := NewClusterState(nodes)
	failed := state.FailedPrimaries()
	if len(failed) != 1 || failed[0].ID != "dead" {
		t.Errorf("FailedPrimaries = %v, want [dead]", failed)
	}
}

func TestGetSyncedReplicas_ExcludesPFailSoft(t *testing.T) {
	state := healthyState(t)
	r0 := state.NodeByID("r0")
	r0.Flags = append(r0.Flags, flagPFailSoft)
	if got := state.GetSyncedReplicas("p0"); len(got) != 0 {
		t.Errorf("a fail?-flagged replica must be excluded, got %v", got)
	}
}

func TestNodeStateClientGetter(t *testing.T) {
	stub := &countingCloser{}
	n := &NodeState{client: stub}
	if n.Client() != stub {
		t.Error("Client() should return the live client")
	}
	if (&NodeState{}).Client() != nil {
		t.Error("Client() with no client should be nil")
	}
}

func TestCompareByAddr_Equal(t *testing.T) {
	a := &NodeState{Addr: "10.0.0.1:6379"}
	b := &NodeState{Addr: "10.0.0.1:6379"}
	if compareByAddr(a, b) != 0 {
		t.Error("equal addresses should compare 0")
	}
}

func TestEffectivePrimaryCount(t *testing.T) {
	state := healthyState(t)
	if got := state.EffectivePrimaryCount(); got != 3 {
		t.Errorf("EffectivePrimaryCount = %d, want 3", got)
	}
	// Add a slot-less pending primary (a scaled-out shard).
	state.PendingNodes = append(state.PendingNodes, &NodeState{ID: "p3", Role: RolePrimary, Addr: "10.0.0.9"})
	if got := state.EffectivePrimaryCount(); got != 4 {
		t.Errorf("with a pending primary EffectivePrimaryCount = %d, want 4", got)
	}
}

// TestAllReachableClusterStateOK covers the Bug #1 non-convergence signal: it is
// the cluster_state:ok gate that bounds the gossip-repair step so a healthy
// cluster never re-MEETs, and it fires on the stale-gossip case (KnownNodes>1 yet
// cluster_state != ok) that IsIsolated cannot see.
func TestAllReachableClusterStateOK(t *testing.T) {
	// Empty state: nothing to affirm.
	if (&ClusterState{}).AllReachableClusterStateOK() {
		t.Error("empty state must report not-ok")
	}
	// All nodes ok -> ok (the healthy steady state that must NOT trigger repair).
	allOK := NewClusterState([]*NodeState{
		{ID: "a", Addr: "10.0.0.1:6379", KnownNodes: 3, ClusterStateOK: true},
		{ID: "b", Addr: "10.0.0.2:6379", KnownNodes: 3, ClusterStateOK: true},
		{ID: "c", Addr: "10.0.0.3:6379", KnownNodes: 3, ClusterStateOK: true},
	})
	if !allOK.AllReachableClusterStateOK() {
		t.Error("all nodes cluster_state:ok must report ok (no repair)")
	}
	// Stale-gossip partition: every node still KNOWS its peers (KnownNodes>1, so
	// none is IsIsolated) but reports cluster_state:fail -> not ok (repair fires).
	stale := NewClusterState([]*NodeState{
		{ID: "a", Addr: "10.0.0.1:6379", KnownNodes: 3, ClusterStateOK: false},
		{ID: "b", Addr: "10.0.0.2:6379", KnownNodes: 3, ClusterStateOK: false},
		{ID: "c", Addr: "10.0.0.3:6379", KnownNodes: 3, ClusterStateOK: false},
	})
	for _, n := range stale.Nodes {
		if n.IsIsolated() {
			t.Fatalf("stale node %s must NOT be isolated (KnownNodes>1)", n.ID)
		}
	}
	if stale.AllReachableClusterStateOK() {
		t.Error("a stale-gossip partition (cluster_state:fail) must report not-ok so repair fires")
	}
	// A single non-ok node is enough to withhold ok.
	mixed := NewClusterState([]*NodeState{
		{ID: "a", Addr: "10.0.0.1:6379", ClusterStateOK: true},
		{ID: "b", Addr: "10.0.0.2:6379", ClusterStateOK: false},
	})
	if mixed.AllReachableClusterStateOK() {
		t.Error("one non-ok node must withhold cluster-wide ok")
	}
}
