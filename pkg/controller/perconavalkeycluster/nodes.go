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
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// nodeKey identifies a desired ValkeyNode position.
type nodeKey struct {
	shard int
	node  int
}

// listClusterNodes lists every ValkeyNode of this cluster (label
// valkey.percona.com/cluster=<cluster>). 04 §2.1 step5.
func (r *Reconciler) listClusterNodes(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
) (*valkeyv1alpha1.ValkeyNodeList, error) {
	nodes := &valkeyv1alpha1.ValkeyNodeList{}
	err := r.List(ctx, nodes,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(naming.ClusterSelector(cluster.Name)))
	return nodes, err
}

// desiredNodes returns the desired (shard, node) positions in shard order,
// replicas-before-primary WITHIN each shard (node index 1..replicas first, then
// node index 0). Replicas-before-primary is the safe roll/create order (04 §2.1
// step6, 05 §3). Wave 2a uses node-index ordering on the create path; the
// live-role-aware reordering for rolling a post-failover primary is GO-3.16
// (Wave 2b).
func desiredNodes(cluster *valkeyv1alpha1.PerconaValkeyCluster) []nodeKey {
	shards := int(cluster.Spec.Shards)
	replicas := int(cluster.Spec.Replicas)
	keys := make([]nodeKey, 0, shards*(1+replicas))
	for shard := 0; shard < shards; shard++ {
		for node := 1; node <= replicas; node++ {
			keys = append(keys, nodeKey{shard: shard, node: node})
		}
		keys = append(keys, nodeKey{shard: shard, node: 0})
	}
	return keys
}

// reconcileValkeyNodes walks the desired (shard, node) positions in
// replicas-before-primary order and reconciles EXACTLY ONE node per call:
// creating a missing ValkeyNode, or stamping a changed serverConfigHash/image to
// trigger a roll. It returns requeue=true until the touched node reports
// status.ready (and has observed its current generation); ready/unchanged nodes
// are skipped. 04 §2.1 step6 / GO-3.10, GO-3.16.
//
// Roll discipline (GO-3.16, 05 §6): a node whose desired spec differs is rolled
// one at a time, replicas-before-primary by LIVE role. Before rolling a node that
// is the live PRIMARY of its shard, a graceful CLUSTER FAILOVER is performed to a
// synced replica and the role flip awaited (proactiveFailover); if no synced
// replica exists the roll is DEFERRED (never FORCE a live primary). The roll is
// also held while shouldGateEngineRoll reports a backup/restore gate (M4/M6 seam).
func (r *Reconciler) reconcileValkeyNodes(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	nodes *valkeyv1alpha1.ValkeyNodeList, configHash string,
) (bool, error) {
	log := logf.FromContext(ctx)
	existing := indexNodesByName(nodes)

	// First pass: create any missing node, one per reconcile (creates precede
	// rolls — a half-built cluster is completed before any roll begins).
	for _, key := range desiredNodes(cluster) {
		name := naming.NodeName(cluster.Name, key.shard, key.node)
		if _, ok := existing[name]; ok {
			continue
		}
		return true, r.createValkeyNode(ctx, cluster, key, configHash)
	}

	// Second pass: roll one node whose spec changed, replicas-before-primary by
	// LIVE role, with proactive failover before a live primary (GO-3.16). Live
	// state is scraped only when a roll is actually pending so a steady pass never
	// scrapes here (the steady scrape is phase 7).
	if r.anyRollPending(cluster, nodes, configHash) {
		return r.rollNextNode(ctx, cluster, nodes, configHash)
	}

	// Third pass: wait on any not-yet-ready node (no spec change pending).
	for _, key := range desiredNodes(cluster) {
		name := naming.NodeName(cluster.Name, key.shard, key.node)
		if current, ok := existing[name]; ok && !nodeConverged(current, cluster, configHash) {
			log.V(1).Info("ValkeyNode not yet ready, waiting", "node", name)
			return true, nil
		}
	}
	return false, nil
}

// createValkeyNode creates one missing ValkeyNode, writing the full parent->node
// spec contract and emitting ValkeyNodeCreated. One effect per reconcile.
func (r *Reconciler) createValkeyNode(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, key nodeKey, configHash string,
) error {
	name := naming.NodeName(cluster.Name, key.shard, key.node)
	desired := buildValkeyNodeSpec(cluster, key, configHash)
	node := &valkeyv1alpha1.ValkeyNode{}
	node.Name, node.Namespace = name, cluster.Namespace
	res, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, node, func() error {
		node.Labels = desired.Labels
		node.Spec = desired.Spec
		return nil
	})
	if err != nil {
		return err
	}
	if res == controllerutil.OperationResultCreated {
		r.recorder.Eventf(cluster, node, eventNormal, EventValkeyNodeCreated, "CreateValkeyNode",
			"Created ValkeyNode %s (shard %d node %d)", name, key.shard, key.node)
	}
	return nil
}

// anyRollPending reports whether any existing node carries a stale roll-relevant
// spec (a roll is pending somewhere). Used to decide whether to scrape live state
// for the roll path.
func (r *Reconciler) anyRollPending(
	cluster *valkeyv1alpha1.PerconaValkeyCluster, nodes *valkeyv1alpha1.ValkeyNodeList, configHash string,
) bool {
	for i := range nodes.Items {
		if nodeNeedsRoll(&nodes.Items[i], cluster, configHash) {
			return true
		}
	}
	return false
}

// rollNextNode rolls exactly one node whose spec changed. It orders roll
// candidates replicas-before-primary by LIVE role (read from a fresh scrape, not
// the node-index label, since a post-failover primary may not be node-index 0),
// performs proactive failover before rolling a live primary, and honours the
// backup/restore roll gate. One roll per reconcile.
func (r *Reconciler) rollNextNode(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	nodes *valkeyv1alpha1.ValkeyNodeList, configHash string,
) (bool, error) {
	log := logf.FromContext(ctx)

	// Backup-running / restore-pause gate (M4/M6 seam): hold the roll so pod churn
	// cannot corrupt an in-flight snapshot (04 §4.2).
	if r.shouldGateEngineRoll(ctx, cluster) {
		setCondition(cluster, CondProgressing, metav1.ConditionTrue, ReasonReconciling, "roll gated by backup/restore")
		log.V(1).Info("engine roll gated; deferring all rolls")
		return true, nil
	}

	// Wait for the previously-rolled node to settle before touching the next one
	// (one node at a time): if any node is mid-roll (stamped but not ready), hold.
	if mid := midRollNode(cluster, nodes, configHash); mid != nil {
		log.V(1).Info("waiting for in-flight roll to settle", "node", mid.Name)
		return true, nil
	}

	state := r.getValkeyClusterState(ctx, nodes)
	if state != nil {
		defer state.CloseClients()
	}

	// Smart-update gate + engine-downgrade policy (M6 GO-6.8/6.12 seam): evaluate
	// the failover-aware engine-upgrade gate (Ready + slots=16384 + replicas synced
	// + no backup running) and refuse an unsafe engine downgrade BEFORE rolling any
	// node. While gated the roll is held (requeue, no data-pod churn). The stub
	// permits the roll so the existing M3 ordering/failover path runs unchanged.
	if allowed, reason := r.reconcileSmartUpdate(ctx, cluster, state); !allowed {
		setCondition(cluster, CondProgressing, metav1.ConditionTrue, ReasonReconciling,
			"engine roll deferred: "+reason)
		log.V(1).Info("smart-update gate holding engine roll", "reason", reason)
		return true, r.writeStatus(ctx, cluster)
	}

	target := nextRollCandidate(cluster, nodes, state, configHash)
	if target == nil {
		return true, nil // nothing rollable yet (e.g. live role unknown); requeue.
	}
	// rollNode always wants the caller to requeue (it either deferred on a
	// failover or stamped the roll), so the result is always (true, err).
	return true, r.rollNode(ctx, cluster, target, state, configHash)
}

// nodeNeedsRoll reports whether an existing node's desired roll-relevant spec
// (serverConfigHash or image) differs from what it currently carries — i.e. a
// rolling restart is pending for it (05 §6 / 04 §11).
func nodeNeedsRoll(node *valkeyv1alpha1.ValkeyNode, cluster *valkeyv1alpha1.PerconaValkeyCluster, configHash string) bool {
	return node.Spec.ServerConfigHash != configHash || node.Spec.Image != cluster.Spec.Image
}

// rollNode rolls a single existing ValkeyNode whose spec changed (GO-3.16). If
// the node is the live PRIMARY of its shard, it first performs a graceful
// proactive failover to a synced replica and waits for the role to flip; the roll
// is deferred (requeue, Degraded) until that completes, and skipped entirely (no
// FORCE) when the shard has no synced replica. Otherwise it stamps the new
// hash/image and requeues for the node to settle.
func (r *Reconciler) rollNode(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	node *valkeyv1alpha1.ValkeyNode, state *valkey.ClusterState, configHash string,
) error {
	log := logf.FromContext(ctx)

	if state != nil {
		if live := liveStateOf(state, node); live != nil && live.IsPrimary() {
			done, err := r.proactiveFailover(ctx, cluster, state, live.ShardID)
			if err != nil {
				return err
			}
			if !done {
				// Defer the live-primary roll until the failover completes (or a
				// synced replica appears). Surface Degraded; retry next pass.
				setCondition(cluster, CondDegraded, metav1.ConditionTrue, ReasonFailoverPending,
					"deferring primary roll until a synced replica is promoted")
				return r.writeStatus(ctx, cluster)
			}
			// Role flipped: clear the transient Degraded so the demoted ex-primary
			// rolls cleanly once it returns Ready.
			setCondition(cluster, CondDegraded, metav1.ConditionFalse, ReasonReconciling, "failover complete")
		}
	}

	// Stamp the new hash/image (the actual roll trigger) and requeue to settle.
	key := nodeKeyOf(node)
	desired := buildValkeyNodeSpec(cluster, key, configHash)
	stamped := node.DeepCopy()
	stamped.Spec = desired.Spec
	stamped.Labels = desired.Labels
	if err := r.Update(ctx, stamped); err != nil {
		return fmt.Errorf("stamp roll on %s: %w", node.Name, err)
	}
	r.recorder.Eventf(cluster, stamped, eventNormal, EventValkeyNodeRolled, "RollValkeyNode",
		"rolling ValkeyNode %s (config/image change)", node.Name)
	log.V(1).Info("stamped roll, waiting for ready", "node", node.Name)
	return nil
}

// midRollNode returns a node that has been stamped with the desired roll-relevant
// spec but is not yet Ready/converged — an in-flight roll the controller must
// wait on before rolling the next node (one node at a time). nil when none.
func midRollNode(
	cluster *valkeyv1alpha1.PerconaValkeyCluster, nodes *valkeyv1alpha1.ValkeyNodeList, configHash string,
) *valkeyv1alpha1.ValkeyNode {
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !nodeNeedsRoll(n, cluster, configHash) && !nodeConverged(n, cluster, configHash) {
			// Spec already matches desired but not yet ready: a roll in flight.
			return n
		}
	}
	return nil
}

// nextRollCandidate picks the single next node to roll, replicas-before-primary
// by LIVE role: across all shards it returns a live replica needing a roll first,
// and only when no replica is rollable does it return a live primary (so a shard's
// primary rolls last). Within those tiers nodes are ordered by name for
// determinism. When live role is unknown for a node it falls back to the
// node-index label (node-index 0 == primary). nil when nothing is rollable.
func nextRollCandidate(
	cluster *valkeyv1alpha1.PerconaValkeyCluster, nodes *valkeyv1alpha1.ValkeyNodeList,
	state *valkey.ClusterState, configHash string,
) *valkeyv1alpha1.ValkeyNode {
	var primary *valkeyv1alpha1.ValkeyNode
	for _, n := range orderedByName(nodes) {
		if !nodeNeedsRoll(n, cluster, configHash) {
			continue
		}
		if isLivePrimaryNode(state, n) {
			if primary == nil {
				primary = n
			}
			continue // primaries roll last.
		}
		return n // a (live) replica needing a roll — roll it first.
	}
	return primary
}

// isLivePrimaryNode reports whether the ValkeyNode is the live primary of its
// shard, read from the scraped state by podIP (falling back to node-index 0 when
// live state is unavailable). 05 §6 live-role ordering.
func isLivePrimaryNode(state *valkey.ClusterState, node *valkeyv1alpha1.ValkeyNode) bool {
	if live := liveStateOf(state, node); live != nil {
		return live.IsPrimary()
	}
	return nodeIndexOf(node) == 0
}

// liveStateOf returns the scraped NodeState for a ValkeyNode, matched by its
// status.podIP dial address, or nil when unscraped / no podIP.
func liveStateOf(state *valkey.ClusterState, node *valkeyv1alpha1.ValkeyNode) *valkey.NodeState {
	if state == nil || node.Status.PodIP == "" {
		return nil
	}
	return state.NodeByAddr(node.Status.PodIP + ":" + strconv.Itoa(valkey.ClientPort))
}

// orderedByName returns the nodes sorted by name for deterministic roll ordering.
func orderedByName(nodes *valkeyv1alpha1.ValkeyNodeList) []*valkeyv1alpha1.ValkeyNode {
	out := make([]*valkeyv1alpha1.ValkeyNode, 0, len(nodes.Items))
	for i := range nodes.Items {
		out = append(out, &nodes.Items[i])
	}
	slices.SortFunc(out, func(a, b *valkeyv1alpha1.ValkeyNode) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// nodeKeyOf reconstructs a node's (shard, node) key from its topology labels.
func nodeKeyOf(node *valkeyv1alpha1.ValkeyNode) nodeKey {
	return nodeKey{shard: shardIndexOf(node), node: nodeIndexOf(node)}
}

// nodeConverged reports whether an existing node already matches the desired
// serverConfigHash/image AND is Ready with its current generation observed —
// the gate for advancing past it to the next node (04 §2.1 step6).
func nodeConverged(node *valkeyv1alpha1.ValkeyNode, cluster *valkeyv1alpha1.PerconaValkeyCluster, configHash string) bool {
	if node.Spec.ServerConfigHash != configHash {
		return false
	}
	if node.Spec.Image != cluster.Spec.Image {
		return false
	}
	if !node.Status.Ready {
		return false
	}
	if node.Status.ObservedGeneration != node.Generation {
		return false
	}
	return true
}

// indexNodesByName maps the listed nodes by name for O(1) lookup.
func indexNodesByName(nodes *valkeyv1alpha1.ValkeyNodeList) map[string]*valkeyv1alpha1.ValkeyNode {
	m := make(map[string]*valkeyv1alpha1.ValkeyNode, len(nodes.Items))
	for i := range nodes.Items {
		m[nodes.Items[i].Name] = &nodes.Items[i]
	}
	return m
}

// buildValkeyNodeSpec builds the desired ValkeyNode for a (shard, node) position,
// writing the full parent->node spec contract (03 §6): image/persistence/
// scheduling/config/exporter/tls + serverConfigMapName/serverConfigHash/
// aclSecretName + the cluster/shard/node topology labels. 04 §2.1 step6.
func buildValkeyNodeSpec(cluster *valkeyv1alpha1.PerconaValkeyCluster, key nodeKey, configHash string) *valkeyv1alpha1.ValkeyNode {
	labels := naming.NodeLabels(naming.NodeName(cluster.Name, key.shard, key.node),
		naming.ClusterTopologyLabels(cluster.Name, key.shard, key.node))

	node := &valkeyv1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.NodeName(cluster.Name, key.shard, key.node),
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: valkeyv1alpha1.ValkeyNodeSpec{
			Image:                        cluster.Spec.Image,
			ImagePullSecrets:             cluster.Spec.ImagePullSecrets,
			WorkloadType:                 cluster.Spec.WorkloadType,
			Persistence:                  cluster.Spec.Persistence,
			Resources:                    cluster.Spec.Resources,
			NodeSelector:                 cluster.Spec.NodeSelector,
			Affinity:                     cluster.Spec.Affinity,
			Tolerations:                  cluster.Spec.Tolerations,
			TopologySpreadConstraints:    cluster.Spec.TopologySpreadConstraints,
			Exporter:                     cluster.Spec.Exporter,
			Containers:                   cluster.Spec.Containers,
			TLS:                          nodeTLSConfig(cluster),
			Config:                       cluster.Spec.Config,
			Env:                          cluster.Spec.Env,
			ExtraEnvVars:                 cluster.Spec.ExtraEnvVars,
			ServiceAccountName:           cluster.Spec.ServiceAccountName,
			AutomountServiceAccountToken: cluster.Spec.AutomountServiceAccountToken,
			PodSecurityContext:           cluster.Spec.PodSecurityContext,
			ContainerSecurityContext:     cluster.Spec.ContainerSecurityContext,
			ServerConfigMapName:          naming.ClusterConfigMapName(cluster.Name),
			ServerConfigHash:             configHash,
			ACLSecretName:                naming.ACLSecretName(cluster.Name),
		},
	}
	// Propagate the per-pod external announce address (expose.perPod) onto the node
	// so the engine gossips its EXTERNAL addr via --cluster-announce-ip/-port
	// (announceForNode is the expose-announce seam; empty until that leg lands, so
	// the node keeps the in-cluster POD_IP announce by default).
	node.Spec.AnnounceHost, node.Spec.AnnouncePort = announceForNode(cluster, key)
	// Propagate the restore-seed marker (restored-from) onto the node so the node
	// controller injects the restore-seed init container that seeds this shard's RDB
	// before the engine boots (restoreSourceForNode is the restore-target seam; nil
	// until that leg resolves the storage/backup, so no seed container by default).
	node.Spec.RestoreFrom = restoreSourceForNode(cluster, key)
	// Propagate the tlsHash alongside serverConfigHash: reconcileTLS records the
	// hash on the cluster in-memory; stamping it as the node's naming.AnnTLSHash
	// annotation makes the resources builder roll the pod on a real cert change
	// via the same machinery as the config hash (07 §3.4). Empty when TLS is off
	// or the TLS leg has not computed a hash yet (no phantom roll).
	stampTLSHash(node, cluster.Annotations[naming.AnnTLSHash])
	return node
}
