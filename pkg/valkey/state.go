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
	"slices"
	"strconv"
	"strings"
)

// Role is the semantic node role. Valkey's engine output uses the historical
// tokens role:master / role:slave and master / slave CLUSTER NODES flags; the
// operator maps those to the primary/replica vocabulary (05 §1). The live role
// is always derived from engine output, never from a node-index label.
type Role string

const (
	// RolePrimary is the semantic primary role (engine role:master / master flag).
	RolePrimary Role = "primary"
	// RoleReplica is the semantic replica role (engine role:slave / slave flag).
	RoleReplica Role = "replica"
)

// CLUSTER NODES flag tokens the operator reasons about (05 §1, §7). Unknown or
// future flags are preserved verbatim in NodeState.Flags but not interpreted.
const (
	flagMyself    = "myself"
	flagMaster    = "master"
	flagSlave     = "slave"
	flagFail      = "fail"
	flagPFailSoft = "fail?" // soft (gossip-suspected) failure
	flagPFail     = "pfail"
	flagHandshake = "handshake"
	flagNoAddr    = "noaddr"
)

// noPrimary is the placeholder Valkey emits in the master-id field of a primary
// line in CLUSTER NODES.
const noPrimary = "-"

// CLUSTER INFO / INFO replication field keys the operator reads (05 §2, §10).
const (
	infoKeyClusterState         = "cluster_state"
	infoKeyClusterSlotsAssigned = "cluster_slots_assigned"
	infoKeyClusterKnownNodes    = "cluster_known_nodes"
	infoKeyClusterSize          = "cluster_size"
	infoKeyClusterCurrentEpoch  = "cluster_current_epoch"
	infoKeyConnectedSlaves      = "connected_slaves"
	infoKeySlaveReplOffset      = "slave_repl_offset"
	infoKeyMasterReplOffset     = "master_repl_offset"
	// ClusterStateOK is the cluster_state value reported when all slots are
	// covered and the cluster is serving (05 §2).
	ClusterStateOK = "ok"
)

// NodeState is the operator's view of a single cluster node, built from one
// node's CLUSTER MYID / CLUSTER MYSHARDID / INFO replication / CLUSTER INFO /
// CLUSTER NODES scrape (05 §10). The live Role comes from INFO replication, not
// from any label.
type NodeState struct {
	// ID is the node's CLUSTER MYID (40-hex).
	ID string
	// ShardID is the node's CLUSTER MYSHARDID.
	ShardID string
	// Addr is the dial address <podIP>:6379 the operator connected to.
	Addr string
	// BusPort is the cluster-bus port from the ip:port@busport form (16379).
	BusPort int
	// Flags are the verbatim CLUSTER NODES flags of this node's "myself" line.
	Flags []string
	// PrimaryID is the primary's node ID for a replica, or "-" for a primary
	// (the master-id field of the myself line).
	PrimaryID string
	// ConfigEpoch is the node's config epoch from its CLUSTER NODES line.
	ConfigEpoch int64
	// LinkUp is true when a replica's master_link_status is "up".
	LinkUp bool
	// Role is the live semantic role from INFO replication (never the label).
	Role Role
	// Offset is slave_repl_offset for a replica or master_repl_offset for a
	// primary; -1 when unavailable.
	Offset int64
	// Slots are the slot ranges this node owns (empty for replicas / pending
	// primaries).
	Slots []SlotRange
	// KnownNodes is cluster_known_nodes from this node's CLUSTER INFO.
	KnownNodes int
	// ClusterSize is cluster_size (slot-owning primaries) from CLUSTER INFO.
	ClusterSize int
	// CurrentEpoch is cluster_current_epoch from CLUSTER INFO.
	CurrentEpoch int64
	// peers are the other nodes this node lists in its CLUSTER NODES output,
	// keyed by node ID. Used for failed-node detection and stale-node forgetting.
	peers map[string]*peerView
	// client is the live connection used for orchestration commands; nil for a
	// hand-built (test) NodeState.
	client ClusterClient
}

// peerView is the subset of a CLUSTER NODES peer line the operator needs: which
// nodes a given node believes are failed, and who it thinks each node's primary
// is. Built per scraped node so failure can be confirmed by gossip consensus.
type peerView struct {
	id        string
	flags     []string
	primaryID string
}

// Client returns the live orchestration client for this node, or nil when the
// NodeState was built without one (unit tests).
func (n *NodeState) Client() ClusterClient {
	return n.client
}

// IsPrimary reports whether this node's live role is primary.
func (n *NodeState) IsPrimary() bool {
	return n.Role == RolePrimary
}

// IsReplica reports whether this node's live role is replica.
func (n *NodeState) IsReplica() bool {
	return n.Role == RoleReplica
}

// NumSlots returns the total number of slots this node owns.
func (n *NodeState) NumSlots() int {
	return CountSlots(n.Slots)
}

// HasFlag reports whether this node's myself line carries the given flag.
func (n *NodeState) HasFlag(flag string) bool {
	return slices.Contains(n.Flags, flag)
}

// IsFailed reports whether this node's own myself line is flagged fail or fail?
// (05 §7 treats the soft fail? as failed so the operator can act under a
// majority-down partition where pfail can never promote to fail).
func (n *NodeState) IsFailed() bool {
	return n.HasFlag(flagFail) || n.HasFlag(flagPFailSoft)
}

// ShardState groups the nodes of one shard around a single primary (05 §1). It
// is a derived view over a ClusterState's nodes.
type ShardState struct {
	// ID is the shard ID (CLUSTER MYSHARDID) shared by the shard's nodes.
	ID string
	// PrimaryID is the live primary's node ID, or "" when no live primary is
	// known.
	PrimaryID string
	// Slots are the slot ranges the shard's primary owns.
	Slots []SlotRange
	// Nodes are all nodes belonging to the shard (primary + replicas).
	Nodes []*NodeState
}

// Primary returns the shard's live primary NodeState, or nil when absent.
func (s *ShardState) Primary() *NodeState {
	idx := slices.IndexFunc(s.Nodes, func(n *NodeState) bool { return n.ID == s.PrimaryID })
	if idx < 0 {
		return nil
	}
	return s.Nodes[idx]
}

// NumSlots returns the total number of slots the shard owns.
func (s *ShardState) NumSlots() int {
	return CountSlots(s.Slots)
}

// ClusterState is the assembled view of all scraped nodes (05 §3). It exposes
// both a flat Nodes slice (with byID/byAddr indexes) and a per-shard grouping
// (Shards + PendingNodes) the controller drives bootstrap/rebalance from.
type ClusterState struct {
	// Nodes is every successfully scraped node.
	Nodes []*NodeState
	// Shards groups slot-owning / attached nodes by shard ID.
	Shards []*ShardState
	// PendingNodes are primaries that own zero slots and have not yet joined a
	// shard (fresh bootstrap / scale-out nodes, 05 §3-§4).
	PendingNodes []*NodeState

	byID   map[string]*NodeState
	byAddr map[string]*NodeState
}

// NodeByID returns the scraped node with the given ID, or nil.
func (s *ClusterState) NodeByID(id string) *NodeState {
	return s.byID[id]
}

// NodeByAddr returns the scraped node at the given dial address, or nil.
func (s *ClusterState) NodeByAddr(addr string) *NodeState {
	return s.byAddr[addr]
}

// CloseClients closes every node's live connection. Call once per reconcile via
// defer (05 §10).
func (s *ClusterState) CloseClients() {
	if s == nil {
		return
	}
	for _, n := range s.Nodes {
		if n.client != nil {
			_ = n.client.Close()
		}
	}
}

// NewClusterState assembles a ClusterState from a set of scraped NodeStates,
// building the byID/byAddr indexes and the per-shard grouping. A primary with
// zero slots lands in PendingNodes (05 §3). nil entries are ignored.
func NewClusterState(nodes []*NodeState) *ClusterState {
	state := &ClusterState{
		Nodes:        make([]*NodeState, 0, len(nodes)),
		Shards:       make([]*ShardState, 0),
		PendingNodes: make([]*NodeState, 0),
		byID:         make(map[string]*NodeState, len(nodes)),
		byAddr:       make(map[string]*NodeState, len(nodes)),
	}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		state.Nodes = append(state.Nodes, n)
		if n.ID != "" {
			state.byID[n.ID] = n
		}
		if n.Addr != "" {
			state.byAddr[n.Addr] = n
		}
	}
	state.groupShards()
	return state
}

// groupShards populates Shards and PendingNodes from the flat Nodes slice.
func (s *ClusterState) groupShards() {
	for _, n := range s.Nodes {
		// A primary with no slots is pending (not yet part of a shard).
		if n.IsPrimary() && n.NumSlots() == 0 {
			s.PendingNodes = append(s.PendingNodes, n)
			continue
		}
		shard := s.shardFor(n.ShardID)
		shard.Nodes = append(shard.Nodes, n)
		if n.IsPrimary() {
			shard.PrimaryID = n.ID
			shard.Slots = n.Slots
		}
	}
}

// shardFor returns the ShardState for id, creating it if absent.
func (s *ClusterState) shardFor(id string) *ShardState {
	idx := slices.IndexFunc(s.Shards, func(sh *ShardState) bool { return sh.ID == id })
	if idx >= 0 {
		return s.Shards[idx]
	}
	shard := &ShardState{ID: id, Nodes: make([]*NodeState, 0, 2)}
	s.Shards = append(s.Shards, shard)
	return shard
}

// ParseClusterNodes parses raw CLUSTER NODES output into NodeStates keyed off
// each line's own perspective (one NodeState per line, with its peers attached).
// The format per line (05 §1) is:
//
//	<id> <ip:port@busport[,hostname]> <flags> <master-id> <ping> <pong> <epoch> <link> <slot>...
//
// Parsing is deliberately tolerant: lines with fewer than 8 fields are skipped,
// unknown/extra trailing fields are ignored, importing/migrating slot markers
// "[...]" are skipped, and an unparseable slot token drops only that node's
// slots rather than failing the whole parse. This is the CR-4 mitigation — a
// single malformed line must never corrupt the whole topology read.
func ParseClusterNodes(raw string) ([]*NodeState, error) {
	raw = strings.TrimPrefix(raw, "txt:")
	lines := strings.Split(raw, "\n")
	parsed := make([]parsedNodeLine, 0, len(lines))
	for _, line := range lines {
		if pl, ok := parseNodeLine(line); ok {
			parsed = append(parsed, pl)
		}
	}

	// Build the shared peer map once: every node line is a peer view.
	peers := make(map[string]*peerView, len(parsed))
	for i := range parsed {
		peers[parsed[i].id] = &peerView{
			id:        parsed[i].id,
			flags:     parsed[i].flags,
			primaryID: parsed[i].primaryID,
		}
	}

	out := make([]*NodeState, 0, len(parsed))
	for i := range parsed {
		n := parsed[i].toNodeState()
		n.peers = peers
		out = append(out, n)
	}
	return out, nil
}

// parsedNodeLine is the structured form of one CLUSTER NODES line before it is
// promoted to a NodeState.
type parsedNodeLine struct {
	id          string
	addr        string
	busPort     int
	flags       []string
	primaryID   string
	configEpoch int64
	slots       []SlotRange
}

// toNodeState promotes a parsedNodeLine to a NodeState, mapping the master/slave
// flag to the semantic Role (overridden later by the live INFO read when one is
// available).
func (p parsedNodeLine) toNodeState() *NodeState {
	role := RoleReplica
	if slices.Contains(p.flags, flagMaster) {
		role = RolePrimary
	}
	primary := p.primaryID
	if primary == noPrimary {
		primary = ""
	}
	return &NodeState{
		ID:          p.id,
		Addr:        p.addr,
		BusPort:     p.busPort,
		Flags:       p.flags,
		PrimaryID:   primary,
		ConfigEpoch: p.configEpoch,
		Role:        role,
		Offset:      -1,
		Slots:       p.slots,
	}
}

// parseNodeLine parses a single CLUSTER NODES line. ok is false when the line is
// blank or has too few fields to be a node entry.
func parseNodeLine(line string) (parsedNodeLine, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 8 {
		return parsedNodeLine{}, false
	}
	pl := parsedNodeLine{
		id:        fields[0],
		addr:      addrFromField(fields[1]),
		busPort:   busPortFromField(fields[1]),
		flags:     strings.Split(fields[2], ","),
		primaryID: fields[3],
	}
	if epoch, err := strconv.ParseInt(fields[6], 10, 64); err == nil {
		pl.configEpoch = epoch
	}
	// Slot tokens start at field 8. A malformed token drops only this node's
	// slots (tolerant parsing) rather than failing the whole read.
	if len(fields) > 8 {
		if ranges, err := parseSlotRanges(fields[8:]); err == nil {
			pl.slots = ranges
		}
	}
	return pl, true
}

// addrFromField extracts the dial address (<ip>:<port>) from the CLUSTER NODES
// address field "<ip>:<port>@<busport>[,hostname]". A handshake/noaddr node may
// carry an empty or ":0" address, which is returned as-is.
func addrFromField(field string) string {
	// Drop an optional ",hostname" suffix first.
	if comma := strings.IndexByte(field, ','); comma >= 0 {
		field = field[:comma]
	}
	if at := strings.IndexByte(field, '@'); at >= 0 {
		return field[:at]
	}
	return field
}

// busPortFromField extracts the cluster-bus port from the "@<busport>" part of
// the address field, or 0 when absent/unparseable.
func busPortFromField(field string) int {
	if comma := strings.IndexByte(field, ','); comma >= 0 {
		field = field[:comma]
	}
	at := strings.IndexByte(field, '@')
	if at < 0 {
		return 0
	}
	port, err := strconv.Atoi(field[at+1:])
	if err != nil {
		return 0
	}
	return port
}

// ParseClusterInfo parses CLUSTER INFO output, returning the cluster_state
// string, the count of assigned slots, and the count of known nodes. Missing or
// non-numeric fields default to "" / 0; parsing never fails.
func ParseClusterInfo(raw string) (clusterState string, slotsAssigned, knownNodes int) {
	m := infoToMap(raw)
	clusterState = m[infoKeyClusterState]
	slotsAssigned = atoiOrZero(m[infoKeyClusterSlotsAssigned])
	knownNodes = atoiOrZero(m[infoKeyClusterKnownNodes])
	return clusterState, slotsAssigned, knownNodes
}

// ParseInfoReplicationTyped parses INFO replication into the typed fields the
// operator needs: live role, replica link state, replication offset, and the
// connected_slaves count. The live role mapping is role:master -> RolePrimary,
// role:slave -> RoleReplica (05 §1, §10).
func ParseInfoReplicationTyped(raw string) (role Role, linkUp bool, offset int64, connectedSlaves int) {
	m := ParseInfoReplication(raw)
	role = RoleReplica
	if m[InfoKeyRole] == InfoRoleMaster {
		role = RolePrimary
		offset = atoi64OrNeg(m[infoKeyMasterReplOffset])
	} else {
		offset = atoi64OrNeg(m[infoKeySlaveReplOffset])
	}
	linkUp = m[InfoKeyMasterLinkStatus] == "up"
	connectedSlaves = atoiOrZero(m[infoKeyConnectedSlaves])
	return role, linkUp, offset, connectedSlaves
}

// infoToMap converts INFO / CLUSTER INFO output (key:value lines, CRLF) into a
// map, skipping comment headers and blank lines. Tolerant of stray lines.
func infoToMap(raw string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimPrefix(raw, "txt:"), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, val, ok := strings.Cut(line, ":"); ok {
			m[strings.TrimSpace(key)] = strings.TrimSpace(val)
		}
	}
	return m
}

// atoiOrZero parses s as an int, returning 0 on any error.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// atoi64OrNeg parses s as an int64, returning -1 on any error (the "unknown
// offset" sentinel used in replica selection).
func atoi64OrNeg(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}
