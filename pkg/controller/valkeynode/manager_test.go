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
	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// These specs drive the shared manager (started in BeforeSuite) so the
// SetupWithManager Owns/Watches wiring is exercised end-to-end: object creates
// trigger reconciles without manual r.Reconcile calls (04 §5 View B).
var _ = ginkgo.Describe("ValkeyNode controller via the shared manager", func() {
	ginkgo.It("reconciles on create (Owns) and a Pod Ready event re-enqueues (E10)", func() {
		node := &valkeyv1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mgrnode",
				Namespace: mgrNamespace,
				Labels: map[string]string{
					naming.LabelCluster: "mycluster", naming.LabelShardIndex: "9", naming.LabelNodeIndex: "0",
				},
			},
			Spec: valkeyv1alpha1.ValkeyNodeSpec{
				Image: "percona/percona-valkey:9.0", WorkloadType: valkeyv1alpha1.WorkloadStatefulSet,
				ServerConfigMapName: "valkey-mycluster", ServerConfigHash: "h",
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())

		// Owns(StatefulSet): the manager reconcile creates the workload.
		gomega.Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-mgrnode", Namespace: mgrNamespace}, &appsv1.StatefulSet{})
		}, timeout, interval).Should(gomega.Succeed())

		// Create a Ready pod matching the node selector; the Watch(Pod) mapping
		// re-enqueues the owning node so status flips to ready/role without the
		// 60s steady wait.
		sts := &appsv1.StatefulSet{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-mgrnode", Namespace: mgrNamespace}, sts)).To(gomega.Succeed())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "valkey-mgrnode-0", Namespace: mgrNamespace,
				Labels: map[string]string{
					naming.LabelAppInstance: "mgrnode",
					naming.LabelCluster:     "mycluster", naming.LabelShardIndex: "9", naming.LabelNodeIndex: "0",
				},
				// Owner-ref to the STS so the mapper resolves Pod -> STS -> ValkeyNode.
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID,
					Controller: ptrToBool(true), BlockOwnerDeletion: ptrToBool(true),
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "x"}}},
		}
		gomega.Expect(k8sClient.Create(testCtx, pod)).To(gomega.Succeed())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = "10.9.9.9"
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		gomega.Expect(k8sClient.Status().Update(testCtx, pod)).To(gomega.Succeed())

		got := &valkeyv1alpha1.ValkeyNode{}
		gomega.Eventually(func() bool {
			if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(node), got); err != nil {
				return false
			}
			return got.Status.Ready && got.Status.Role == valkeyv1alpha1.NodeRolePrimary && got.Status.PodIP == "10.9.9.9"
		}, timeout, interval).Should(gomega.BeTrue())
	})

	ginkgo.It("surfaces PVC ready/size-ready conditions from the live PVC (E6)", func() {
		node := &valkeyv1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvcnode",
				Namespace: mgrNamespace,
				Labels: map[string]string{
					naming.LabelCluster: "mycluster", naming.LabelShardIndex: "8", naming.LabelNodeIndex: "0",
				},
			},
			Spec: valkeyv1alpha1.ValkeyNodeSpec{
				Image: "percona/percona-valkey:9.0", WorkloadType: valkeyv1alpha1.WorkloadStatefulSet,
				ServerConfigMapName: "valkey-mycluster", ServerConfigHash: "h",
				Persistence: &valkeyv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.Succeed())

		gomega.Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-pvcnode", Namespace: mgrNamespace}, &appsv1.StatefulSet{})
		}, timeout, interval).Should(gomega.Succeed())

		// Before the PVC exists, PVCReady should be False (pending). The STS
		// controller is absent in envtest, so we exercise the condition derivation
		// by creating the PVC and binding it manually.
		gomega.Eventually(func() bool {
			got := &valkeyv1alpha1.ValkeyNode{}
			if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(node), got); err != nil {
				return false
			}
			return conditionIs(got, valkeyv1alpha1.NodeConditionPVCReady, metav1.ConditionFalse)
		}, timeout, interval).Should(gomega.BeTrue())

		// Create + bind the PVC the STS would normally provision. The STS controller
		// materializes the volumeClaimTemplate as <vctName>-<stsName>-0, which is the
		// name the node controller reads — so use that name here (envtest has no STS
		// controller to create it).
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "valkey-pvcnode-data-valkey-pvcnode-0", Namespace: mgrNamespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, pvc)).To(gomega.Succeed())
		pvc.Status.Phase = corev1.ClaimBound
		pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
		gomega.Expect(k8sClient.Status().Update(testCtx, pvc)).To(gomega.Succeed())

		// Touch the node so the manager reconciles against the now-bound PVC.
		gomega.Eventually(func() error {
			cur := &valkeyv1alpha1.ValkeyNode{}
			if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(node), cur); err != nil {
				return err
			}
			if cur.Annotations == nil {
				cur.Annotations = map[string]string{}
			}
			cur.Annotations["test/poke"] = "1"
			return k8sClient.Update(testCtx, cur)
		}, timeout, interval).Should(gomega.Succeed())

		gomega.Eventually(func() bool {
			got := &valkeyv1alpha1.ValkeyNode{}
			if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(node), got); err != nil {
				return false
			}
			return conditionIs(got, valkeyv1alpha1.NodeConditionPVCReady, metav1.ConditionTrue) &&
				conditionIs(got, valkeyv1alpha1.NodeConditionPVCSizeReady, metav1.ConditionTrue)
		}, timeout, interval).Should(gomega.BeTrue())
	})

	ginkgo.It("rejects persistence with a Deployment workload via CEL (E2)", func() {
		node := &valkeyv1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "badcombo", Namespace: mgrNamespace},
			Spec: valkeyv1alpha1.ValkeyNodeSpec{
				WorkloadType: valkeyv1alpha1.WorkloadDeployment,
				Persistence:  &valkeyv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, node)).To(gomega.HaveOccurred())
	})

	ginkgo.It("maps an unrelated pod to no request", func() {
		// A pod with no ValkeyNode owner chain must not enqueue anything; covered
		// indirectly here by creating a stray pod and asserting no panic / no STS.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: mgrNamespace, Labels: map[string]string{"app": "other"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
		}
		gomega.Expect(k8sClient.Create(testCtx, pod)).To(gomega.Succeed())
		gomega.Consistently(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-stray", Namespace: mgrNamespace}, &appsv1.StatefulSet{})
		}, "1s", interval).ShouldNot(gomega.Succeed())
	})
})

func ptrToBool(b bool) *bool { return &b }

// ensure valkey import is used even if specs change.
var _ = valkey.ClientPort
