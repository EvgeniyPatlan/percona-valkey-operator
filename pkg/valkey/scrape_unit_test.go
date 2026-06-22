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
	"context"
	"testing"
)

func TestSplitUnassignedEvenlyThreeWay(t *testing.T) {
	t.Parallel()
	full := []SlotRange{{Start: 0, End: TotalSlots - 1}}
	chunks := SplitUnassignedEvenly(full, 3)
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(chunks))
	}
	counts := []int{CountSlots(chunks[0]), CountSlots(chunks[1]), CountSlots(chunks[2])}
	// Remainder to the lowest-addressed shard: 5462/5461/5461 = 16384.
	if counts[0] != 5462 || counts[1] != 5461 || counts[2] != 5461 {
		t.Fatalf("uneven split: %v", counts)
	}
	if counts[0]+counts[1]+counts[2] != TotalSlots {
		t.Fatalf("split does not sum to 16384: %v", counts)
	}
	// Contiguous, non-overlapping, ascending.
	if chunks[0][0].Start != 0 || chunks[0][len(chunks[0])-1].End != 5461 {
		t.Fatalf("first chunk wrong: %v", chunks[0])
	}
	if chunks[1][0].Start != 5462 {
		t.Fatalf("second chunk should start at 5462: %v", chunks[1])
	}
}

func TestSplitUnassignedEvenlyEdgeCases(t *testing.T) {
	t.Parallel()
	if got := SplitUnassignedEvenly(nil, 0); len(got) != 0 {
		t.Fatalf("n=0 should give empty: %v", got)
	}
	// More primaries than slots: extra chunks are empty.
	chunks := SplitUnassignedEvenly([]SlotRange{{Start: 0, End: 1}}, 4)
	nonEmpty := 0
	total := 0
	for _, c := range chunks {
		total += CountSlots(c)
		if len(c) > 0 {
			nonEmpty++
		}
	}
	if total != 2 || nonEmpty != 2 {
		t.Fatalf("2 slots over 4 primaries: total=%d nonEmpty=%d", total, nonEmpty)
	}
}

// scrapeStub is a ClusterClient returning canned engine output for ScrapeNode.
type scrapeStub struct {
	ClusterClient
	id, shardID, info, clusterInfo, clusterNodes string
}

func (s *scrapeStub) ClusterMyID(context.Context) (string, error)      { return s.id, nil }
func (s *scrapeStub) ClusterMyShardID(context.Context) (string, error) { return s.shardID, nil }
func (s *scrapeStub) Info(context.Context, string) (string, error)     { return s.info, nil }
func (s *scrapeStub) ClusterInfo(context.Context) (string, error)      { return s.clusterInfo, nil }
func (s *scrapeStub) ClusterNodes(context.Context) (string, error)     { return s.clusterNodes, nil }

func TestScrapeNodePrimaryWithSlots(t *testing.T) {
	t.Parallel()
	id := "1111111111111111111111111111111111111111"
	stub := &scrapeStub{
		id:          id,
		shardID:     "shardA",
		info:        "# Replication\r\nrole:master\r\nmaster_repl_offset:500\r\n",
		clusterInfo: "cluster_state:ok\r\ncluster_slots_assigned:16384\r\ncluster_known_nodes:3\r\ncluster_size:3\r\ncluster_current_epoch:7\r\n",
		clusterNodes: id + " 10.0.0.1:6379@16379 myself,master - 0 0 7 connected 0-5461\n" +
			"2222222222222222222222222222222222222222 10.0.0.2:6379@16379 master - 0 0 8 connected 5462-10922\n",
	}
	n, err := ScrapeNode(context.Background(), "10.0.0.1:6379", stub)
	if err != nil {
		t.Fatalf("ScrapeNode error: %v", err)
	}
	if n.ID != id || n.ShardID != "shardA" || n.Addr != "10.0.0.1:6379" {
		t.Fatalf("identity wrong: %+v", n)
	}
	if !n.IsPrimary() {
		t.Fatalf("role should be primary, got %q", n.Role)
	}
	if n.Offset != 500 {
		t.Fatalf("offset = %d, want 500", n.Offset)
	}
	if n.KnownNodes != 3 || n.ClusterSize != 3 || n.CurrentEpoch != 7 {
		t.Fatalf("cluster info fields: known=%d size=%d epoch=%d", n.KnownNodes, n.ClusterSize, n.CurrentEpoch)
	}
	if n.NumSlots() != 5462 {
		t.Fatalf("own slots = %d, want 5462", n.NumSlots())
	}
	if n.Client() != stub {
		t.Fatal("scraped node should retain its client")
	}
	// Peer table sees both nodes (failed-node / stale-node reasoning).
	if n.peers["2222222222222222222222222222222222222222"] == nil {
		t.Fatal("peer table missing the second node")
	}
}

func TestScrapeNodeIsolatedReplica(t *testing.T) {
	t.Parallel()
	id := "3333333333333333333333333333333333333333"
	stub := &scrapeStub{
		id:           id,
		shardID:      "shardB",
		info:         "# Replication\r\nrole:slave\r\nmaster_link_status:up\r\nslave_repl_offset:42\r\n",
		clusterInfo:  "cluster_state:fail\r\ncluster_slots_assigned:0\r\ncluster_known_nodes:1\r\n",
		clusterNodes: id + " 10.0.0.9:6379@16379 myself,slave - 0 0 0 connected\n",
	}
	n, err := ScrapeNode(context.Background(), "10.0.0.9:6379", stub)
	if err != nil {
		t.Fatalf("ScrapeNode error: %v", err)
	}
	if !n.IsReplica() || !n.LinkUp || n.Offset != 42 {
		t.Fatalf("replica fields wrong: role=%q link=%v off=%d", n.Role, n.LinkUp, n.Offset)
	}
	if !n.IsIsolated() {
		t.Fatal("fresh node with known_nodes=1 should be isolated")
	}
	if n.NumSlots() != 0 {
		t.Fatalf("isolated node should own 0 slots, got %d", n.NumSlots())
	}
}
