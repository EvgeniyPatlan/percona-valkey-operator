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

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeycluster"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// makeCluster builds a minimal cluster spec (no tls/persistence/backup) with the
// given shard count and one replica per shard (the CRD default; an explicit 0
// would be re-defaulted to 1 by the replicas marker since the field is omitempty).
func makeCluster(name, namespace string, shards int32) *valkeyv1alpha1.PerconaValkeyCluster {
	return &valkeyv1alpha1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: valkeyv1alpha1.PerconaValkeyClusterSpec{
			Mode:     valkeyv1alpha1.ModeCluster,
			Shards:   shards,
			Replicas: 1,
			Image:    "percona/valkey:9.0",
		},
	}
}

// reconcileUntil drives Reconcile repeatedly (up to maxIters), after each pass
// promoting every created ValkeyNode to ready (the node controller is not
// running under envtest). It stops once the cluster reaches Ready or iterations
// are exhausted.
func reconcileUntilReady(r *perconavalkeycluster.Reconciler, key types.NamespacedName, maxIters int) {
	for i := 0; i < maxIters; i++ {
		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		promoteNodes(key.Namespace)
		cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, cluster)).To(gomega.Succeed())
		if cluster.Status.State == valkeyv1alpha1.StateReady {
			return
		}
	}
}

// promoteNodes drives every ValkeyNode in the namespace to ready, simulating the
// node controller: assigns a deterministic podIP, sets status.ready/role/podIP
// and observedGeneration. podIP is derived from shard/node index so the fake
// cluster keys stay stable across reconciles. A node whose spec was re-stamped
// (a rolling restart: a fresh generation the status has not yet observed) is
// likewise re-converged — the node controller rolls the pod and re-publishes
// status.ready + observedGeneration == generation when the roll settles, which
// the cluster controller waits on before advancing to the next node (04 §2.1
// step6, 05 §6). Without this the one-at-a-time roll would stall on the first
// re-stamped node forever.
func promoteNodes(namespace string) {
	nodes := &valkeyv1alpha1.ValkeyNodeList{}
	gomega.Expect(k8sClient.List(testCtx, nodes, client.InNamespace(namespace))).To(gomega.Succeed())
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Status.Ready && node.Status.ObservedGeneration == node.Generation {
			continue // already converged (and not mid-roll).
		}
		shard := node.Labels["valkey.percona.com/shard-index"]
		idx := node.Labels["valkey.percona.com/node-index"]
		ip := fmt.Sprintf("10.%s.%s.10", shard, idx)
		role := valkeyv1alpha1.NodeRolePrimary
		if idx != "0" {
			role = valkeyv1alpha1.NodeRoleReplica
		}
		node.Status.Ready = true
		node.Status.PodIP = ip
		node.Status.Role = role
		node.Status.ObservedGeneration = node.Generation
		gomega.Expect(k8sClient.Status().Update(testCtx, node)).To(gomega.Succeed())
	}
}

var _ = ginkgo.Describe("PerconaValkeyCluster controller bootstrap", func() {
	var (
		ns      string
		fc      *fakeCluster
		r       *perconavalkeycluster.Reconciler
		nsIndex int
	)

	ginkgo.BeforeEach(func() {
		nsIndex++
		ns = makeNamespace(fmt.Sprintf("pvk-bootstrap-%d", nsIndex))
		fc = newFakeCluster()
		r = perconavalkeycluster.NewReconcilerForTest(k8sClient, apiScheme, &fakeClientFactory{fc: fc})
	})

	ginkgo.It("creates Service/PDB/ACL-Secret/ConfigMap and one ValkeyNode per (shard,node) (E1)", func() {
		cluster := makeCluster("c1", ns, 3)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		// Several passes to step through one-at-a-time node creation.
		for i := 0; i < 12; i++ {
			_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			promoteNodes(ns)
		}

		svc := &corev1.Service{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-c1", Namespace: ns}, svc)).To(gomega.Succeed())
		gomega.Expect(svc.Spec.ClusterIP).To(gomega.Equal(corev1.ClusterIPNone))
		gomega.Expect(svc.Spec.PublishNotReadyAddresses).To(gomega.BeTrue())
		gomega.Expect(svc.Spec.Ports).To(gomega.HaveLen(2))

		pdb := &policyv1.PodDisruptionBudget{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-c1-pdb", Namespace: ns}, pdb)).To(gomega.Succeed())

		aclSecret := &corev1.Secret{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "internal-c1-acl", Namespace: ns}, aclSecret)).To(gomega.Succeed())
		gomega.Expect(aclSecret.Type).To(gomega.Equal(corev1.SecretType("valkey.io/acl")))
		acl := string(aclSecret.Data["users.acl"])
		gomega.Expect(acl).To(gomega.ContainSubstring("user _operator on #"))
		gomega.Expect(acl).To(gomega.ContainSubstring("-@all +cluster +config|get +config|set +info +client|setname +client|setinfo +replicaof +wait +ping"))
		gomega.Expect(acl).To(gomega.ContainSubstring("user _backup"))

		cm := &corev1.ConfigMap{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-c1", Namespace: ns}, cm)).To(gomega.Succeed())
		gomega.Expect(cm.Data["valkey.conf"]).To(gomega.ContainSubstring("cluster-enabled yes"))

		// 3 shards * (1 primary + 1 replica) = 6 ValkeyNodes, named c1-<s>-<n>.
		nodes := &valkeyv1alpha1.ValkeyNodeList{}
		gomega.Expect(k8sClient.List(testCtx, nodes, client.InNamespace(ns))).To(gomega.Succeed())
		gomega.Expect(nodes.Items).To(gomega.HaveLen(6))
		names := map[string]bool{}
		for i := range nodes.Items {
			names[nodes.Items[i].Name] = true
			gomega.Expect(nodes.Items[i].Spec.ServerConfigMapName).To(gomega.Equal("valkey-c1"))
			gomega.Expect(nodes.Items[i].Spec.ACLSecretName).To(gomega.Equal("internal-c1-acl"))
			gomega.Expect(nodes.Items[i].Spec.ServerConfigHash).NotTo(gomega.BeEmpty())
		}
		for s := 0; s < 3; s++ {
			for n := 0; n < 2; n++ {
				gomega.Expect(names[fmt.Sprintf("c1-%d-%d", s, n)]).To(gomega.BeTrue(), "missing node c1-%d-%d", s, n)
			}
		}
	})

	ginkgo.It("forms a healthy cluster: MEET then ADDSLOTSRANGE then REPLICATE, reaches Ready (E2/CR-6)", func() {
		cluster := makeCluster("c2", ns, 3)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileUntilReady(r, key, 40)

		// Final state: Ready with conditions + readyShards == shards.
		final := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
		gomega.Expect(final.Status.State).To(gomega.Equal(valkeyv1alpha1.StateReady))
		gomega.Expect(conditionStatus(final, "ClusterFormed")).To(gomega.Equal(metav1.ConditionTrue))
		gomega.Expect(conditionStatus(final, "SlotsAssigned")).To(gomega.Equal(metav1.ConditionTrue))
		gomega.Expect(conditionStatus(final, "Ready")).To(gomega.Equal(metav1.ConditionTrue))
		gomega.Expect(final.Status.Shards).To(gomega.Equal(int32(3)))
		gomega.Expect(final.Status.ReadyShards).To(gomega.Equal(int32(3)))
		gomega.Expect(final.Status.ObservedGeneration).To(gomega.Equal(final.Generation))
		gomega.Expect(final.Status.Host).To(gomega.Equal("valkey-c2." + ns + ".svc"))

		// Command-order invariant: last MEET before first ADDSLOTSRANGE before
		// first REPLICATE (the strict bootstrap ordering, 05 §3).
		gomega.Expect(fc.firstIndexOf("ADDSLOTSRANGE")).To(gomega.BeNumerically(">", fc.lastIndexOf("MEET")))
		gomega.Expect(fc.firstIndexOf("REPLICATE")).To(gomega.BeNumerically(">", fc.lastIndexOf("ADDSLOTSRANGE")))

		// Even slot split summing to exactly 16384 across 3 primaries.
		addCalls := fc.callsOfType("ADDSLOTSRANGE")
		gomega.Expect(addCalls).To(gomega.HaveLen(3))
		gomega.Expect(fc.totalAssignedSlots()).To(gomega.Equal(valkey.TotalSlots))

		// CR-6: 3 primaries replicated-to once, no double-issue. 3 replicas total
		// (one per shard), so exactly 3 REPLICATE calls.
		gomega.Expect(fc.callsOfType("REPLICATE")).To(gomega.HaveLen(3))
	})

	ginkgo.It("forms a minimal single-shard cluster with no tls/persistence/backup (has()-guard discipline)", func() {
		// A minimal spec: no TLS, no persistence, no backup. replicas defaults to
		// 1 (the CRD default marker stamps it since the field is omitempty), so the
		// single shard is a primary + one replica. This exercises the has()-guard
		// discipline — nil Persistence/TLS must not panic the config/node builders.
		cluster := makeCluster("c3", ns, 1)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileUntilReady(r, key, 40)

		final := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
		gomega.Expect(final.Status.State).To(gomega.Equal(valkeyv1alpha1.StateReady))
		gomega.Expect(final.Status.Shards).To(gomega.Equal(int32(1)))
		gomega.Expect(final.Status.ReadyShards).To(gomega.Equal(int32(1)))
		// The single primary owns all 16384 slots (one ADDSLOTSRANGE).
		gomega.Expect(fc.totalAssignedSlots()).To(gomega.Equal(valkey.TotalSlots))
		gomega.Expect(fc.callsOfType("ADDSLOTSRANGE")).To(gomega.HaveLen(1))
		// One replica attaches to the one primary.
		gomega.Expect(fc.callsOfType("REPLICATE")).To(gomega.HaveLen(1))

		// has()-guard: no persistence/TLS configured, and the rendered config
		// omits the persistence/tls directive blocks.
		cm := &corev1.ConfigMap{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-c3", Namespace: ns}, cm)).To(gomega.Succeed())
		gomega.Expect(cm.Data["valkey.conf"]).NotTo(gomega.ContainSubstring("tls-port"))
	})

	ginkgo.It("fails with a clear reason when defaults validation fails (invalid backup schedule)", func() {
		cluster := makeCluster("c6", ns, 3)
		cluster.Spec.Backup.Schedule = []valkeyv1alpha1.BackupScheduleSpec{
			{Name: "nightly", Schedule: "0 0 * * *", StorageName: "does-not-exist"},
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).To(gomega.HaveOccurred()) // fail() returns the error for backoff.

		final := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
		gomega.Expect(conditionStatus(final, "Ready")).To(gomega.Equal(metav1.ConditionFalse))
		gomega.Expect(conditionReason(final, "Ready")).To(gomega.Equal("ConfigMapError"))
	})

	ginkgo.It("deletes the PDB when podDisruptionBudget policy is Disabled", func() {
		cluster := makeCluster("c5", ns, 3)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		// First reconcile creates the PDB (Managed default).
		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pdb := &policyv1.PodDisruptionBudget{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-c5-pdb", Namespace: ns}, pdb)).To(gomega.Succeed())

		// Flip to Disabled; the next reconcile deletes it.
		gomega.Expect(k8sClient.Get(testCtx, key, cluster)).To(gomega.Succeed())
		cluster.Spec.PodDisruptionBudget = valkeyv1alpha1.PDBDisabled
		gomega.Expect(k8sClient.Update(testCtx, cluster)).To(gomega.Succeed())
		_, err = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() bool {
			e := k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-c5-pdb", Namespace: ns}, &policyv1.PodDisruptionBudget{})
			return apierrors.IsNotFound(e)
		}, timeout, interval).Should(gomega.BeTrue())
	})

	ginkgo.It("stops with UnsupportedCRVersion when crVersion is newer than the operator", func() {
		cluster := makeCluster("c4", ns, 3)
		cluster.Spec.CrVersion = "999.0"
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		final := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
		gomega.Expect(conditionReason(final, "Ready")).To(gomega.Equal("UnsupportedCRVersion"))
		// No nodes created (reconcile stopped at the gate).
		nodes := &valkeyv1alpha1.ValkeyNodeList{}
		gomega.Expect(k8sClient.List(testCtx, nodes, client.InNamespace(ns))).To(gomega.Succeed())
		gomega.Expect(nodes.Items).To(gomega.BeEmpty())
	})

	ginkgo.It("rejects a runtime crVersion downgrade with Degraded/CrVersionDowngradeRejected and keeps the prior contract (GO-6.3)", func() {
		// Form the cluster at the operator's stamped crVersion; reaching Ready writes
		// status.lastObservedCrVersion, the monotonicity anchor.
		cluster := makeCluster("crdown", ns, 3)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		formed := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, formed)).To(gomega.Succeed())
		observed := formed.Status.LastObservedCrVersion
		gomega.Expect(observed).NotTo(gomega.BeEmpty(), "a Ready cluster must mirror its accepted crVersion")

		rec := events.NewFakeRecorder(400)
		r.RecorderForTest(rec)

		// Attempt to lower crVersion below the last-observed value: 0.0 < 0.1.
		gomega.Expect(k8sClient.Get(testCtx, key, formed)).To(gomega.Succeed())
		formed.Spec.CrVersion = "0.0"
		gomega.Expect(k8sClient.Update(testCtx, formed)).To(gomega.Succeed())

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// The decrease is refused: Degraded/CrVersionDowngradeRejected, prior contract held.
		after := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, after)).To(gomega.Succeed())
		gomega.Expect(after.Status.State).To(gomega.Equal(valkeyv1alpha1.StateDegraded))
		gomega.Expect(conditionReason(after, perconavalkeycluster.CondDegraded)).
			To(gomega.Equal(perconavalkeycluster.ReasonCrVersionDowngradeRejected))
		// The last-observed anchor is unchanged (the downgrade never advanced it).
		gomega.Expect(after.Status.LastObservedCrVersion).To(gomega.Equal(observed))
		// A Warning event named the refusal.
		gomega.Expect(drainRecorder(rec)).To(gomega.ContainElement(
			gomega.ContainSubstring(perconavalkeycluster.ReasonCrVersionDowngradeRejected)))
	})
})

var _ = ginkgo.Describe("PerconaValkeyCluster controller manager wiring", func() {
	ginkgo.It("forms a cluster end-to-end through the manager (Owns/Watches + node status flip enqueue)", func() {
		cluster := makeCluster("mgrc", mgrNamespace, 3)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		// The manager reconciles on its own; we only drive node readiness (the
		// node controller is not running) and let the Owns(ValkeyNode) status-flip
		// watch re-enqueue the owner until the cluster forms.
		gomega.Eventually(func() valkeyv1alpha1.ClusterState {
			promoteNodes(mgrNamespace)
			final := &valkeyv1alpha1.PerconaValkeyCluster{}
			if err := k8sClient.Get(testCtx, key, final); err != nil {
				return ""
			}
			return final.Status.State
		}, timeout, interval).Should(gomega.Equal(valkeyv1alpha1.StateReady))

		gomega.Expect(mgrFC.totalAssignedSlots()).To(gomega.Equal(valkey.TotalSlots))
	})
})

// conditionStatus returns the status of the named condition (or "" if absent).
func conditionStatus(cluster *valkeyv1alpha1.PerconaValkeyCluster, condType string) metav1.ConditionStatus {
	for _, c := range cluster.Status.Conditions {
		if c.Type == condType {
			return c.Status
		}
	}
	return ""
}

// conditionReason returns the reason of the named condition (or "" if absent).
func conditionReason(cluster *valkeyv1alpha1.PerconaValkeyCluster, condType string) string {
	for _, c := range cluster.Status.Conditions {
		if c.Type == condType {
			return c.Reason
		}
	}
	return ""
}
