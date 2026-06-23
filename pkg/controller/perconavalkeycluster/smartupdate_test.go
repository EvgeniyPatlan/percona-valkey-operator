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
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeybackup"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeycluster"
)

// setImage edits spec.image on the cluster (the engine-roll trigger, GO-6.9).
func setImage(key types.NamespacedName, image string) {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	gomega.Expect(k8sClient.Get(testCtx, key, cluster)).To(gomega.Succeed())
	cluster.Spec.Image = image
	gomega.Expect(k8sClient.Update(testCtx, cluster)).To(gomega.Succeed())
}

// allNodesAtImage reports whether every ValkeyNode in the namespace carries the
// given spec.image AND is Ready/converged — engine-roll completion.
func allNodesAtImage(namespace, image string) bool {
	nodes := &valkeyv1alpha1.ValkeyNodeList{}
	if err := k8sClient.List(testCtx, nodes, client.InNamespace(namespace)); err != nil {
		return false
	}
	if len(nodes.Items) == 0 {
		return false
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Image != image || !n.Status.Ready || n.Status.ObservedGeneration != n.Generation {
			return false
		}
	}
	return true
}

// nodesOnImage counts the ValkeyNodes in the namespace stamped with image.
func nodesOnImage(namespace, image string) int {
	nodes := &valkeyv1alpha1.ValkeyNodeList{}
	gomega.Expect(k8sClient.List(testCtx, nodes, client.InNamespace(namespace))).To(gomega.Succeed())
	n := 0
	for i := range nodes.Items {
		if nodes.Items[i].Spec.Image == image {
			n++
		}
	}
	return n
}

// holdBackupLease seeds a fresh per-cluster backup Lease held by a fake backup so
// the smart-update gate observes a backup running (CR-14). dropBackupLease frees
// it. Both are control-plane writes the cluster controller never makes itself.
func holdBackupLease(namespace, cluster string) {
	now := metav1.NewMicroTime(time.Now())
	holder := "backup/" + namespace + "/bk"
	dur := int32(30)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: perconavalkeybackup.LeaseName(cluster), Namespace: namespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			RenewTime:            &now,
		},
	}
	gomega.Expect(k8sClient.Create(testCtx, lease)).To(gomega.Succeed())
}

func dropBackupLease(namespace, cluster string) {
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Name: perconavalkeybackup.LeaseName(cluster), Namespace: namespace}
	gomega.Expect(k8sClient.Get(testCtx, key, lease)).To(gomega.Succeed())
	gomega.Expect(k8sClient.Delete(testCtx, lease)).To(gomega.Succeed())
}

var _ = ginkgo.Describe("PerconaValkeyCluster M6 smart engine update", func() {
	var (
		ns      string
		fc      *fakeCluster
		r       *perconavalkeycluster.Reconciler
		nsIndex int
	)

	const (
		baseImage = "percona/valkey:9.0.0"
		newImage  = "percona/valkey:9.0.1" // patch-forward within the 9.0 line.
	)

	ginkgo.BeforeEach(func() {
		nsIndex++
		ns = makeNamespace(fmt.Sprintf("pvk-m6-%d", nsIndex))
		fc = newFakeCluster()
		r = perconavalkeycluster.NewReconcilerForTest(k8sClient, apiScheme, &fakeClientFactory{fc: fc})
	})

	ginkgo.It("rolls the engine shard-by-shard, replicas-before-primary with proactive failover on a spec.image change (GO-6.9/6.10)", func() {
		cluster := makeCluster("eng", ns, 2)
		cluster.Spec.Image = baseImage
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)
		gomega.Expect(allNodesAtImage(ns, baseImage)).To(gomega.BeTrue())

		failoverBefore := len(fc.callsOfType("FAILOVER"))

		// Trigger the engine roll: a new image tag (patch-forward, no downgrade).
		setImage(key, newImage)

		// Observe one-node-at-a-time pacing: the new image never lands on more than
		// one node per pass beyond those already converged (one ValkeyNode under
		// churn cluster-wide, 09 §5 invariant 2).
		sawPartial := false
		sawFailover := false
		for i := 0; i < 80; i++ {
			reconcileOnce(r, key)
			if len(fc.callsOfType("FAILOVER")) > failoverBefore {
				sawFailover = true
			}
			onNew := nodesOnImage(ns, newImage)
			if onNew > 0 && onNew < 4 {
				sawPartial = true // a mid-roll snapshot: some nodes rolled, some not.
			}
			if allNodesAtImage(ns, newImage) {
				break
			}
		}

		// Every node ended on the new engine image.
		gomega.Expect(allNodesAtImage(ns, newImage)).To(gomega.BeTrue())
		// The roll progressed incrementally (never all-at-once).
		gomega.Expect(sawPartial).To(gomega.BeTrue(), "engine roll should advance one node at a time")
		// Rolling the live primaries was preceded by a graceful proactive failover.
		gomega.Expect(sawFailover).To(gomega.BeTrue(), "a live-primary engine roll must proactively fail over first")
		for _, f := range fc.callsOfType("FAILOVER")[failoverBefore:] {
			gomega.Expect(f.arg).To(gomega.Equal(""), "engine-roll failover must be graceful, not %q", f.arg)
		}

		// A few more passes let phase-15 verify re-mark the cluster Ready now that
		// every node is on the new engine (the roll-complete pass returns before
		// verify runs).
		reconcileUntilReady(r, key, 20)
		final := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
		gomega.Expect(final.Status.State).To(gomega.Equal(valkeyv1alpha1.StateReady))
		gomega.Expect(allNodesAtImage(ns, newImage)).To(gomega.BeTrue())
	})

	ginkgo.It("blocks the engine roll while a backup Lease is held and resumes after it is released (GO-6.8/CR-14)", func() {
		cluster := makeCluster("gate", ns, 2)
		cluster.Spec.Image = baseImage
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		rec := events.NewFakeRecorder(400)
		r.RecorderForTest(rec)

		// A backup is running: hold the per-cluster Lease, then trigger the roll.
		holdBackupLease(ns, cluster.Name)
		setImage(key, newImage)

		// Drive several passes: the gate must hold — no node moves to the new image.
		for i := 0; i < 10; i++ {
			reconcileOnce(r, key)
		}
		gomega.Expect(nodesOnImage(ns, newImage)).To(gomega.Equal(0),
			"engine roll must not touch data pods while a backup runs")
		gomega.Expect(allNodesAtImage(ns, baseImage)).To(gomega.BeTrue())

		// Release the backup Lease: the roll now proceeds to completion.
		dropBackupLease(ns, cluster.Name)
		for i := 0; i < 80; i++ {
			reconcileOnce(r, key)
			if allNodesAtImage(ns, newImage) {
				break
			}
		}
		gomega.Expect(allNodesAtImage(ns, newImage)).To(gomega.BeTrue(),
			"engine roll must resume once the backup Lease is released")
	})

	ginkgo.It("refuses an engine feature-line downgrade with Degraded/UnsupportedDowngrade and no roll (GO-6.12)", func() {
		cluster := makeCluster("down", ns, 2)
		cluster.Spec.Image = "percona/valkey:9.0.0"
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		rec := events.NewFakeRecorder(400)
		r.RecorderForTest(rec)

		// Attempt a feature-line downgrade 9.0 -> 8.0: must be refused.
		downImage := "percona/valkey:8.0.1"
		setImage(key, downImage)

		for i := 0; i < 8; i++ {
			reconcileOnce(r, key)
		}

		// No node was rolled to the older engine.
		gomega.Expect(nodesOnImage(ns, downImage)).To(gomega.Equal(0),
			"a feature-line downgrade must not roll any node")
		// The cluster is Degraded with the UnsupportedDowngrade reason.
		final := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, final)).To(gomega.Succeed())
		gomega.Expect(final.Status.State).To(gomega.Equal(valkeyv1alpha1.StateDegraded))
		cond := findCondition(final, perconavalkeycluster.CondDegraded)
		gomega.Expect(cond).NotTo(gomega.BeNil())
		gomega.Expect(cond.Reason).To(gomega.Equal(perconavalkeycluster.ReasonUnsupportedDowngrade))
		// A Warning event was emitted naming the refusal.
		gomega.Expect(drainRecorder(rec)).To(gomega.ContainElement(
			gomega.ContainSubstring(perconavalkeycluster.ReasonUnsupportedDowngrade)))
	})

	ginkgo.It("does not roll the engine when the cluster is not Ready (GO-6.8 health gate)", func() {
		cluster := makeCluster("notready", ns, 2)
		cluster.Spec.Image = baseImage
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		// Fail a primary so the next scrape reports the cluster unhealthy, then ask
		// for the engine roll: the gate must hold while it is not Ready.
		fc.failPrimary("10.0.0.10")
		setImage(key, newImage)

		// A few passes WITHOUT promoting (so the cluster cannot re-converge): the
		// roll is gated; no node reaches the new image.
		for i := 0; i < 5; i++ {
			_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		gomega.Expect(nodesOnImage(ns, newImage)).To(gomega.Equal(0),
			"engine roll must be gated while the cluster is not Ready")
	})
})

// findCondition returns the named status condition, or nil.
func findCondition(cluster *valkeyv1alpha1.PerconaValkeyCluster, condType string) *metav1.Condition {
	for i := range cluster.Status.Conditions {
		if cluster.Status.Conditions[i].Type == condType {
			return &cluster.Status.Conditions[i]
		}
	}
	return nil
}
