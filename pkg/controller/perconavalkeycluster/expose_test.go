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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// exposeTestNS is the namespace used across the expose controller unit tests.
const exposeTestNS = "ns"

// exposeTestCluster is the cluster name used across the expose controller unit
// tests (a single fixed name keeps Service-name assertions readable).
const exposeTestCluster = "c"

// exposeTestScheme registers the API + core types the expose reconcile and its
// fake client need (Service create/update + owner-ref to the cluster).
func exposeTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := valkeyv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("api scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	return s
}

// newExposeReconciler builds a Reconciler backed by a controller-runtime fake
// client seeded with objs, mirroring the TLS-leg unit-test harness.
func newExposeReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	s := exposeTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Reconciler{
		Client:             c,
		scheme:             s,
		recorder:           events.NewFakeRecorder(200),
		skipNameValidation: true,
	}, c
}

// exposeCluster builds a cluster-mode PerconaValkeyCluster named
// exposeTestCluster with the given expose block and shard count (one replica per
// shard, so the desired per-pod Service set is 2*shards nodes).
func exposeCluster(shards int32, expose *valkeyv1alpha1.ExposeSpec) *valkeyv1alpha1.PerconaValkeyCluster {
	return &valkeyv1alpha1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: exposeTestCluster, Namespace: exposeTestNS},
		Spec: valkeyv1alpha1.PerconaValkeyClusterSpec{
			Mode:     valkeyv1alpha1.ModeCluster,
			Shards:   shards,
			Replicas: 1,
			Expose:   expose,
		},
	}
}

// getService fetches a Service by name in the test namespace, or returns ok=false.
func getService(t *testing.T, c client.Client, name string) (*corev1.Service, bool) {
	t.Helper()
	svc := &corev1.Service{}
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: exposeTestNS}, svc)
	if err != nil {
		return nil, false
	}
	return svc, true
}

// TestReconcileExposeNilNoop: spec.expose nil => no external Service, no error.
func TestReconcileExposeNilNoop(t *testing.T) {
	t.Parallel()
	cl := exposeCluster(1, nil)
	r, c := newExposeReconciler(t, cl)

	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (nil): %v", err)
	}
	if _, ok := getService(t, c, clientServiceName("c")); ok {
		t.Fatal("nil expose must not create an external client Service")
	}
}

// TestReconcileExposeClusterIPNoop: explicit ClusterIP type stays in-cluster.
func TestReconcileExposeClusterIPNoop(t *testing.T) {
	t.Parallel()
	cl := exposeCluster(1, &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeClusterIP})
	r, c := newExposeReconciler(t, cl)

	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (ClusterIP): %v", err)
	}
	if _, ok := getService(t, c, clientServiceName("c")); ok {
		t.Fatal("ClusterIP expose must not create an external Service")
	}
}

// TestReconcileExposeLoadBalancer: type LoadBalancer renders the aggregate client
// Service with the source ranges + annotations and an owner-ref to the cluster.
func TestReconcileExposeLoadBalancer(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{
		Type:                     corev1.ServiceTypeLoadBalancer,
		LoadBalancerSourceRanges: []string{"203.0.113.0/24"},
		Annotations:              map[string]string{"service.beta.kubernetes.io/aws-load-balancer-internal": "true"},
	}
	cl := exposeCluster(1, expose)
	r, c := newExposeReconciler(t, cl)

	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (LB): %v", err)
	}
	svc, ok := getService(t, c, clientServiceName("c"))
	if !ok {
		t.Fatal("LoadBalancer expose must create the aggregate client Service")
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %q, want LoadBalancer", svc.Spec.Type)
	}
	if len(svc.Spec.LoadBalancerSourceRanges) != 1 || svc.Spec.LoadBalancerSourceRanges[0] != "203.0.113.0/24" {
		t.Errorf("sourceRanges = %v, want [203.0.113.0/24]", svc.Spec.LoadBalancerSourceRanges)
	}
	if svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-internal"] != "true" {
		t.Errorf("annotation not propagated: %v", svc.Annotations)
	}
	if svc.Spec.Selector[clusterLabel()] != "c" {
		t.Errorf("selector = %v, want cluster=c", svc.Spec.Selector)
	}
	if !hasPortValue(svc.Spec.Ports, 6379) || !hasPortValue(svc.Spec.Ports, 16379) {
		t.Errorf("ports = %v, want client 6379 + bus 16379", svc.Spec.Ports)
	}
	if len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].Name != "c" {
		t.Errorf("owner refs = %v, want one ref to cluster c", svc.OwnerReferences)
	}
}

// TestReconcileExposeNodePortNoSourceRanges: a NodePort Service must NOT carry
// loadBalancerSourceRanges (they only bind for LoadBalancer).
func TestReconcileExposeNodePortNoSourceRanges(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{
		Type:                     corev1.ServiceTypeNodePort,
		LoadBalancerSourceRanges: []string{"203.0.113.0/24"},
	}
	cl := exposeCluster(1, expose)
	r, c := newExposeReconciler(t, cl)

	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (NodePort): %v", err)
	}
	svc, ok := getService(t, c, clientServiceName("c"))
	if !ok {
		t.Fatal("NodePort expose must create the aggregate client Service")
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("type = %q, want NodePort", svc.Spec.Type)
	}
	if len(svc.Spec.LoadBalancerSourceRanges) != 0 {
		t.Errorf("NodePort must not carry sourceRanges, got %v", svc.Spec.LoadBalancerSourceRanges)
	}
}

// TestReconcileExposePerPodClusterMode: perPod=true in cluster mode renders one
// external Service per desired ValkeyNode, each selecting exactly that pod.
func TestReconcileExposePerPodClusterMode(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(2, expose) // 2 shards * (1 primary + 1 replica) = 4 nodes
	r, c := newExposeReconciler(t, cl)

	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (perPod): %v", err)
	}
	// The aggregate Service must NOT exist in per-pod mode.
	if _, ok := getService(t, c, clientServiceName("c")); ok {
		t.Fatal("per-pod mode must not create the aggregate client Service")
	}
	// One Service per node: c-0-0, c-0-1, c-1-0, c-1-1 -> valkey-<node>-ext.
	wantNodes := []string{"c-0-0", "c-0-1", "c-1-0", "c-1-1"}
	for _, node := range wantNodes {
		svc, ok := getService(t, c, perPodServiceName(node))
		if !ok {
			t.Fatalf("missing per-pod Service for node %s", node)
		}
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			t.Errorf("%s type = %q, want LoadBalancer", node, svc.Spec.Type)
		}
		if svc.Spec.Selector[nodeIndexLabel()] == "" || svc.Spec.Selector[shardIndexLabel()] == "" {
			t.Errorf("%s selector must pin shard+node index, got %v", node, svc.Spec.Selector)
		}
	}
	// Exactly four external Services exist (no extras).
	list := &corev1.ServiceList{}
	if err := c.List(context.Background(), list, client.InNamespace(exposeTestNS)); err != nil {
		t.Fatalf("list services: %v", err)
	}
	if got := len(list.Items); got != len(wantNodes) {
		t.Errorf("external Service count = %d, want %d", got, len(wantNodes))
	}
}

// TestReconcileExposePerPodIgnoredOutsideClusterMode: perPod in replication mode
// falls back to the single aggregate Service (per-pod announce is cluster-only).
func TestReconcileExposePerPodIgnoredOutsideClusterMode(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	cl.Spec.Mode = valkeyv1alpha1.ModeReplication
	r, c := newExposeReconciler(t, cl)

	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (perPod replication): %v", err)
	}
	if _, ok := getService(t, c, clientServiceName("c")); !ok {
		t.Fatal("perPod outside cluster mode must still create the aggregate Service")
	}
	if _, ok := getService(t, c, perPodServiceName("c-0-0")); ok {
		t.Fatal("perPod outside cluster mode must not create per-pod Services")
	}
}

// TestReconcileExposePrunesOnDisable: turning expose off (back to ClusterIP)
// deletes a previously-created external Service (no orphans).
func TestReconcileExposePrunesOnDisable(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer}
	cl := exposeCluster(1, expose)
	r, c := newExposeReconciler(t, cl)

	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (enable): %v", err)
	}
	if _, ok := getService(t, c, clientServiceName("c")); !ok {
		t.Fatal("expected the external Service to exist after enable")
	}

	// Flip expose back to ClusterIP and reconcile: the Service must be pruned.
	cl.Spec.Expose = &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeClusterIP}
	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (disable): %v", err)
	}
	if _, ok := getService(t, c, clientServiceName("c")); ok {
		t.Fatal("disabling expose must prune the external Service")
	}
}

// TestReconcileExposePrunesStalePerPodOnAggregateSwitch: switching from per-pod
// to aggregate prunes the stale per-pod Services and leaves only the aggregate.
func TestReconcileExposePrunesStalePerPodOnAggregateSwitch(t *testing.T) {
	t.Parallel()
	perPod := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, perPod) // nodes c-0-0, c-0-1
	r, c := newExposeReconciler(t, cl)

	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (perPod): %v", err)
	}
	if _, ok := getService(t, c, perPodServiceName("c-0-0")); !ok {
		t.Fatal("expected per-pod Service before the switch")
	}

	// Switch to aggregate: per-pod Services must be pruned, aggregate created.
	cl.Spec.Expose = &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer}
	if err := r.reconcileExpose(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExpose (aggregate): %v", err)
	}
	if _, ok := getService(t, c, clientServiceName("c")); !ok {
		t.Fatal("aggregate Service must exist after the switch")
	}
	if _, ok := getService(t, c, perPodServiceName("c-0-0")); ok {
		t.Fatal("stale per-pod Service must be pruned after the switch to aggregate")
	}
	if _, ok := getService(t, c, perPodServiceName("c-0-1")); ok {
		t.Fatal("stale per-pod Service must be pruned after the switch to aggregate")
	}
}

// TestReconcileExposeIdempotent: a second reconcile must not error or churn.
func TestReconcileExposeIdempotent(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeNodePort}
	cl := exposeCluster(1, expose)
	r, c := newExposeReconciler(t, cl)

	for i := 0; i < 3; i++ {
		if err := r.reconcileExpose(context.Background(), cl); err != nil {
			t.Fatalf("reconcileExpose pass %d: %v", i, err)
		}
	}
	if _, ok := getService(t, c, clientServiceName("c")); !ok {
		t.Fatal("aggregate Service must persist across idempotent reconciles")
	}
	list := &corev1.ServiceList{}
	if err := c.List(context.Background(), list, client.InNamespace(exposeTestNS)); err != nil {
		t.Fatalf("list services: %v", err)
	}
	if got := len(list.Items); got != 1 {
		t.Errorf("idempotent reconcile produced %d Services, want 1", got)
	}
}

// hasPortValue reports whether the port list contains a port with the given value.
func hasPortValue(ports []corev1.ServicePort, value int32) bool {
	for i := range ports {
		if ports[i].Port == value {
			return true
		}
	}
	return false
}

// clusterLabel/shardIndexLabel/nodeIndexLabel mirror the naming label keys for
// assertion readability without importing the naming package into the test.
func clusterLabel() string    { return "valkey.percona.com/cluster" }
func shardIndexLabel() string { return "valkey.percona.com/shard-index" }
func nodeIndexLabel() string  { return "valkey.percona.com/node-index" }
