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

// findNode returns the parsed node with the given ID, or nil.
func findNode(nodes []*NodeState, id string) *NodeState {
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func TestParseClusterNodes_HealthyThreeShard(t *testing.T) {
	// A realistic 3-shard x 2 (primary + replica) dump with the canonical
	// 5462/5461/5461 split, including the txt: verbatim prefix.
	raw := "txt:" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5461\n" +
		"r0 10.0.0.2:6379@16379 slave p0 0 100 1 connected\n" +
		"p1 10.0.0.3:6379@16379 master - 0 0 2 connected 5462-10922\n" +
		"r1 10.0.0.4:6379@16379 slave p1 0 100 2 connected\n" +
		"p2 10.0.0.5:6379@16379 master - 0 0 3 connected 10923-16383\n" +
		"r2 10.0.0.6:6379@16379 slave p2 0 100 3 connected\n"

	nodes, err := ParseClusterNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 6 {
		t.Fatalf("got %d nodes, want 6", len(nodes))
	}

	p0 := findNode(nodes, "p0")
	if p0 == nil {
		t.Fatal("p0 not found")
	}
	if p0.Addr != "10.0.0.1:6379" {
		t.Errorf("p0.Addr = %q, want 10.0.0.1:6379", p0.Addr)
	}
	if p0.BusPort != 16379 {
		t.Errorf("p0.BusPort = %d, want 16379", p0.BusPort)
	}
	if p0.Role != RolePrimary {
		t.Errorf("p0.Role = %q, want primary", p0.Role)
	}
	if p0.PrimaryID != "" {
		t.Errorf("p0.PrimaryID = %q, want empty (was '-')", p0.PrimaryID)
	}
	if p0.ConfigEpoch != 1 {
		t.Errorf("p0.ConfigEpoch = %d, want 1", p0.ConfigEpoch)
	}
	if !p0.HasFlag(flagMyself) || !p0.HasFlag(flagMaster) {
		t.Errorf("p0.Flags = %v, want myself,master", p0.Flags)
	}
	wantSlots := []SlotRange{{Start: 0, End: 5461}}
	if !reflect.DeepEqual(p0.Slots, wantSlots) {
		t.Errorf("p0.Slots = %v, want %v", p0.Slots, wantSlots)
	}

	r0 := findNode(nodes, "r0")
	if r0 == nil {
		t.Fatal("r0 not found")
	}
	if r0.Role != RoleReplica {
		t.Errorf("r0.Role = %q, want replica", r0.Role)
	}
	if r0.PrimaryID != "p0" {
		t.Errorf("r0.PrimaryID = %q, want p0", r0.PrimaryID)
	}
	if len(r0.Slots) != 0 {
		t.Errorf("r0.Slots = %v, want empty", r0.Slots)
	}
}

func TestParseClusterNodes_MultiRangeSlots(t *testing.T) {
	raw := "p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-100 200-300 5000\n"
	nodes, err := ParseClusterNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p0 := findNode(nodes, "p0")
	want := []SlotRange{{0, 100}, {200, 300}, {5000, 5000}}
	if !reflect.DeepEqual(p0.Slots, want) {
		t.Errorf("Slots = %v, want %v", p0.Slots, want)
	}
	if p0.NumSlots() != 101+101+1 {
		t.Errorf("NumSlots = %d, want 203", p0.NumSlots())
	}
}

func TestParseClusterNodes_MigratingImportingMarkers(t *testing.T) {
	// Importing/migrating markers must be skipped — they are not owned ranges.
	raw := "p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460 [5461->-dst123] [5462-<-src456]\n"
	nodes, err := ParseClusterNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p0 := findNode(nodes, "p0")
	want := []SlotRange{{0, 5460}}
	if !reflect.DeepEqual(p0.Slots, want) {
		t.Errorf("Slots = %v, want %v (markers skipped)", p0.Slots, want)
	}
}

func TestParseClusterNodes_FailFlags(t *testing.T) {
	raw := "" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5461\n" +
		"dead 10.0.0.9:6379@16379 master,fail - 0 0 2 disconnected 5462-10922\n" +
		"soft 10.0.0.8:6379@16379 master,fail? - 0 0 3 connected 10923-16383\n"
	nodes, err := ParseClusterNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dead := findNode(nodes, "dead")
	if !dead.IsFailed() {
		t.Error("dead should be IsFailed (fail flag)")
	}
	soft := findNode(nodes, "soft")
	if !soft.IsFailed() {
		t.Error("soft should be IsFailed (fail? flag treated as failed)")
	}
	p0 := findNode(nodes, "p0")
	if p0.IsFailed() {
		t.Error("p0 should not be failed")
	}
}

func TestParseClusterNodes_HandshakeNoAddr(t *testing.T) {
	// A handshaking node may carry a :0 address and no slots; a noaddr node has
	// lost its address. Both must parse without error.
	raw := "" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n" +
		"hs :0@0 handshake - 0 0 0 connected\n" +
		"na - noaddr,slave p0 0 0 0 disconnected\n"
	nodes, err := ParseClusterNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3", len(nodes))
	}
	hs := findNode(nodes, "hs")
	if !hs.HasFlag(flagHandshake) {
		t.Errorf("hs.Flags = %v, want handshake", hs.Flags)
	}
	// A handshaking node advertises ":0@0"; the dial portion ":0" is preserved
	// as-is (the operator skips empty/zero-port nodes elsewhere).
	if hs.Addr != ":0" {
		t.Errorf("hs.Addr = %q, want :0", hs.Addr)
	}
	na := findNode(nodes, "na")
	if !na.HasFlag(flagNoAddr) {
		t.Errorf("na.Flags = %v, want noaddr", na.Flags)
	}
}

func TestParseClusterNodes_TolerantOfMalformed(t *testing.T) {
	// Blank lines, short lines, and trailing/unknown fields must be tolerated.
	raw := "\n" +
		"too few fields\n" +
		"   \n" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383 extraTrailingJunk\n"
	nodes, err := ParseClusterNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1 (others skipped)", len(nodes))
	}
	// "extraTrailingJunk" is an unparseable slot token, so the tolerant parser
	// drops only this node's slots rather than erroring.
	if len(nodes[0].Slots) != 0 {
		t.Errorf("Slots = %v, want empty (malformed token drops slots)", nodes[0].Slots)
	}
}

func TestParseClusterNodes_HostnameSuffix(t *testing.T) {
	raw := "p0 10.0.0.1:6379@16379,valkey-mycache-0-0 myself,master - 0 0 1 connected 0-16383\n"
	nodes, err := ParseClusterNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p0 := findNode(nodes, "p0")
	if p0.Addr != "10.0.0.1:6379" {
		t.Errorf("Addr = %q, want 10.0.0.1:6379 (hostname suffix dropped)", p0.Addr)
	}
	if p0.BusPort != 16379 {
		t.Errorf("BusPort = %d, want 16379", p0.BusPort)
	}
}

func TestParseClusterInfo(t *testing.T) {
	raw := "txt:cluster_state:ok\r\ncluster_slots_assigned:16384\r\ncluster_known_nodes:6\r\ncluster_size:3\r\n"
	state, assigned, known := ParseClusterInfo(raw)
	if state != ClusterStateOK {
		t.Errorf("state = %q, want ok", state)
	}
	if assigned != 16384 {
		t.Errorf("assigned = %d, want 16384", assigned)
	}
	if known != 6 {
		t.Errorf("known = %d, want 6", known)
	}
}

func TestParseClusterInfo_MissingFieldsTolerant(t *testing.T) {
	state, assigned, known := ParseClusterInfo("# header\ngarbage\ncluster_state:fail\n")
	if state != "fail" {
		t.Errorf("state = %q, want fail", state)
	}
	if assigned != 0 || known != 0 {
		t.Errorf("missing numeric fields should default to 0, got %d/%d", assigned, known)
	}
}

func TestParseInfoReplicationTyped(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantRole   Role
		wantLink   bool
		wantOffset int64
		wantSlaves int
	}{
		{
			name:       "primary",
			raw:        "role:master\r\nconnected_slaves:2\r\nmaster_repl_offset:12345\r\n",
			wantRole:   RolePrimary,
			wantLink:   false,
			wantOffset: 12345,
			wantSlaves: 2,
		},
		{
			name:       "replica in sync",
			raw:        "role:slave\r\nmaster_link_status:up\r\nslave_repl_offset:999\r\n",
			wantRole:   RoleReplica,
			wantLink:   true,
			wantOffset: 999,
			wantSlaves: 0,
		},
		{
			name:       "replica link down",
			raw:        "role:slave\nmaster_link_status:down\nslave_repl_offset:5\n",
			wantRole:   RoleReplica,
			wantLink:   false,
			wantOffset: 5,
			wantSlaves: 0,
		},
		{
			name:       "missing offset defaults to -1",
			raw:        "role:slave\nmaster_link_status:up\n",
			wantRole:   RoleReplica,
			wantLink:   true,
			wantOffset: -1,
			wantSlaves: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, link, offset, slaves := ParseInfoReplicationTyped(tt.raw)
			if role != tt.wantRole {
				t.Errorf("role = %q, want %q", role, tt.wantRole)
			}
			if link != tt.wantLink {
				t.Errorf("link = %v, want %v", link, tt.wantLink)
			}
			if offset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", offset, tt.wantOffset)
			}
			if slaves != tt.wantSlaves {
				t.Errorf("connected_slaves = %d, want %d", slaves, tt.wantSlaves)
			}
		})
	}
}

func TestNewClusterState_GroupsShardsAndPending(t *testing.T) {
	// p0 owns slots (a shard), r0 is its replica, p1 owns zero slots (pending).
	p0 := &NodeState{ID: "p0", ShardID: "s0", Addr: "10.0.0.1", Role: RolePrimary, Slots: []SlotRange{{0, 16383}}}
	r0 := &NodeState{ID: "r0", ShardID: "s0", Addr: "10.0.0.2", Role: RoleReplica, PrimaryID: "p0", LinkUp: true}
	p1 := &NodeState{ID: "p1", ShardID: "s1", Addr: "10.0.0.3", Role: RolePrimary}

	state := NewClusterState([]*NodeState{p0, r0, p1, nil})
	if len(state.Nodes) != 3 {
		t.Fatalf("Nodes = %d, want 3 (nil dropped)", len(state.Nodes))
	}
	if len(state.Shards) != 1 {
		t.Fatalf("Shards = %d, want 1", len(state.Shards))
	}
	if len(state.PendingNodes) != 1 || state.PendingNodes[0].ID != "p1" {
		t.Fatalf("PendingNodes = %v, want [p1]", state.PendingNodes)
	}
	shard := state.Shards[0]
	if shard.PrimaryID != "p0" {
		t.Errorf("shard.PrimaryID = %q, want p0", shard.PrimaryID)
	}
	if shard.Primary() == nil || shard.Primary().ID != "p0" {
		t.Errorf("shard.Primary() = %v, want p0", shard.Primary())
	}
	if state.NodeByID("p0") != p0 {
		t.Error("byID index missing p0")
	}
	if state.NodeByAddr("10.0.0.2") != r0 {
		t.Error("byAddr index missing r0")
	}
}

func TestClusterState_CloseClients(t *testing.T) {
	// CloseClients must tolerate nil state and nil clients.
	var nilState *ClusterState
	nilState.CloseClients()

	closed := &countingCloser{}
	state := NewClusterState([]*NodeState{
		{ID: "p0", Role: RolePrimary, Slots: []SlotRange{{0, 16383}}, client: closed},
		{ID: "r0", Role: RoleReplica, PrimaryID: "p0"},
	})
	state.CloseClients()
	if closed.closes != 1 {
		t.Errorf("Close called %d times, want 1", closed.closes)
	}
}

// countingCloser is a minimal ClusterClient stub that counts Close calls.
type countingCloser struct {
	ClusterClient
	closes int
}

func (c *countingCloser) Close() error {
	c.closes++
	return nil
}
