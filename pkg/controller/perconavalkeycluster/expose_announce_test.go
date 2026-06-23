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
	"errors"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// errInjected is the sentinel an interceptor returns to drive a reconcile error
// path; errNotIntercepted signals the interceptor to fall through to the real fake
// client for calls it does not target.
var (
	errInjected       = errors.New("injected client error")
	errNotIntercepted = errors.New("__not_intercepted__")
)

// newErrorReconciler builds a Reconciler over a fake client wrapped with the given
// interceptor funcs, so a test can inject a List/Get/Patch failure on a targeted
// object kind. An interceptor that returns errNotIntercepted delegates to the
// underlying fake client (the normal path for non-targeted calls).
func newErrorReconciler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *Reconciler {
	t.Helper()
	s := exposeTestScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	wrapped := interceptor.NewClient(base, delegateOnFallthrough(base, funcs))
	return &Reconciler{
		Client:             wrapped,
		scheme:             s,
		recorder:           events.NewFakeRecorder(200),
		skipNameValidation: true,
	}
}

// delegateOnFallthrough wraps each provided interceptor so a returned
// errNotIntercepted sentinel is translated into a real call on the underlying
// client (interceptor.Funcs has no built-in "pass through" once a func is set).
func delegateOnFallthrough(base client.WithWatch, funcs interceptor.Funcs) interceptor.Funcs {
	out := interceptor.Funcs{}
	if funcs.List != nil {
		inner := funcs.List
		out.List = func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := inner(ctx, c, list, opts...); err != errNotIntercepted {
				return err
			}
			return base.List(ctx, list, opts...)
		}
	}
	if funcs.Get != nil {
		inner := funcs.Get
		out.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := inner(ctx, c, key, obj, opts...); err != errNotIntercepted {
				return err
			}
			return base.Get(ctx, key, obj, opts...)
		}
	}
	if funcs.Patch != nil {
		inner := funcs.Patch
		out.Patch = func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := inner(ctx, c, obj, patch, opts...); err != errNotIntercepted {
				return err
			}
			return base.Patch(ctx, obj, patch, opts...)
		}
	}
	return out
}

// announceNodeName is the single-node (shard 0, node 0) name used across the
// expose-announce tests; the matching per-pod external Service is valkey-<name>-ext.
const announceNodeName = exposeTestCluster + "-0-0"

// makeValkeyNode builds a ValkeyNode for the given (shard, node) key of the test
// cluster, carrying the cluster topology labels the per-pod selector and
// reconcileExposeAnnounce match on, plus the FQDN announce default
// buildValkeyNodeSpec would have stamped.
func makeValkeyNode(key nodeKey) *valkeyv1alpha1.ValkeyNode {
	name := naming.NodeName(exposeTestCluster, key.shard, key.node)
	fqdn := perPodServiceName(name) + "." + exposeTestNS + ".svc"
	clientPort := int32(valkey.ClientPort)
	return &valkeyv1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: exposeTestNS,
			Labels: map[string]string{
				naming.LabelCluster:    exposeTestCluster,
				naming.LabelShardIndex: strconv.Itoa(key.shard),
				naming.LabelNodeIndex:  strconv.Itoa(key.node),
			},
		},
		Spec: valkeyv1alpha1.ValkeyNodeSpec{
			AnnounceHost: fqdn,
			AnnouncePort: &clientPort,
		},
	}
}

// makeLBService builds a per-pod LoadBalancer external Service for a node, with the
// given ingress address pre-populated in status (empty ingress => pending).
func makeLBService(node, ingressIP, ingressHost string) *corev1.Service {
	svc := externalPerPodService(node, corev1.ServiceTypeLoadBalancer)
	if ingressIP != "" || ingressHost != "" {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
			{IP: ingressIP, Hostname: ingressHost},
		}
	}
	return svc
}

// makeNodePortService builds the single-node (announceNodeName) per-pod NodePort
// external Service with the assigned client nodePort (0 => unassigned/pending).
func makeNodePortService(nodePort int32) *corev1.Service {
	svc := externalPerPodService(announceNodeName, corev1.ServiceTypeNodePort)
	svc.Spec.Ports[0].NodePort = nodePort
	return svc
}

// externalPerPodService builds the per-pod external Service skeleton (name, type,
// client port) reconcileExposeAnnounce reads. Only the named client port matters to
// the announce logic, so the bus port is intentionally omitted from the fixture.
func externalPerPodService(node string, typ corev1.ServiceType) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: perPodServiceName(node), Namespace: exposeTestNS},
		Spec: corev1.ServiceSpec{
			Type: typ,
			Ports: []corev1.ServicePort{
				{Name: valkeyPortName, Port: valkey.ClientPort},
			},
		},
	}
}

// makeBackingPod builds the pod backing a node's per-pod Service, carrying the
// per-pod topology selector labels and the given node (host) IP in status.
func makeBackingPod(cluster string, key nodeKey, hostIP string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.NodeName(cluster, key.shard, key.node) + "-0",
			Namespace: exposeTestNS,
			Labels:    perPodPodSelector(cluster, key),
		},
		Status: corev1.PodStatus{HostIP: hostIP},
	}
}

// getNode fetches a ValkeyNode by name in the test namespace.
func getNode(t *testing.T, c client.Client, name string) *valkeyv1alpha1.ValkeyNode {
	t.Helper()
	n := &valkeyv1alpha1.ValkeyNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: exposeTestNS}, n); err != nil {
		t.Fatalf("get node %s: %v", name, err)
	}
	return n
}

// TestReconcileExposeAnnounceNoopWhenNotWanted: with expose.perPod unset (or
// non-cluster / ClusterIP) the announce reconcile is a no-op — it never touches the
// node's announce fields.
func TestReconcileExposeAnnounceNoopWhenNotWanted(t *testing.T) {
	t.Parallel()
	cases := map[string]*valkeyv1alpha1.ExposeSpec{
		"nil expose":          nil,
		"clusterIP perPod":    {Type: corev1.ServiceTypeClusterIP, PerPod: true},
		"loadBalancer noPerP": {Type: corev1.ServiceTypeLoadBalancer},
	}
	for name, expose := range cases {
		t.Run(name, func(t *testing.T) {
			cl := exposeCluster(1, expose)
			node := makeValkeyNode(nodeKey{shard: 0, node: 0})
			node.Spec.AnnounceHost = ""
			node.Spec.AnnouncePort = nil
			// A populated LB Service exists, but the reconcile must ignore it.
			svc := makeLBService(announceNodeName, "203.0.113.7", "")
			r, c := newExposeReconciler(t, cl, node, svc)

			if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
				t.Fatalf("reconcileExposeAnnounce: %v", err)
			}
			got := getNode(t, c, announceNodeName)
			if got.Spec.AnnounceHost != "" || got.Spec.AnnouncePort != nil {
				t.Errorf("not-wanted must not announce, got host=%q port=%v", got.Spec.AnnounceHost, got.Spec.AnnouncePort)
			}
		})
	}
}

// TestReconcileExposeAnnounceLoadBalancerIP: a per-pod LoadBalancer Service whose
// status carries an ingress IP refines the node's announce host to that IP and the
// published client port.
func TestReconcileExposeAnnounceLoadBalancerIP(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	node := makeValkeyNode(nodeKey{shard: 0, node: 0})
	svc := makeLBService(announceNodeName, "203.0.113.42", "")
	r, c := newExposeReconciler(t, cl, node, svc)

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce: %v", err)
	}
	got := getNode(t, c, announceNodeName)
	if got.Spec.AnnounceHost != "203.0.113.42" {
		t.Errorf("announceHost = %q, want 203.0.113.42 (LB ingress IP)", got.Spec.AnnounceHost)
	}
	if got.Spec.AnnouncePort == nil || *got.Spec.AnnouncePort != valkey.ClientPort {
		t.Errorf("announcePort = %v, want %d (published client port)", got.Spec.AnnouncePort, valkey.ClientPort)
	}
}

// TestReconcileExposeAnnounceLoadBalancerHostname: when the LB ingress reports a
// hostname instead of an IP, the announce host is the hostname.
func TestReconcileExposeAnnounceLoadBalancerHostname(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	node := makeValkeyNode(nodeKey{shard: 0, node: 0})
	svc := makeLBService(announceNodeName, "", "lb-abc123.example.elb.amazonaws.com")
	r, c := newExposeReconciler(t, cl, node, svc)

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce: %v", err)
	}
	got := getNode(t, c, announceNodeName)
	if got.Spec.AnnounceHost != "lb-abc123.example.elb.amazonaws.com" {
		t.Errorf("announceHost = %q, want the LB hostname", got.Spec.AnnounceHost)
	}
}

// TestReconcileExposeAnnounceLoadBalancerPending: an LB Service with no ingress yet
// leaves the node's announce on its FQDN default (no error, no premature refine) —
// the owned-Service status watch re-enqueues when the address lands.
func TestReconcileExposeAnnounceLoadBalancerPending(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	node := makeValkeyNode(nodeKey{shard: 0, node: 0})
	fqdn := node.Spec.AnnounceHost // the FQDN default
	svc := makeLBService(announceNodeName, "", "")
	r, c := newExposeReconciler(t, cl, node, svc)

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce (pending): %v", err)
	}
	got := getNode(t, c, announceNodeName)
	if got.Spec.AnnounceHost != fqdn {
		t.Errorf("pending LB must leave the FQDN default, got %q want %q", got.Spec.AnnounceHost, fqdn)
	}
}

// TestReconcileExposeAnnounceNodePort: a per-pod NodePort Service with an assigned
// nodePort refines the node's announce to its backing pod's node (host) IP and the
// assigned nodePort.
func TestReconcileExposeAnnounceNodePort(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeNodePort, PerPod: true}
	cl := exposeCluster(1, expose)
	key := nodeKey{shard: 0, node: 0}
	node := makeValkeyNode(key)
	svc := makeNodePortService(31234)
	pod := makeBackingPod(exposeTestCluster, key, "10.0.0.9")
	r, c := newExposeReconciler(t, cl, node, svc, pod)

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce (nodePort): %v", err)
	}
	got := getNode(t, c, announceNodeName)
	if got.Spec.AnnounceHost != "10.0.0.9" {
		t.Errorf("announceHost = %q, want 10.0.0.9 (backing pod node IP)", got.Spec.AnnounceHost)
	}
	if got.Spec.AnnouncePort == nil || *got.Spec.AnnouncePort != 31234 {
		t.Errorf("announcePort = %v, want 31234 (assigned nodePort)", got.Spec.AnnouncePort)
	}
}

// TestReconcileExposeAnnounceNodePortPendingNodeIP: a NodePort Service whose backing
// pod has no node IP yet (unscheduled) leaves the announce on its FQDN default.
func TestReconcileExposeAnnounceNodePortPendingNodeIP(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeNodePort, PerPod: true}
	cl := exposeCluster(1, expose)
	key := nodeKey{shard: 0, node: 0}
	node := makeValkeyNode(key)
	fqdn := node.Spec.AnnounceHost
	svc := makeNodePortService(31234)
	pod := makeBackingPod(exposeTestCluster, key, "") // unscheduled: no hostIP
	r, c := newExposeReconciler(t, cl, node, svc, pod)

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce (nodePort pending): %v", err)
	}
	got := getNode(t, c, announceNodeName)
	if got.Spec.AnnounceHost != fqdn {
		t.Errorf("pending nodeIP must leave the FQDN default, got %q want %q", got.Spec.AnnounceHost, fqdn)
	}
}

// TestReconcileExposeAnnounceServiceMissing: when the per-pod Service does not exist
// yet (node predates this expose generation's Services) the reconcile defers without
// error and leaves the node's announce default in place.
func TestReconcileExposeAnnounceServiceMissing(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	node := makeValkeyNode(nodeKey{shard: 0, node: 0})
	fqdn := node.Spec.AnnounceHost
	r, c := newExposeReconciler(t, cl, node) // no Service seeded

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce (missing svc): %v", err)
	}
	got := getNode(t, c, announceNodeName)
	if got.Spec.AnnounceHost != fqdn {
		t.Errorf("missing Service must leave the FQDN default, got %q", got.Spec.AnnounceHost)
	}
}

// TestReconcileExposeAnnounceIdempotent: a second pass against an already-refined
// node makes no further change (only-if-differs; the resourceVersion is stable).
func TestReconcileExposeAnnounceIdempotent(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	node := makeValkeyNode(nodeKey{shard: 0, node: 0})
	svc := makeLBService(announceNodeName, "203.0.113.42", "")
	r, c := newExposeReconciler(t, cl, node, svc)

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce pass 1: %v", err)
	}
	first := getNode(t, c, announceNodeName)
	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce pass 2: %v", err)
	}
	second := getNode(t, c, announceNodeName)
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("second pass must not re-patch: rv %q -> %q", first.ResourceVersion, second.ResourceVersion)
	}
}

// TestReconcileExposeAnnounceMultiNode: with multiple per-pod Services each node is
// refined to its OWN external address (no cross-talk between nodes), and a node
// whose Service is still pending keeps its FQDN default while a resolved sibling is
// refined in the same pass.
func TestReconcileExposeAnnounceMultiNode(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose) // nodes c-0-0 (primary) + c-0-1 (replica)
	primaryKey := nodeKey{shard: 0, node: 0}
	replicaKey := nodeKey{shard: 0, node: 1}
	primary := makeValkeyNode(primaryKey)
	replica := makeValkeyNode(replicaKey)
	replicaFQDN := replica.Spec.AnnounceHost

	primaryName := naming.NodeName(exposeTestCluster, primaryKey.shard, primaryKey.node)
	replicaName := naming.NodeName(exposeTestCluster, replicaKey.shard, replicaKey.node)
	primarySvc := makeLBService(primaryName, "203.0.113.10", "")
	replicaSvc := makeLBService(replicaName, "", "") // still pending

	r, c := newExposeReconciler(t, cl, primary, replica, primarySvc, replicaSvc)

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce (multi): %v", err)
	}
	gotPrimary := getNode(t, c, primaryName)
	if gotPrimary.Spec.AnnounceHost != "203.0.113.10" {
		t.Errorf("primary announceHost = %q, want its own LB IP 203.0.113.10", gotPrimary.Spec.AnnounceHost)
	}
	gotReplica := getNode(t, c, replicaName)
	if gotReplica.Spec.AnnounceHost != replicaFQDN {
		t.Errorf("pending replica must keep FQDN default, got %q want %q", gotReplica.Spec.AnnounceHost, replicaFQDN)
	}
}

// TestLoadBalancerAnnounceUnit covers the pure LB extractor: IP wins over hostname,
// pending returns empty, and the published port is honoured.
func TestLoadBalancerAnnounceUnit(t *testing.T) {
	t.Parallel()
	// IP preferred over hostname.
	svc := makeLBService(announceNodeName, "198.51.100.5", "ignored.example.com")
	host, port := loadBalancerAnnounce(svc)
	if host != "198.51.100.5" {
		t.Errorf("host = %q, want the ingress IP", host)
	}
	if port == nil || *port != valkey.ClientPort {
		t.Errorf("port = %v, want %d", port, valkey.ClientPort)
	}
	// Pending => empty.
	if h, p := loadBalancerAnnounce(makeLBService(announceNodeName, "", "")); h != "" || p != nil {
		t.Errorf("pending must be empty, got host=%q port=%v", h, p)
	}
}

// TestServiceClientNodePortUnit covers the nodePort extractor: assigned returns the
// value, unassigned (0) and a missing named port return nil.
func TestServiceClientNodePortUnit(t *testing.T) {
	t.Parallel()
	if np := serviceClientNodePort(makeNodePortService(30007)); np == nil || *np != 30007 {
		t.Errorf("assigned nodePort = %v, want 30007", np)
	}
	if np := serviceClientNodePort(makeNodePortService(0)); np != nil {
		t.Errorf("unassigned nodePort must be nil, got %v", np)
	}
	noNamed := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "other", NodePort: 30009}}}}
	if np := serviceClientNodePort(noNamed); np != nil {
		t.Errorf("missing named client port must be nil, got %v", np)
	}
}

// TestPortsEqualUnit covers the optional-port comparator.
func TestPortsEqualUnit(t *testing.T) {
	t.Parallel()
	p1 := int32(6379)
	p2 := int32(6379)
	p3 := int32(16379)
	if !portsEqual(nil, nil) {
		t.Error("nil == nil must be equal")
	}
	if portsEqual(&p1, nil) || portsEqual(nil, &p1) {
		t.Error("nil vs set must be unequal")
	}
	if !portsEqual(&p1, &p2) {
		t.Error("same value must be equal")
	}
	if portsEqual(&p1, &p3) {
		t.Error("different values must be unequal")
	}
}

// TestReconcileExposeAnnounceNodePortPendingPort: a NodePort Service whose client
// nodePort is still unassigned (0) leaves the announce on its FQDN default even when
// the backing pod's node IP is already known.
func TestReconcileExposeAnnounceNodePortPendingPort(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeNodePort, PerPod: true}
	cl := exposeCluster(1, expose)
	key := nodeKey{shard: 0, node: 0}
	node := makeValkeyNode(key)
	fqdn := node.Spec.AnnounceHost
	svc := makeNodePortService(0) // nodePort not yet assigned
	pod := makeBackingPod(exposeTestCluster, key, "10.0.0.9")
	r, c := newExposeReconciler(t, cl, node, svc, pod)

	if err := r.reconcileExposeAnnounce(context.Background(), cl); err != nil {
		t.Fatalf("reconcileExposeAnnounce (nodePort pending port): %v", err)
	}
	got := getNode(t, c, announceNodeName)
	if got.Spec.AnnounceHost != fqdn {
		t.Errorf("pending nodePort must leave the FQDN default, got %q want %q", got.Spec.AnnounceHost, fqdn)
	}
}

// TestReconcileExposeAnnounceListNodesError: a List(nodes) failure surfaces as a
// wrapped error (the reconcile fails the pass rather than silently dropping work).
func TestReconcileExposeAnnounceListNodesError(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	r := newErrorReconciler(t, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			if _, ok := list.(*valkeyv1alpha1.ValkeyNodeList); ok {
				return errInjected
			}
			return errNotIntercepted
		},
	}, cl)

	err := r.reconcileExposeAnnounce(context.Background(), cl)
	if err == nil || !strings.Contains(err.Error(), "list nodes for expose-announce") {
		t.Fatalf("expected wrapped list-nodes error, got %v", err)
	}
}

// TestReconcileExposeAnnounceGetServiceError: a non-NotFound Get(Service) failure
// surfaces as a wrapped error (NotFound is benign; a real API error is not).
func TestReconcileExposeAnnounceGetServiceError(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	node := makeValkeyNode(nodeKey{shard: 0, node: 0})
	r := newErrorReconciler(t, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			if _, ok := obj.(*corev1.Service); ok {
				return errInjected
			}
			return errNotIntercepted
		},
	}, cl, node)

	err := r.reconcileExposeAnnounce(context.Background(), cl)
	if err == nil || !strings.Contains(err.Error(), "get per-pod service") {
		t.Fatalf("expected wrapped get-service error, got %v", err)
	}
}

// TestReconcileExposeAnnouncePatchError: a Patch(node) failure surfaces as a wrapped
// error so a persistence failure is not swallowed.
func TestReconcileExposeAnnouncePatchError(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeLoadBalancer, PerPod: true}
	cl := exposeCluster(1, expose)
	node := makeValkeyNode(nodeKey{shard: 0, node: 0})
	svc := makeLBService(announceNodeName, "203.0.113.42", "")
	r := newErrorReconciler(t, interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			if _, ok := obj.(*valkeyv1alpha1.ValkeyNode); ok {
				return errInjected
			}
			return errNotIntercepted
		},
	}, cl, node, svc)

	err := r.reconcileExposeAnnounce(context.Background(), cl)
	if err == nil || !strings.Contains(err.Error(), "patch announce on") {
		t.Fatalf("expected wrapped patch error, got %v", err)
	}
}

// TestReconcileExposeAnnounceListPodsError: a List(pods) failure during NodePort
// node-IP discovery surfaces as a wrapped error.
func TestReconcileExposeAnnounceListPodsError(t *testing.T) {
	t.Parallel()
	expose := &valkeyv1alpha1.ExposeSpec{Type: corev1.ServiceTypeNodePort, PerPod: true}
	cl := exposeCluster(1, expose)
	node := makeValkeyNode(nodeKey{shard: 0, node: 0})
	svc := makeNodePortService(31234)
	r := newErrorReconciler(t, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return errInjected
			}
			return errNotIntercepted
		},
	}, cl, node, svc)

	err := r.reconcileExposeAnnounce(context.Background(), cl)
	if err == nil || !strings.Contains(err.Error(), "list backing pod for") {
		t.Fatalf("expected wrapped list-pods error, got %v", err)
	}
}
