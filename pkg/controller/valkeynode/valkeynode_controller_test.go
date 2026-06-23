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

package valkeynode_test

import (
	"context"
	"fmt"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/valkeynode"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// nodeBuilder constructs a ValkeyNode with the Charter topology labels.
func makeNode(name string) *valkeyv1alpha1.ValkeyNode {
	return &valkeyv1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				naming.LabelCluster:    "mycluster",
				naming.LabelShardIndex: "0",
				naming.LabelNodeIndex:  "0",
			},
		},
		Spec: valkeyv1alpha1.ValkeyNodeSpec{
			Image:               "percona/valkey:9.0",
			WorkloadType:        valkeyv1alpha1.WorkloadStatefulSet,
			ServerConfigMapName: "valkey-mycluster",
			ServerConfigHash:    "hash-1",
			Exporter:            valkeyv1alpha1.ExporterSpec{Enabled: true, Image: "percona/valkey-exporter:1"},
		},
	}
}

// reconcileN drives the reconciler request count times.
func reconcileN(r *valkeynode.Reconciler, key types.NamespacedName, n int) (ctrl.Result, error) {
	var res ctrl.Result
	var err error
	for i := 0; i < n; i++ {
		res, err = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		if err != nil {
			return res, err
		}
	}
	return res, err
}

// markPodReady creates a Ready pod matching the node selector (envtest has no
// kubelet, so the test simulates the pod the STS would create).
func markPodReady(ctx context.Context, node *valkeyv1alpha1.ValkeyNode, podIP string) {
	labels := map[string]string{
		naming.LabelAppInstance: node.Name,
		naming.LabelCluster:     node.Labels[naming.LabelCluster],
		naming.LabelShardIndex:  node.Labels[naming.LabelShardIndex],
		naming.LabelNodeIndex:   node.Labels[naming.LabelNodeIndex],
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: naming.NodeWorkloadName(node.Name) + "-0", Namespace: node.Namespace, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "x"}}},
	}
	gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = podIP
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	gomega.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
}

var _ = ginkgo.Describe("ValkeyNode controller", func() {
	var (
		mockCtrl *gomock.Controller
		factory  *valkey.MockClientFactory
		vc       *valkey.MockConfigClient
		r        *valkeynode.Reconciler
	)

	ginkgo.BeforeEach(func() {
		mockCtrl = gomock.NewController(ginkgo.GinkgoT())
		factory = valkey.NewMockClientFactory(mockCtrl)
		vc = valkey.NewMockConfigClient(mockCtrl)
		r = valkeynode.NewReconcilerForTest(k8sClient, apiScheme, factory)
	})

	ginkgo.AfterEach(func() { mockCtrl.Finish() })

	ginkgo.It("creates a 1-replica StatefulSet + PVC with owner refs (E1)", func() {
		node := makeNode("stsnode")
		node.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")}
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		sts := &appsv1.StatefulSet{}
		gomega.Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-stsnode", Namespace: "default"}, sts)
		}, timeout, interval).Should(gomega.Succeed())
		gomega.Expect(*sts.Spec.Replicas).To(gomega.Equal(int32(1)))
		gomega.Expect(sts.Spec.VolumeClaimTemplates).To(gomega.HaveLen(1))
		gomega.Expect(sts.Spec.VolumeClaimTemplates[0].Name).To(gomega.Equal("valkey-stsnode-data"))
		gomega.Expect(sts.OwnerReferences).To(gomega.HaveLen(1))
		gomega.Expect(sts.OwnerReferences[0].Name).To(gomega.Equal("stsnode"))
		gomega.Expect(*sts.OwnerReferences[0].Controller).To(gomega.BeTrue())
		gomega.Expect(sts.Spec.Template.Annotations[naming.AnnServerConfigHash]).To(gomega.Equal("hash-1"))
	})

	ginkgo.It("creates a Deployment with no PVC for cache nodes (E2)", func() {
		node := makeNode("depnode")
		node.Spec.WorkloadType = valkeyv1alpha1.WorkloadDeployment
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dep := &appsv1.Deployment{}
		gomega.Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-depnode", Namespace: "default"}, dep)
		}, timeout, interval).Should(gomega.Succeed())
		gomega.Expect(*dep.Spec.Replicas).To(gomega.Equal(int32(1)))

		// No PVC should exist.
		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-depnode-data", Namespace: "default"}, pvc)
		gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
	})

	ginkgo.It("derives ready/role/podIP from the live pod + INFO via the fake client (E3)", func() {
		node := makeNode("readynode")
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)

		// First reconcile: workload created, pod not ready yet.
		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		markPodReady(testCtx, node, "10.1.2.3")

		// Fake: INFO returns role:master -> primary; no live keys -> no ConfigSet.
		factory.EXPECT().For(gomock.Any(), gomock.Any()).Return(vc, nil).AnyTimes()
		vc.EXPECT().InfoReplication(gomock.Any()).Return(map[string]string{valkey.InfoKeyRole: valkey.InfoRoleMaster}, nil).AnyTimes()
		vc.EXPECT().Close().Return(nil).AnyTimes()

		_, err = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		got := &valkeyv1alpha1.ValkeyNode{}
		gomega.Eventually(func() bool {
			if err := k8sClient.Get(testCtx, key, got); err != nil {
				return false
			}
			return got.Status.Ready && got.Status.Role == valkeyv1alpha1.NodeRolePrimary && got.Status.PodIP == "10.1.2.3"
		}, timeout, interval).Should(gomega.BeTrue())
		gomega.Expect(got.Status.PodName).To(gomega.Equal("valkey-readynode-0"))
		gomega.Expect(got.Status.ObservedGeneration).To(gomega.Equal(got.Generation))
	})

	ginkgo.It("maps role:slave to replica (E3)", func() {
		node := makeNode("replicanode")
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)
		_, _ = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		markPodReady(testCtx, node, "10.1.2.4")

		factory.EXPECT().For(gomock.Any(), gomock.Any()).Return(vc, nil).AnyTimes()
		vc.EXPECT().InfoReplication(gomock.Any()).Return(map[string]string{valkey.InfoKeyRole: valkey.InfoRoleSlave}, nil).AnyTimes()
		vc.EXPECT().Close().Return(nil).AnyTimes()

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		got := &valkeyv1alpha1.ValkeyNode{}
		gomega.Eventually(func() valkeyv1alpha1.NodeRole {
			_ = k8sClient.Get(testCtx, key, got)
			return got.Status.Role
		}, timeout, interval).Should(gomega.Equal(valkeyv1alpha1.NodeRoleReplica))
	})

	ginkgo.It("applies a good live config via CONFIG SET with no roll (E4)", func() {
		node := makeNode("liveok")
		node.Spec.Config = map[string]string{"maxmemory": "200mb"}
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)
		_, _ = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})

		// capture the pod-template annotation BEFORE marking ready.
		sts := &appsv1.StatefulSet{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-liveok", Namespace: "default"}, sts)).To(gomega.Succeed())
		annoBefore := sts.Spec.Template.Annotations[naming.AnnServerConfigHash]

		markPodReady(testCtx, node, "10.1.2.5")

		factory.EXPECT().For(gomock.Any(), gomock.Any()).Return(vc, nil).AnyTimes()
		vc.EXPECT().ConfigSet(gomock.Any(), "maxmemory", "200mb").Return(nil).MinTimes(1)
		vc.EXPECT().InfoReplication(gomock.Any()).Return(map[string]string{valkey.InfoKeyRole: valkey.InfoRoleMaster}, nil).AnyTimes()
		vc.EXPECT().Close().Return(nil).AnyTimes()

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		got := &valkeyv1alpha1.ValkeyNode{}
		gomega.Eventually(func() bool {
			_ = k8sClient.Get(testCtx, key, got)
			return conditionIs(got, valkeyv1alpha1.NodeConditionLiveConfigApplied, metav1.ConditionTrue)
		}, timeout, interval).Should(gomega.BeTrue())

		// No roll: the config-hash annotation is unchanged by a live-config apply.
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-liveok", Namespace: "default"}, sts)).To(gomega.Succeed())
		gomega.Expect(sts.Spec.Template.Annotations[naming.AnnServerConfigHash]).To(gomega.Equal(annoBefore))
	})

	ginkgo.It("fails closed on a bad live config, blocking readiness (E4)", func() {
		node := makeNode("livebad")
		node.Spec.Config = map[string]string{"maxmemory": "not-a-size"}
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)
		_, _ = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		markPodReady(testCtx, node, "10.1.2.6")

		rec := events.NewFakeRecorder(20)
		r.RecorderForTest(rec)
		factory.EXPECT().For(gomock.Any(), gomock.Any()).Return(vc, nil).AnyTimes()
		vc.EXPECT().ConfigSet(gomock.Any(), "maxmemory", "not-a-size").Return(fmt.Errorf("ERR invalid value")).MinTimes(1)
		vc.EXPECT().Close().Return(nil).AnyTimes()

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred()) // fail-closed returns nil + requeue

		got := &valkeyv1alpha1.ValkeyNode{}
		gomega.Eventually(func() bool {
			_ = k8sClient.Get(testCtx, key, got)
			return !got.Status.Ready && conditionIs(got, valkeyv1alpha1.NodeConditionLiveConfigApplied, metav1.ConditionFalse)
		}, timeout, interval).Should(gomega.BeTrue())

		// Warning event emitted.
		gomega.Eventually(rec.Events, timeout, interval).Should(gomega.Receive(gomega.ContainSubstring("LiveConfigApplyFailed")))

		// Stays not-ready across another reconcile (no auto-remediation).
		_, err = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_ = k8sClient.Get(testCtx, key, got)
		gomega.Expect(got.Status.Ready).To(gomega.BeFalse())
	})

	ginkgo.It("rolls the pod template on a serverConfigHash change (E5)", func() {
		node := makeNode("rollnode")
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)
		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		sts := &appsv1.StatefulSet{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-rollnode", Namespace: "default"}, sts)).To(gomega.Succeed())
		gomega.Expect(sts.Spec.Template.Annotations[naming.AnnServerConfigHash]).To(gomega.Equal("hash-1"))

		// Parent stamps a new hash.
		gomega.Expect(k8sClient.Get(testCtx, key, node)).To(gomega.Succeed())
		node.Spec.ServerConfigHash = "hash-2"
		gomega.Expect(k8sClient.Update(testCtx, node)).To(gomega.Succeed())

		_, err = r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() string {
			_ = k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-rollnode", Namespace: "default"}, sts)
			return sts.Spec.Template.Annotations[naming.AnnServerConfigHash]
		}, timeout, interval).Should(gomega.Equal("hash-2"))
	})

	ginkgo.It("skips ConfigMap creation when serverConfigMapName is set", func() {
		node := makeNode("nocm")
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)
		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// No per-node ConfigMap created (parent owns valkey-mycluster).
		cm := &corev1.ConfigMap{}
		err = k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-nocm", Namespace: "default"}, cm)
		gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
	})

	ginkgo.It("renders its own ConfigMap when serverConfigMapName is empty", func() {
		node := makeNode("owncm")
		node.Spec.ServerConfigMapName = ""
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)
		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		cm := &corev1.ConfigMap{}
		gomega.Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-owncm", Namespace: "default"}, cm)
		}, timeout, interval).Should(gomega.Succeed())
		gomega.Expect(cm.OwnerReferences).To(gomega.HaveLen(1))
	})

	ginkgo.It("adds the cleanup finalizer for Delete reclaim and deletes the PVC on teardown (E7)", func() {
		node := makeNode("delreclaim")
		node.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{
			Size:          resource.MustParse("1Gi"),
			ReclaimPolicy: valkeyv1alpha1.ReclaimDelete,
		}
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)

		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() bool {
			n := &valkeyv1alpha1.ValkeyNode{}
			_ = k8sClient.Get(testCtx, key, n)
			return containsFinalizer(n.Finalizers, naming.FinalizerPVCCleanup)
		}, timeout, interval).Should(gomega.BeTrue())

		// Settle the workload (creates the STS, which would create the PVC). Create
		// the PVC manually since envtest has no STS controller.
		// The StatefulSet controller would materialize the volumeClaimTemplate as
		// <vctName>-<stsName>-0; replicate that name here since envtest has no STS
		// controller.
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "valkey-delreclaim-data-valkey-delreclaim-0", Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, pvc)).To(gomega.Succeed())

		// Delete the node -> teardown deletes the PVC then drops the finalizer.
		gomega.Expect(k8sClient.Delete(testCtx, node)).To(gomega.Succeed())
		_, err = reconcileN(r, key, 3)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		gomega.Eventually(func() bool {
			p := &corev1.PersistentVolumeClaim{}
			err := k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-delreclaim-data-valkey-delreclaim-0", Namespace: "default"}, p)
			return apierrors.IsNotFound(err) || !p.DeletionTimestamp.IsZero()
		}, timeout, interval).Should(gomega.BeTrue())
	})

	ginkgo.It("does not add the finalizer for Retain reclaim (E7)", func() {
		node := makeNode("retainnode")
		node.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{
			Size:          resource.MustParse("1Gi"),
			ReclaimPolicy: valkeyv1alpha1.ReclaimRetain,
		}
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(node)
		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		n := &valkeyv1alpha1.ValkeyNode{}
		gomega.Expect(k8sClient.Get(testCtx, key, n)).To(gomega.Succeed())
		gomega.Expect(containsFinalizer(n.Finalizers, naming.FinalizerPVCCleanup)).To(gomega.BeFalse())
	})

})

func conditionIs(node *valkeyv1alpha1.ValkeyNode, condType string, status metav1.ConditionStatus) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == condType {
			return c.Status == status
		}
	}
	return false
}

func containsFinalizer(finalizers []string, f string) bool {
	for _, x := range finalizers {
		if x == f {
			return true
		}
	}
	return false
}
