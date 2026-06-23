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

package perconavalkeycluster_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// fakeCluster is a stateful in-memory Valkey-cluster simulator shared by every
// fakeNode. It progresses across reconciles exactly as a real cluster would:
// nodes start isolated (cluster_known_nodes=1, zero slots), become met (gossip
// converges, known_nodes = N), then own slots (ADDSLOTSRANGE) and attach as
// replicas (REPLICATE). It records the MEET/ADDSLOTSRANGE/REPLICATE call order
// + args so a test can assert the strict MEET->ADDSLOTSRANGE->REPLICATE
// invariant and the even slot split (CR-6: no double-issue, no ADDSLOTS on a
// busy slot). CR-18: there is no real engine under envtest.
type fakeCluster struct {
	mu sync.Mutex

	// nodes is keyed by dial address (<podIP>:6379).
	nodes map[string]*fakeNodeState

	// calls records the ordered command log for assertions.
	calls []recordedCall

	// metPairs tracks which (a,b) MEETs were issued (idempotency check).
	metPairs map[string]bool
}

// fakeNodeState is one simulated node's view.
type fakeNodeState struct {
	id      string
	shardID string
	addr    string
	host    string
	// known is the set of node IDs this node's gossip table knows (incl. self).
	known map[string]bool
	// slots are the assigned ranges this node owns (primary only).
	slots []valkey.SlotRange
	// primaryID is set when this node has REPLICATEd a primary.
	primaryID string
	role      valkey.Role
	// migrations is this node's CLUSTER GETSLOTMIGRATIONS log (terminal entries
	// for completed atomic moves; a non-terminal entry blocks a fresh move).
	migrations []valkey.SlotMigration
	// failed marks the node hard-failed in gossip (its peers see fail/fail?),
	// simulating a lost primary for the recovery specs.
	failed bool
}

// recordedCall is one issued orchestration command, in order.
type recordedCall struct {
	cmd  string // MEET, ADDSLOTSRANGE, REPLICATE, SETEPOCH
	addr string // issuing node
	arg  string // target/peer/ranges
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{
		nodes:    map[string]*fakeNodeState{},
		metPairs: map[string]bool{},
	}
}

// register adds a node to the simulator keyed by its address, deriving a stable
// per-node id/shardid. It is called from ForNode the first time a podIP dials.
// shard/idx are accepted for signature symmetry but unused: a fresh node is
// always a primary of its own shard until CLUSTER REPLICATE, and the live role
// is derived from the engine, not node-index.
func (fc *fakeCluster) register(addr string, _, _ int) *fakeNodeState {
	if n, ok := fc.nodes[addr]; ok {
		return n
	}
	id := fmt.Sprintf("%040x", len(fc.nodes)+1)
	// A freshly-started node is ALWAYS a primary of its own (empty) shard until
	// it issues CLUSTER REPLICATE — matching real Valkey, where node-index alone
	// never determines the live role (the operator reads it from INFO). The
	// shard id is per-node initially; a replica adopts its primary's shard id on
	// REPLICATE.
	n := &fakeNodeState{
		id:      id,
		shardID: fmt.Sprintf("shard-%s", id),
		addr:    addr,
		host:    strings.TrimSuffix(addr, ":6379"),
		known:   map[string]bool{id: true},
		role:    valkey.RolePrimary,
	}
	fc.nodes[addr] = n
	return n
}

// knownCount returns this node's cluster_known_nodes.
func (n *fakeNodeState) knownCount() int { return len(n.known) }

// totalAssignedSlots returns the slots owned across the whole simulated cluster.
func (fc *fakeCluster) totalAssignedSlots() int {
	total := 0
	for _, n := range fc.nodes {
		total += valkey.CountSlots(n.slots)
	}
	return total
}

// callsOfType returns the recorded calls of a given command type, in order.
func (fc *fakeCluster) callsOfType(cmd string) []recordedCall {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var out []recordedCall
	for _, c := range fc.calls {
		if c.cmd == cmd {
			out = append(out, c)
		}
	}
	return out
}

// firstIndexOf returns the index of the first recorded call of cmd, or -1.
func (fc *fakeCluster) firstIndexOf(cmd string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for i, c := range fc.calls {
		if c.cmd == cmd {
			return i
		}
	}
	return -1
}

// lastIndexOf returns the index of the last recorded call of cmd, or -1.
func (fc *fakeCluster) lastIndexOf(cmd string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	last := -1
	for i, c := range fc.calls {
		if c.cmd == cmd {
			last = i
		}
	}
	return last
}

// ---- fakeClientFactory + fakeNode (the valkey seams) -----------------------

// fakeClientFactory is the injected valkey.ClusterClientFactory. ForNode
// registers the node in the simulator (by its labels) and returns a fakeNode
// bound to the shared fakeCluster.
type fakeClientFactory struct {
	fc *fakeCluster
}

func (f *fakeClientFactory) ForNode(_ context.Context, node *valkeyv1alpha1.ValkeyNode) (string, valkey.ClusterClient, error) {
	addr := node.Status.PodIP + ":6379"
	shard := atoiLabel(node.Labels["valkey.percona.com/shard-index"])
	idx := atoiLabel(node.Labels["valkey.percona.com/node-index"])
	f.fc.mu.Lock()
	f.fc.register(addr, shard, idx)
	f.fc.mu.Unlock()
	return addr, &fakeNode{
		fc:        f.fc,
		addr:      addr,
		cluster:   node.Labels["valkey.percona.com/cluster"],
		namespace: node.Namespace,
	}, nil
}

// fakeNode is a valkey.ClusterClient bound to one simulated node.
type fakeNode struct {
	fc        *fakeCluster
	addr      string
	cluster   string
	namespace string
}

func (n *fakeNode) self() *fakeNodeState { return n.fc.nodes[n.addr] }

func (n *fakeNode) ClusterMyID(context.Context) (string, error) {
	return n.self().id, nil
}
func (n *fakeNode) ClusterMyShardID(context.Context) (string, error) {
	return n.self().shardID, nil
}

func (n *fakeNode) ClusterInfo(context.Context) (string, error) {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	s := n.self()
	state := "fail"
	if n.fc.totalAssignedSlots() >= valkey.TotalSlots {
		state = "ok"
	}
	// cluster_size = count of slot-owning primaries.
	size := 0
	for _, nd := range n.fc.nodes {
		if valkey.CountSlots(nd.slots) > 0 {
			size++
		}
	}
	return fmt.Sprintf("cluster_state:%s\r\ncluster_slots_assigned:%d\r\ncluster_known_nodes:%d\r\ncluster_size:%d\r\ncluster_current_epoch:%d\r\n",
		state, n.fc.totalAssignedSlots(), s.knownCount(), size, len(n.fc.nodes)), nil
}

func (n *fakeNode) ClusterNodes(context.Context) (string, error) {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	self := n.self()
	// Emit one line per node this node knows about (gossip view).
	ids := make([]string, 0, len(self.known))
	for id := range self.known {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var b strings.Builder
	for _, id := range ids {
		nd := n.fc.byID(id)
		if nd == nil {
			continue
		}
		flags := "master"
		master := "-"
		if nd.role == valkey.RoleReplica {
			flags = "slave"
			if nd.primaryID != "" {
				master = nd.primaryID
			}
		}
		if nd.failed {
			flags += ",fail"
		}
		if nd.id == self.id {
			flags = "myself," + flags
		}
		slotStr := ""
		for _, r := range nd.slots {
			slotStr += " " + r.String()
		}
		// <id> <ip:port@bus> <flags> <master> <ping> <pong> <epoch> <link> <slots...>
		fmt.Fprintf(&b, "%s %s:6379@16379 %s %s 0 0 0 connected%s\n",
			nd.id, nd.host, flags, master, slotStr)
	}
	return b.String(), nil
}

func (n *fakeNode) Info(_ context.Context, _ string) (string, error) {
	self := n.self()
	role := "master"
	link := ""
	if self.role == valkey.RoleReplica {
		role = "slave"
		link = "master_link_status:up\r\n"
	}
	return fmt.Sprintf("# Replication\r\nrole:%s\r\n%smaster_repl_offset:100\r\nslave_repl_offset:100\r\n", role, link), nil
}

// InfoReplication / ConfigSet / Ping satisfy the embedded ConfigClient.
func (n *fakeNode) InfoReplication(ctx context.Context) (map[string]string, error) {
	raw, _ := n.Info(ctx, "replication")
	return valkey.ParseInfoReplication(raw), nil
}

// ConfigSet records the CONFIG SET so a test can assert the live auth reload
// issued CONFIG SET masterauth on a real change (and not on a no-op). The key is
// recorded as the arg so callsOfType("CONFIGSET") can be filtered by key downstream.
func (n *fakeNode) ConfigSet(_ context.Context, key, value string) error {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	n.record("CONFIGSET", key+"="+value)
	return nil
}
func (n *fakeNode) Ping(context.Context) error { return nil }
func (n *fakeNode) Close() error               { return nil }

// ACLLoad records the ACL LOAD so a test can assert the in-place auth reload
// reloaded the aclfile live on a real ACL change (and never on a no-op reconcile).
func (n *fakeNode) ACLLoad(context.Context) error {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	n.record("ACLLOAD", "")
	return nil
}

// ACLList models the engine's currently-loaded ACL by returning the lines of the
// cluster's rendered internal-<cluster>-acl Secret — i.e. zero Secret-mount
// propagation lag, the normal steady state once the kubelet has projected the
// file. The operator's liveReloadAuth verifies the loaded ACL against the
// rendered content via this call; modelling it as always-current keeps the
// happy-path reload deterministic under envtest (the real-cluster propagation
// race is covered by the parseACLUsers/nodeACLMatches unit tests).
func (n *fakeNode) ACLList(ctx context.Context) ([]string, error) {
	if n.cluster == "" {
		return nil, nil
	}
	sec := &corev1.Secret{}
	key := types.NamespacedName{Namespace: n.namespace, Name: naming.ACLSecretName(n.cluster)}
	if err := k8sClient.Get(ctx, key, sec); err != nil {
		// No rendered ACL yet (pre-bootstrap): report nothing loaded.
		return nil, nil //nolint:nilerr // absence is "nothing loaded", not a failure
	}
	body := string(sec.Data[valkey.ACLFileKey])
	return strings.Split(strings.TrimRight(body, "\n"), "\n"), nil
}

func (n *fakeNode) ClusterSetConfigEpoch(_ context.Context, _ int64) error {
	n.record("SETEPOCH", "")
	return nil
}

// ClusterMeet introduces ip into this node's gossip and (simulating gossip
// convergence) makes the whole connected component learn every member.
func (n *fakeNode) ClusterMeet(_ context.Context, ip string, _, _ int) error {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	n.record("MEET", ip)
	peerAddr := ip + ":6379"
	peer := n.fc.nodes[peerAddr]
	self := n.self()
	if peer == nil || self == nil {
		return nil
	}
	n.fc.metPairs[self.id+"->"+peer.id] = true
	// Simulate gossip: union the known sets of every node reachable from either.
	n.fc.gossipConverge(self, peer)
	return nil
}

func (n *fakeNode) ClusterAddSlotsRange(_ context.Context, ranges []valkey.SlotRange) error {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	self := n.self()
	// CR-6: reject ADDSLOTSRANGE on a node that already owns slots (slot busy).
	if valkey.CountSlots(self.slots) > 0 {
		return fmt.Errorf("ERR Slot is already busy")
	}
	self.slots = append(self.slots, ranges...)
	n.record("ADDSLOTSRANGE", valkey.FormatSlotRanges(ranges))
	return nil
}

func (n *fakeNode) ClusterReplicate(_ context.Context, primaryID string) error {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	self := n.self()
	if n.fc.byID(primaryID) == nil {
		return fmt.Errorf("ERR Unknown node %s", primaryID)
	}
	self.role = valkey.RoleReplica
	self.primaryID = primaryID
	// Adopt the primary's shard id (a replica shares its primary's shard).
	if p := n.fc.byID(primaryID); p != nil {
		self.shardID = p.shardID
	}
	n.record("REPLICATE", primaryID)
	return nil
}

// ClusterMigrateSlots atomically moves ranges off the issuing (source) node onto
// dstID, reflecting the new ownership in CLUSTER NODES (Wave 2b scale-out/in).
// It records a terminal migration on the source so a subsequent GETSLOTMIGRATIONS
// observes the just-completed move (mirroring the engine's terminal state), and
// enforces the CR-6 guards: the destination must be gossip-visible, and a
// non-terminal in-flight migration blocks a fresh one. The atomic flip is modeled
// as a single-step ownership transfer (no half-migrated window, 05 §4).
func (n *fakeNode) ClusterMigrateSlots(_ context.Context, ranges []valkey.SlotRange, dstID string) error {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	src := n.self()
	dst := n.fc.byID(dstID)
	if dst == nil || !src.known[dstID] {
		return fmt.Errorf("ERR Unknown node %s", dstID)
	}
	// CR-6: never re-issue while a migration is still in flight on the source.
	for _, m := range src.migrations {
		if !m.IsTerminal() {
			return fmt.Errorf("ERR a slot migration is already in progress")
		}
	}
	moving := slotSet(ranges)
	owned := slotSet(src.slots)
	for s := range moving {
		if !owned[s] {
			return fmt.Errorf("ERR slots are not served by this node")
		}
	}
	// Atomic flip: source loses the slots, destination gains them.
	src.slots = setToRanges(subtractSet(owned, moving))
	dst.slots = setToRanges(unionSet(slotSet(dst.slots), moving))
	src.migrations = append(src.migrations, valkey.SlotMigration{State: "success", NodeID: dstID})
	n.record("MIGRATESLOTS", valkey.FormatSlotRanges(ranges)+"->"+dstID)
	return nil
}

func (n *fakeNode) ClusterGetSlotMigrations(context.Context) ([]valkey.SlotMigration, error) {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	return append([]valkey.SlotMigration(nil), n.self().migrations...), nil
}

// ClusterForget drops nodeID from the issuing node's gossip table. Because the
// controller broadcasts FORGET to every surviving node (broadcastForget), a
// fully-forgotten node leaves the cluster: the simulator therefore also evicts it
// from any already-deleted (unscraped) peers' gossip tables and from the node map
// so knownByPeers reflects the post-forget cluster — a deleted scale-in remnant
// (which the controller can no longer reach to issue a per-node FORGET) does not
// keep its likewise-deleted shardmate visible in gossip. An unknown node is
// benign (already gone), matching the engine. Wave 2b scale-in / stale-node
// cleanup (05 §5, §7).
func (n *fakeNode) ClusterForget(_ context.Context, nodeID string) error {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	n.record("FORGET", nodeID)
	// Remove nodeID from every node's gossip view and evict it from the cluster
	// (the node map is keyed by dial address, so locate it by id).
	for _, nd := range n.fc.nodes {
		delete(nd.known, nodeID)
	}
	for addr, nd := range n.fc.nodes {
		if nd.id == nodeID {
			delete(n.fc.nodes, addr)
		}
	}
	return nil
}

// ClusterFailover flips roles on the issuing (replica) node. Graceful/FORCE/
// TAKEOVER all promote the issuing replica to primary, demote its old primary to
// a replica of the new primary, and transfer slot ownership — modeling the atomic
// role swap (05 §6-§7). The mode is recorded so a test can assert FORCE vs
// TAKEOVER (CR-5).
func (n *fakeNode) ClusterFailover(_ context.Context, mode valkey.FailoverMode) error {
	n.fc.mu.Lock()
	defer n.fc.mu.Unlock()
	self := n.self()
	n.record("FAILOVER", string(mode))
	if self.role != valkey.RoleReplica || self.primaryID == "" {
		return fmt.Errorf("ERR FAILOVER can only be sent to a replica")
	}
	old := n.fc.byID(self.primaryID)
	if old == nil {
		return fmt.Errorf("ERR cannot find primary")
	}
	// Promote self: take the old primary's slots and become primary of the shard.
	self.role = valkey.RolePrimary
	self.slots = append(self.slots, old.slots...)
	self.primaryID = ""
	// Demote the old primary to a replica of the new primary (when still alive).
	old.slots = nil
	old.role = valkey.RoleReplica
	old.primaryID = self.id
	return nil
}

// record appends a call to the shared log (caller holds fc.mu for state-mutating
// commands; SETEPOCH/MEET take it themselves).
func (n *fakeNode) record(cmd, arg string) {
	n.fc.calls = append(n.fc.calls, recordedCall{cmd: cmd, addr: n.addr, arg: arg})
}

// byID returns the node with the given id (caller holds fc.mu).
func (fc *fakeCluster) byID(id string) *fakeNodeState {
	for _, n := range fc.nodes {
		if n.id == id {
			return n
		}
	}
	return nil
}

// gossipConverge unions the known sets across the connected component of a and
// b, simulating gossip propagation so a subsequent scrape sees known_nodes=N
// for every met node (caller holds fc.mu).
func (fc *fakeCluster) gossipConverge(a, b *fakeNodeState) {
	component := map[string]bool{}
	for id := range a.known {
		component[id] = true
	}
	for id := range b.known {
		component[id] = true
	}
	// Apply the union to every node already in the component.
	for _, n := range fc.nodes {
		if component[n.id] {
			for id := range component {
				n.known[id] = true
			}
		}
	}
}

// ---- recovery / scale test controls -------------------------------------

// failPrimary marks the slot-owning primary at the given podIP hard-failed
// (gossip fail flag) so the recovery specs can drive a lost-primary scenario. The
// node keeps its slots (a failed primary still owns them in gossip) so cluster
// quorum math sees it in the denominator. The podIP is keyed to its dial address.
func (fc *fakeCluster) failPrimary(podIP string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if n := fc.nodes[podIP+":6379"]; n != nil {
		n.failed = true
	}
}

// addStaleGossipNode injects a stale node into the simulator that is present in
// every node's gossip table but has NO backing ValkeyNode (a scale-in remnant or
// permanently-dead pod the operator should CLUSTER FORGET). The stale node lives
// at an address (1.2.3.4) that no ValkeyNode's podIP matches, so backingNodeIDs
// classifies it as backing-less. Returns the injected id.
func (fc *fakeCluster) addStaleGossipNode() string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	id := fmt.Sprintf("%040x", 0xdead0000+len(fc.nodes))
	staleAddr := "1.2.3.4:6379"
	fc.nodes[staleAddr] = &fakeNodeState{
		id:      id,
		shardID: "stale-" + id,
		addr:    staleAddr,
		host:    "1.2.3.4",
		known:   map[string]bool{id: true},
		role:    valkey.RolePrimary,
	}
	// Make every existing node know about the stale node (gossip-visible) so it
	// appears as a peer in their CLUSTER NODES output.
	for _, n := range fc.nodes {
		n.known[id] = true
	}
	return id
}

// knownByPeers reports whether id is still present in any OTHER node's gossip
// table (a node trivially knows itself, so its own entry is ignored). A test
// asserts a cluster-wide FORGET removed id from every peer's table.
func (fc *fakeCluster) knownByPeers(id string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, n := range fc.nodes {
		if n.id == id {
			continue // a node always knows itself.
		}
		if n.known[id] {
			return true
		}
	}
	return false
}

// roleAt returns the live role of the node at the given dial address.
func (fc *fakeCluster) roleAt(addr string) valkey.Role {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if n := fc.nodes[addr]; n != nil {
		return n.role
	}
	return ""
}

// primarySlotCounts returns the slot count of every slot-owning primary, for the
// scale-out balance assertion.
func (fc *fakeCluster) primarySlotCounts() []int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var counts []int
	for _, n := range fc.nodes {
		if c := valkey.CountSlots(n.slots); c > 0 {
			counts = append(counts, c)
		}
	}
	return counts
}

// idsForShard returns the live node IDs of the nodes in the given shard — used to
// assert those IDs are later FORGOTten on scale-in. Nodes are matched by their
// deterministic podIP (10.<shard>.<node>.10, see promoteNodes).
func (fc *fakeCluster) idsForShard(shard int) []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var ids []string
	for _, n := range fc.nodes {
		if n.host == fmt.Sprintf("10.%d.0.10", shard) || n.host == fmt.Sprintf("10.%d.1.10", shard) {
			ids = append(ids, n.id)
		}
	}
	return ids
}

// slotSet expands ranges into a set of individual slots.
func slotSet(ranges []valkey.SlotRange) map[int]bool {
	out := map[int]bool{}
	for _, r := range ranges {
		for s := r.Start; s <= r.End; s++ {
			out[s] = true
		}
	}
	return out
}

// setToRanges collapses a slot set into the minimal contiguous ranges.
func setToRanges(set map[int]bool) []valkey.SlotRange {
	slots := make([]int, 0, len(set))
	for s := range set {
		slots = append(slots, s)
	}
	if len(slots) == 0 {
		return nil
	}
	return valkey.SlotsToRanges(slots)
}

// subtractSet returns a \ b.
func subtractSet(a, b map[int]bool) map[int]bool {
	out := map[int]bool{}
	for s := range a {
		if !b[s] {
			out[s] = true
		}
	}
	return out
}

// unionSet returns a ∪ b.
func unionSet(a, b map[int]bool) map[int]bool {
	out := map[int]bool{}
	for s := range a {
		out[s] = true
	}
	for s := range b {
		out[s] = true
	}
	return out
}

// atoiLabel parses an integer label value, defaulting to 0.
func atoiLabel(s string) int {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int(c-'0')
	}
	return v
}
