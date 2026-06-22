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
)

// ScrapeTarget is one node to scrape: its dial address (<podIP>:6379) and a
// connected ClusterClient. The controller resolves auth/TLS and dials each
// target via its injected ClientFactory; this file owns the protocol scrape
// itself (one DoMulti-equivalent batch of read commands per node) so the
// parsing stays in pkg/valkey (05 §10).
type ScrapeTarget struct {
	// Addr is the dial address <podIP>:6379 used to connect Client.
	Addr string
	// Client is the live single-node connection for this target.
	Client ClusterClient
}

// ScrapeNode reads one node's CLUSTER MYID / CLUSTER MYSHARDID / INFO
// replication / CLUSTER INFO / CLUSTER NODES and assembles a NodeState. The
// live Role / offset / link state come from INFO replication; the flags, slots,
// primaryID and config epoch come from this node's own "myself" line in
// CLUSTER NODES (05 §10). The client is retained on the NodeState so the
// controller can issue orchestration commands against it and CloseClients()
// later. A read error from any single command fails the scrape for that node;
// the caller skips a failed node rather than aborting the whole state build
// (one dead node must not blind the operator to the rest, CR-mitigation).
func ScrapeNode(ctx context.Context, addr string, c ClusterClient) (*NodeState, error) {
	id, err := c.ClusterMyID(ctx)
	if err != nil {
		return nil, err
	}
	shardID, err := c.ClusterMyShardID(ctx)
	if err != nil {
		return nil, err
	}
	infoRepl, err := c.Info(ctx, "replication")
	if err != nil {
		return nil, err
	}
	clusterInfo, err := c.ClusterInfo(ctx)
	if err != nil {
		return nil, err
	}
	clusterNodes, err := c.ClusterNodes(ctx)
	if err != nil {
		return nil, err
	}

	n := nodeStateFromScrape(id, shardID, addr, infoRepl, clusterInfo, clusterNodes)
	n.client = c
	return n, nil
}

// nodeStateFromScrape builds a NodeState purely from raw engine output (pure,
// testable without a live client). It locates this node's own line in CLUSTER
// NODES by the id, layering the live INFO-replication role/offset/link over the
// parsed myself line and attaching every peer line as a peerView.
func nodeStateFromScrape(id, shardID, addr, infoRepl, clusterInfo, clusterNodes string) *NodeState {
	_, _, knownNodes := ParseClusterInfo(clusterInfo)
	clusterSize, currentEpoch := parseClusterSizeEpoch(clusterInfo)
	role, linkUp, offset, _ := ParseInfoReplicationTyped(infoRepl)

	// ParseClusterNodes yields one NodeState per line (with the shared peer
	// table attached). Find this node's own line for flags/slots/primaryID/
	// epoch/bus port; everything else is overlaid from the live INFO read.
	parsed, _ := ParseClusterNodes(clusterNodes)
	n := &NodeState{
		ID:           id,
		ShardID:      shardID,
		Addr:         addr,
		Role:         role,
		LinkUp:       linkUp,
		Offset:       offset,
		KnownNodes:   knownNodes,
		ClusterSize:  clusterSize,
		CurrentEpoch: currentEpoch,
	}
	for _, p := range parsed {
		if n.peers == nil {
			n.peers = p.peers
		}
		if p.ID == id {
			n.BusPort = p.BusPort
			n.Flags = p.Flags
			n.PrimaryID = p.PrimaryID
			n.ConfigEpoch = p.ConfigEpoch
			n.Slots = p.Slots
		}
	}
	return n
}

// parseClusterSizeEpoch reads cluster_size and cluster_current_epoch from
// CLUSTER INFO, defaulting to 0 when absent.
func parseClusterSizeEpoch(clusterInfo string) (size int, epoch int64) {
	m := infoToMap(clusterInfo)
	size = atoiOrZero(m[infoKeyClusterSize])
	epoch = atoi64OrNeg(m[infoKeyClusterCurrentEpoch])
	if epoch < 0 {
		epoch = 0
	}
	return size, epoch
}
