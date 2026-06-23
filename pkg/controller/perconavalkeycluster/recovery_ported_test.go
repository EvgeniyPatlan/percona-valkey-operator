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

// This file ports high-value behavioral scenarios from the upstream
// valkey-io/valkey-operator test suites that OUR envtest harness did not already
// cover, translated onto OUR API (PerconaValkeyCluster + per-node ValkeyNode) and
// the fakecluster simulator. The covered gaps are:
//
//   - PDB sizing + selector content (maxUnavailable=1, cluster-label selector) and
//     recreate-on-external-delete — our existing PDB specs only checked existence
//     and the Disabled-policy delete path.
//     Ported/adapted from valkey-io/valkey-operator internal/controller/
//     valkeycluster_pdb_test.go (Apache-2.0).
//
//   - The FORCE-vs-TAKEOVER matrix's quorum-INTACT arm at the controller/envtest
//     level: a single failed primary (2-of-3 live = quorum) must NOT trigger an
//     operator-issued CLUSTER FAILOVER (native election / a quorum FORCE handles
//     it). Our wave2b recovery spec only exercised the quorum-LOSS (TAKEOVER) arm.
//     Ported/adapted from valkey-io/valkey-operator internal/controller/
//     failover_test.go TestFindFailoverShard (synced-replica eligibility) and our
//     promoteOrphanedReplicas quorum gate (Apache-2.0).
//
//   - Rolling-update pause-while-not-ready: a rolled node that is held not-ready
//     must stall the rollout (no further node is re-stamped) until it converges.
//     Ported/adapted from valkey-io/valkey-operator internal/controller/
//     valkeycluster_controller_test.go "pauses rollout while an updated node is not
//     yet ready" (Apache-2.0).

package perconavalkeycluster_test

import (
	"fmt"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeycluster"
)

var _ = ginkgo.Describe("PerconaValkeyCluster ported recovery/PDB/roll specs", func() {
	var (
		ns      string
		fc      *fakeCluster
		r       *perconavalkeycluster.Reconciler
		nsIndex int
	)

	ginkgo.BeforeEach(func() {
		nsIndex++
		ns = makeNamespace(fmt.Sprintf("pvk-ported-%d", nsIndex))
		fc = newFakeCluster()
		r = perconavalkeycluster.NewReconcilerForTest(k8sClient, apiScheme, &fakeClientFactory{fc: fc})
	})

	// Ported/adapted from valkey-io/valkey-operator valkeycluster_pdb_test.go
	// (Apache-2.0). Upstream uses minAvailable/maxUnavailable semantics over a
	// cluster-label selector; OUR operator pins a conservative cluster-wide
	// maxUnavailable=1 (pkg/controller/.../pdb.go OQ-3.B) with the cluster
	// selector. Our pre-existing specs only asserted the PDB EXISTS and that the
	// Disabled policy deletes it — never the sizing/selector content nor the
	// recreate-on-external-delete behaviour.
	ginkgo.It("creates a PDB with maxUnavailable=1 and the cluster selector, and recreates it when externally deleted", func() {
		cluster := makeCluster("pdbsize", ns, 1)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		pdbKey := types.NamespacedName{Name: "valkey-pdbsize-pdb", Namespace: ns}
		pdb := &policyv1.PodDisruptionBudget{}
		gomega.Expect(k8sClient.Get(testCtx, pdbKey, pdb)).To(gomega.Succeed())

		// Conservative cluster-wide sizing: at most one Valkey pod evicted at a time.
		gomega.Expect(pdb.Spec.MaxUnavailable).NotTo(gomega.BeNil())
		gomega.Expect(*pdb.Spec.MaxUnavailable).To(gomega.Equal(intstr.FromInt32(1)))
		gomega.Expect(pdb.Spec.MinAvailable).To(gomega.BeNil(),
			"our PDB uses maxUnavailable, never minAvailable")
		// The selector pins the cluster label so it covers every shard's pods.
		gomega.Expect(pdb.Spec.Selector).NotTo(gomega.BeNil())
		gomega.Expect(pdb.Spec.Selector.MatchLabels).To(
			gomega.HaveKeyWithValue("valkey.percona.com/cluster", "pdbsize"))

		// Externally delete the PDB; the next reconcile must recreate it.
		gomega.Expect(k8sClient.Delete(testCtx, pdb)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(testCtx, pdbKey, &policyv1.PodDisruptionBudget{}))
		}, timeout, interval).Should(gomega.BeTrue())

		_, err = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() error {
			return k8sClient.Get(testCtx, pdbKey, &policyv1.PodDisruptionBudget{})
		}, timeout, interval).Should(gomega.Succeed())
	})

	// Ported/adapted from valkey-io/valkey-operator failover_test.go
	// TestFindFailoverShard (Apache-2.0). Upstream asserts (at the helper level)
	// that a primary with a synced replica is failover-eligible but a primary
	// whose shard still has quorum is NOT unilaterally taken over; the operator
	// only observes while a normal vote can complete. OUR promoteOrphanedReplicas
	// (recovery.go) implements exactly that quorum gate, but no envtest spec
	// exercised the quorum-INTACT arm — wave2b only failed TWO of three primaries
	// (quorum loss -> TAKEOVER). Here a SINGLE failed primary keeps quorum
	// (2-of-3 live), so the operator must issue NO CLUSTER FAILOVER of any kind.
	ginkgo.It("does NOT issue an operator FAILOVER for a single failed primary while quorum is intact (FORCE-vs-TAKEOVER matrix, quorum-intact arm)", func() {
		cluster := makeCluster("quorumok", ns, 3)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 50)

		failoverBefore := len(fc.callsOfType("FAILOVER"))

		// Fail exactly ONE of three primaries: 2 of 3 remain live, so a failover
		// quorum still exists. The operator's takeover path is reserved for quorum
		// loss; with quorum intact native election handles promotion.
		fc.failPrimary("10.0.0.10")

		for i := 0; i < 10; i++ {
			reconcileOnce(r, key)
		}

		// No operator-issued failover (neither graceful, FORCE nor TAKEOVER): the
		// only recovery FAILOVER path is promoteOrphanedReplicas, which returns
		// early while HasFailoverQuorum() is true.
		gomega.Expect(len(fc.callsOfType("FAILOVER"))).To(gomega.Equal(failoverBefore),
			"a single primary loss with quorum intact must not trigger an operator FAILOVER")
	})

	// Ported/adapted from valkey-io/valkey-operator valkeycluster_controller_test.go
	// "pauses rollout while an updated node is not yet ready" (Apache-2.0).
	// Upstream re-stamps one node, holds it not-ready, and asserts no further node
	// is updated until it converges. OUR rolling update is config-hash driven and
	// gated on Status.Ready + ObservedGeneration == Generation (controller.go
	// phase 6 / 05 §6). The roll stalls cluster-wide on any single un-converged
	// node. Our smartupdate specs observed partial pacing but never explicitly
	// HELD a rolled node not-ready to prove the roll pauses.
	ginkgo.It("pauses the rolling update while a re-stamped node is held not-ready, then resumes once it converges", func() {
		cluster := makeCluster("rollpause", ns, 2)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 50)

		// Trigger a roll-worthy change (non-live key => new serverConfigHash).
		setConfig(key, map[string]string{"appendonly": "yes"})

		// Drive reconciles, but DO NOT promote nodes (so any node re-stamped by the
		// roll stays not-converged: ObservedGeneration < Generation). The roll must
		// re-stamp at most ONE node and then stall — never re-stamp a second.
		targetHash := ""
		for i := 0; i < 12; i++ {
			_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			h, n := rolledNodeCount(ns)
			if n > 0 && targetHash == "" {
				targetHash = h
			}
			// Invariant: while no node is converging, never more than one node is
			// re-stamped to the new hash (one-at-a-time, paused on the first).
			gomega.Expect(n).To(gomega.BeNumerically("<=", 1),
				"rolling update must pause after re-stamping one node while it is not ready")
		}
		_, stalled := rolledNodeCount(ns)
		gomega.Expect(stalled).To(gomega.Equal(1),
			"exactly one node should be mid-roll while held not-ready")

		// Now resume readiness convergence: promoting nodes each pass lets the roll
		// proceed node-by-node to completion.
		for i := 0; i < 80; i++ {
			reconcileOnce(r, key)
			if allNodesAtHash(ns) {
				break
			}
		}
		gomega.Expect(allNodesAtHash(ns)).To(gomega.BeTrue(),
			"the rolling update must resume and complete once the held node converges")
	})
})

// rolledNodeCount reports the most-common non-empty serverConfigHash among the
// namespace's ValkeyNodes and how many nodes carry a hash that differs from the
// majority (i.e. nodes that have been re-stamped by an in-flight roll but whose
// shardmates have not). It lets a spec assert one-at-a-time roll pacing without
// promoting nodes to ready. Returns ("", 0) when no nodes exist.
func rolledNodeCount(namespace string) (string, int) {
	nodes := &valkeyv1alpha1.ValkeyNodeList{}
	gomega.Expect(k8sClient.List(testCtx, nodes, client.InNamespace(namespace))).To(gomega.Succeed())
	if len(nodes.Items) == 0 {
		return "", 0
	}
	// Tally hashes; the majority hash is the pre-roll baseline.
	counts := map[string]int{}
	for i := range nodes.Items {
		counts[nodes.Items[i].Spec.ServerConfigHash]++
	}
	baseline := ""
	best := -1
	for h, c := range counts {
		if c > best {
			best, baseline = c, h
		}
	}
	moved := ""
	n := 0
	for i := range nodes.Items {
		if h := nodes.Items[i].Spec.ServerConfigHash; h != baseline {
			moved = h
			n++
		}
	}
	return moved, n
}
