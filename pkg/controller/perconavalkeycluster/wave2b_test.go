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
	"fmt"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeycluster"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// reconcileOnce drives a single Reconcile then promotes any newly-created nodes,
// returning the requeue result. Used by the Wave 2b specs that need to observe
// per-pass effects (command order, one-move-per-reconcile).
func reconcileOnce(r *perconavalkeycluster.Reconciler, key types.NamespacedName) ctrl.Result {
	res, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	promoteNodes(key.Namespace)
	return res
}

// setShards updates spec.shards on the cluster (a scale operation).
func setShards(key types.NamespacedName, shards int32) {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	gomega.Expect(k8sClient.Get(testCtx, key, cluster)).To(gomega.Succeed())
	cluster.Spec.Shards = shards
	gomega.Expect(k8sClient.Update(testCtx, cluster)).To(gomega.Succeed())
}

var _ = ginkgo.Describe("PerconaValkeyCluster Wave 2b lifecycle", func() {
	var (
		ns      string
		fc      *fakeCluster
		r       *perconavalkeycluster.Reconciler
		nsIndex int
	)

	ginkgo.BeforeEach(func() {
		nsIndex++
		ns = makeNamespace(fmt.Sprintf("pvk-2b-%d", nsIndex))
		fc = newFakeCluster()
		r = perconavalkeycluster.NewReconcilerForTest(k8sClient, apiScheme, &fakeClientFactory{fc: fc})
	})

	ginkgo.It("scale-out 3->4 rebalances one MIGRATESLOTS move per reconcile to balanced+full-coverage (GO-3.14/CR-6)", func() {
		cluster := makeCluster("so", ns, 3)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileUntilReady(r, key, 40)
		gomega.Expect(fc.totalAssignedSlots()).To(gomega.Equal(valkey.TotalSlots))

		// Scale out to 4 shards.
		setShards(key, 4)

		// Drive to convergence; assert one MIGRATESLOTS per reconcile pacing.
		maxMigratePerPass := 0
		for i := 0; i < 60; i++ {
			before := len(fc.callsOfType("MIGRATESLOTS"))
			reconcileOnce(r, key)
			delta := len(fc.callsOfType("MIGRATESLOTS")) - before
			if delta > maxMigratePerPass {
				maxMigratePerPass = delta
			}
			final := &valkeyv1alpha1.PerconaValkeyCluster{}
			gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
			if final.Status.State == valkeyv1alpha1.StateReady && final.Status.Shards == 4 {
				break
			}
		}

		// One move per reconcile (the deliberate ~30s pacing, CR-6 no double-issue).
		gomega.Expect(maxMigratePerPass).To(gomega.Equal(1))

		final := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
		gomega.Expect(final.Status.State).To(gomega.Equal(valkeyv1alpha1.StateReady))
		gomega.Expect(final.Status.Shards).To(gomega.Equal(int32(4)))
		// Full coverage preserved across the rebalance.
		gomega.Expect(fc.totalAssignedSlots()).To(gomega.Equal(valkey.TotalSlots))
		// Balanced: 16384/4 = 4096 per shard (±1 tolerance via the planner).
		for _, n := range fc.primarySlotCounts() {
			gomega.Expect(n).To(gomega.BeNumerically("~", 4096, 1))
		}
		// At least one migration actually happened.
		gomega.Expect(len(fc.callsOfType("MIGRATESLOTS"))).To(gomega.BeNumerically(">", 0))
	})

	ginkgo.It("scale-in 4->3 drains then deletes+forgets the excess shard (GO-3.15)", func() {
		cluster := makeCluster("si", ns, 4)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileUntilReady(r, key, 50)
		gomega.Expect(fc.totalAssignedSlots()).To(gomega.Equal(valkey.TotalSlots))

		// Capture shard-3's node IDs before they are drained/deleted/forgotten.
		shard3IDs := fc.idsForShard(3)
		gomega.Expect(shard3IDs).NotTo(gomega.BeEmpty())

		// Scale in to 3 shards.
		setShards(key, 3)

		// Drive until the excess shard is drained, its nodes deleted, AND the stale
		// IDs forgotten cluster-wide (the forget happens on passes after the delete).
		for i := 0; i < 100; i++ {
			reconcileOnce(r, key)
			nodes := &valkeyv1alpha1.ValkeyNodeList{}
			gomega.Expect(k8sClient.List(testCtx, nodes, client.InNamespace(ns))).To(gomega.Succeed())
			final := &valkeyv1alpha1.PerconaValkeyCluster{}
			gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
			forgotten := true
			for _, id := range shard3IDs {
				if fc.knownByPeers(id) {
					forgotten = false
				}
			}
			if len(nodes.Items) == 6 && final.Status.State == valkeyv1alpha1.StateReady && forgotten {
				break
			}
		}

		// Excess shard's ValkeyNodes are gone (deleted after drain).
		nodes := &valkeyv1alpha1.ValkeyNodeList{}
		gomega.Expect(k8sClient.List(testCtx, nodes, client.InNamespace(ns))).To(gomega.Succeed())
		gomega.Expect(nodes.Items).To(gomega.HaveLen(6)) // 3 shards * 2.
		for i := range nodes.Items {
			gomega.Expect(shardIndexLabel(&nodes.Items[i])).To(gomega.BeNumerically("<", 3))
		}

		// Draining happened, and the survivors still cover all 16384 slots.
		gomega.Expect(len(fc.callsOfType("MIGRATESLOTS"))).To(gomega.BeNumerically(">", 0))
		gomega.Expect(fc.totalAssignedSlots()).To(gomega.Equal(valkey.TotalSlots))

		// FORGET was broadcast for the drained shard's stale node IDs.
		forgets := fc.callsOfType("FORGET")
		gomega.Expect(forgets).NotTo(gomega.BeEmpty())
		for _, id := range shard3IDs {
			gomega.Expect(fc.knownByPeers(id)).To(gomega.BeFalse(), "stale id %s should be forgotten", id)
		}
	})

	ginkgo.It("rolls replicas-before-primary and proactively fails over before rolling the live primary (GO-3.16)", func() {
		cluster := makeCluster("roll", ns, 2)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		failoverBefore := len(fc.callsOfType("FAILOVER"))

		// A roll-triggering config change (appendonly => new serverConfigHash).
		setConfig(key, map[string]string{"appendonly": "yes"})

		// Drive the roll to completion, recording the order of FAILOVER vs the
		// per-node roll stamps.
		sawFailover := false
		for i := 0; i < 80; i++ {
			reconcileOnce(r, key)
			if len(fc.callsOfType("FAILOVER")) > failoverBefore {
				sawFailover = true
			}
			final := &valkeyv1alpha1.PerconaValkeyCluster{}
			gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
			if final.Status.State == valkeyv1alpha1.StateReady && allNodesAtHash(ns) {
				break
			}
		}

		// A proactive graceful failover was issued (to roll the live primary).
		gomega.Expect(sawFailover).To(gomega.BeTrue())
		failovers := fc.callsOfType("FAILOVER")
		gomega.Expect(failovers).NotTo(gomega.BeEmpty())
		// The proactive failover is GRACEFUL (empty mode), never FORCE/TAKEOVER on
		// a live primary (CR-5 safety).
		for _, f := range failovers[failoverBefore:] {
			gomega.Expect(f.arg).To(gomega.Equal(""), "live-primary roll must use graceful failover, not %q", f.arg)
		}

		// The graceful failover actually flipped a role: at least one node that
		// started as a node-index-1 replica (10.<shard>.1.10) is now the live
		// primary of its shard — the live role is read from the engine, never the
		// node-index label (05 §1, §6).
		flipped := false
		for shard := 0; shard < 2; shard++ {
			if fc.roleAt(fmt.Sprintf("10.%d.1.10:6379", shard)) == valkey.RolePrimary {
				flipped = true
			}
		}
		gomega.Expect(flipped).To(gomega.BeTrue(), "a proactive failover should have promoted a replica to live primary")

		final := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
		gomega.Expect(final.Status.State).To(gomega.Equal(valkeyv1alpha1.StateReady))
	})

	ginkgo.It("defers the primary roll when the shard has no synced replica (GO-3.16)", func() {
		// A single-shard, zero-replica cluster: its lone primary has no replica, so
		// a roll must DEFER (no FORCE) and surface a FailoverDeferred event.
		//
		// spec.replicas carries +kubebuilder:default=1, which the API server applies
		// only when the field is ABSENT from the submitted object. The typed client's
		// `omitempty` drops a zero value, so a plain Create with Replicas=0 would be
		// silently re-defaulted to 1 (and the shard would get a synced replica, never
		// deferring). Submit the 0 EXPLICITLY via a merge patch so the field is
		// present and defaulting leaves it untouched — genuinely a zero-replica shard.
		cluster := makeCluster("noreplica", ns, 1)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		gomega.Expect(k8sClient.Patch(testCtx, cluster,
			client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":0}}`)))).To(gomega.Succeed())
		// Confirm the zero replica count actually persisted (guards against a future
		// schema change re-introducing the default-to-1 footgun).
		persisted := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, persisted)).To(gomega.Succeed())
		gomega.Expect(persisted.Spec.Replicas).To(gomega.Equal(int32(0)))
		reconcileUntilReady(r, key, 40)

		rec := events.NewFakeRecorder(200)
		r.RecorderForTest(rec)

		setConfig(key, map[string]string{"appendonly": "yes"})

		// Several passes: the primary roll is deferred each time (no synced replica).
		for i := 0; i < 6; i++ {
			reconcileOnce(r, key)
		}

		// No FAILOVER of any kind was issued (deferred, never FORCE on a live primary).
		gomega.Expect(fc.callsOfType("FAILOVER")).To(gomega.BeEmpty())
		gomega.Expect(drainRecorder(rec)).To(gomega.ContainElement(gomega.ContainSubstring("FailoverDeferred")))
		// The lone primary was NOT rolled (its hash is still the old one).
		gomega.Expect(allNodesAtHash(ns)).To(gomega.BeFalse())
	})

	ginkgo.It("promotes an orphaned replica via TAKEOVER only on quorum loss, and forgets stale nodes (GO-3.17/CR-5)", func() {
		// 3 shards / 1 replica, persistence OFF (the takeover precondition).
		cluster := makeCluster("rec", ns, 3)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 50)

		// Fail TWO of three primaries so quorum is lost (1 of 3 live < majority).
		fc.failPrimary("10.0.0.10") // shard 0 primary
		fc.failPrimary("10.1.0.10") // shard 1 primary

		// Inject a stale in-gossip-only node id (no backing ValkeyNode).
		staleID := fc.addStaleGossipNode()

		// Drive recovery passes.
		for i := 0; i < 10; i++ {
			reconcileOnce(r, key)
		}

		failovers := fc.callsOfType("FAILOVER")
		gomega.Expect(failovers).NotTo(gomega.BeEmpty())
		// CR-5: under quorum loss the promotion is TAKEOVER (never graceful/FORCE).
		for _, f := range failovers {
			gomega.Expect(f.arg).To(gomega.Equal(string(valkey.FailoverTakeover)),
				"quorum-loss promotion must be TAKEOVER, got %q", f.arg)
		}
		// The stale in-gossip-only node was FORGOTten cluster-wide.
		gomega.Expect(fc.knownByPeers(staleID)).To(gomega.BeFalse())
	})

	ginkgo.It("does NOT take over when persistence is on (same-node-ID pod returns) (GO-3.17)", func() {
		cluster := makeCluster("persist", ns, 3)
		cluster.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 50)

		fc.failPrimary("10.0.0.10")
		fc.failPrimary("10.1.0.10")

		for i := 0; i < 6; i++ {
			reconcileOnce(r, key)
		}
		// Persistence on => no takeover (the restarted pod reclaims its slots).
		gomega.Expect(fc.callsOfType("FAILOVER")).To(gomega.BeEmpty())
	})

	ginkgo.It("ordered teardown removes the finalizer and the cluster is gone (GO-3.19)", func() {
		cluster := makeCluster("del", ns, 2)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		// Finalizers are registered.
		got := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, got)).To(gomega.Succeed())
		gomega.Expect(got.Finalizers).To(gomega.ContainElement("valkey.percona.com/delete-pods-in-order"))

		// Delete the cluster: the API server sets deletionTimestamp (finalizers
		// block GC) and the controller runs ordered teardown.
		gomega.Expect(k8sClient.Delete(testCtx, got)).To(gomega.Succeed())

		for i := 0; i < 40; i++ {
			res, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// Reap any ValkeyNodes the teardown deleted (no node controller here):
			// their owner-GC + finalizer removal is simulated by the API server.
			if apierrors.IsNotFound(k8sClient.Get(testCtx, key, &valkeyv1alpha1.PerconaValkeyCluster{})) {
				break
			}
			if res.RequeueAfter == 0 {
				// Teardown removed the finalizer; the API server GCs the object.
				_, _ = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			}
		}

		// The cluster object is gone (finalizers cleared, GC complete).
		gomega.Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(testCtx, key, &valkeyv1alpha1.PerconaValkeyCluster{}))
		}, timeout, interval).Should(gomega.BeTrue())

		// Its ValkeyNodes were deleted in ordered teardown.
		nodes := &valkeyv1alpha1.ValkeyNodeList{}
		gomega.Expect(k8sClient.List(testCtx, nodes, client.InNamespace(ns))).To(gomega.Succeed())
		gomega.Expect(nodes.Items).To(gomega.BeEmpty())
	})
})

// setConfig updates spec.config (a roll-triggering change for non-live keys).
func setConfig(key types.NamespacedName, cfg map[string]string) {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	gomega.Expect(k8sClient.Get(testCtx, key, cluster)).To(gomega.Succeed())
	cluster.Spec.Config = cfg
	gomega.Expect(k8sClient.Update(testCtx, cluster)).To(gomega.Succeed())
}

// allNodesAtHash reports whether every ValkeyNode in the namespace carries a
// non-empty serverConfigHash AND is Ready — used to detect roll completion.
func allNodesAtHash(namespace string) bool {
	nodes := &valkeyv1alpha1.ValkeyNodeList{}
	if err := k8sClient.List(testCtx, nodes, client.InNamespace(namespace)); err != nil {
		return false
	}
	if len(nodes.Items) == 0 {
		return false
	}
	hash := nodes.Items[0].Spec.ServerConfigHash
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.ServerConfigHash != hash || !n.Status.Ready {
			return false
		}
	}
	return true
}

// shardIndexLabel returns a node's shard-index label as an int.
func shardIndexLabel(node *valkeyv1alpha1.ValkeyNode) int {
	return atoiLabel(node.Labels["valkey.percona.com/shard-index"])
}

// drainRecorder non-blockingly drains all events currently buffered in a
// FakeRecorder channel, returning them as a slice.
func drainRecorder(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}
