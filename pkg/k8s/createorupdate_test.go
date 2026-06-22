package k8s_test

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := valkeyv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func ownerNode() *valkeyv1alpha1.ValkeyNode {
	return &valkeyv1alpha1.ValkeyNode{
		TypeMeta:   metav1.TypeMeta{Kind: "ValkeyNode", APIVersion: valkeyv1alpha1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster-0-0", Namespace: "valkey", UID: "uid-1"},
	}
}

func TestCreateOrUpdateCreatesWithOwnerRef(t *testing.T) {
	s := scheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	owner := ownerNode()

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "valkey-cm", Namespace: "valkey"}}
	res, err := k8s.CreateOrUpdate(context.Background(), c, s, owner, cm, func() error {
		cm.Data = map[string]string{"k": "v"}
		return nil
	})
	if err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if res != controllerutil.OperationResultCreated {
		t.Errorf("op = %v, want created", res)
	}

	got := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "valkey-cm", Namespace: "valkey"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Data["k"] != "v" {
		t.Errorf("data = %v", got.Data)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "mycluster-0-0" {
		t.Fatalf("owner refs = %v", got.OwnerReferences)
	}
	or := got.OwnerReferences[0]
	if or.Controller == nil || !*or.Controller || or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
		t.Errorf("owner ref must be controller+blockOwnerDeletion: %+v", or)
	}
}

func TestCreateOrUpdateUpdates(t *testing.T) {
	s := scheme(t)
	owner := ownerNode()
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey-cm", Namespace: "valkey"},
		Data:       map[string]string{"k": "old"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "valkey-cm", Namespace: "valkey"}}
	res, err := k8s.CreateOrUpdate(context.Background(), c, s, owner, cm, func() error {
		cm.Data = map[string]string{"k": "new"}
		return nil
	})
	if err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if res != controllerutil.OperationResultUpdated {
		t.Errorf("op = %v, want updated", res)
	}
	got := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "valkey-cm", Namespace: "valkey"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Data["k"] != "new" {
		t.Errorf("data = %v, want new", got.Data)
	}
}

func TestCreateOrUpdateNoOp(t *testing.T) {
	s := scheme(t)
	owner := ownerNode()
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "valkey-cm", Namespace: "valkey"}}
	mutate := func() error { cm.Data = map[string]string{"k": "v"}; return nil }
	if _, err := k8s.CreateOrUpdate(context.Background(), c, s, owner, cm, mutate); err != nil {
		t.Fatal(err)
	}
	cm2 := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "valkey-cm", Namespace: "valkey"}}
	res, err := k8s.CreateOrUpdate(context.Background(), c, s, owner, cm2, mutate)
	if err != nil {
		t.Fatal(err)
	}
	if res != controllerutil.OperationResultNone {
		t.Errorf("second pass op = %v, want none", res)
	}
}

func TestWriteStatus(t *testing.T) {
	s := scheme(t)
	node := &valkeyv1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster-0-0", Namespace: "valkey"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(node).WithStatusSubresource(node).Build()

	// Mutate in-memory status, then write back.
	node.Status.Ready = true
	node.Status.Role = valkeyv1alpha1.NodeRolePrimary
	node.Status.PodIP = "10.0.0.9"
	if err := k8s.WriteStatus(context.Background(), c, node, func(n *valkeyv1alpha1.ValkeyNode) *valkeyv1alpha1.ValkeyNodeStatus {
		return &n.Status
	}); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}

	got := &valkeyv1alpha1.ValkeyNode{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(node), got); err != nil {
		t.Fatal(err)
	}
	if !got.Status.Ready || got.Status.Role != valkeyv1alpha1.NodeRolePrimary || got.Status.PodIP != "10.0.0.9" {
		t.Errorf("status not persisted: %+v", got.Status)
	}
}

func TestSetControllerOwnerRef(t *testing.T) {
	s := scheme(t)
	owner := ownerNode()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "valkey"}}
	if err := k8s.SetControllerOwnerRef(owner, cm, s); err != nil {
		t.Fatalf("SetControllerOwnerRef: %v", err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != "mycluster-0-0" {
		t.Errorf("owner ref not set: %v", cm.OwnerReferences)
	}
}

func TestCreateOrUpdateMutateError(t *testing.T) {
	s := scheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "valkey"}}
	_, err := k8s.CreateOrUpdate(context.Background(), c, s, ownerNode(), cm, func() error {
		return fmt.Errorf("boom")
	})
	if err == nil {
		t.Error("expected mutate error to propagate")
	}
}

func TestWriteStatusRefetchMissing(t *testing.T) {
	s := scheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	// Object never created -> re-fetch fails.
	node := &valkeyv1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: "valkey"}}
	err := k8s.WriteStatus(context.Background(), c, node, func(n *valkeyv1alpha1.ValkeyNode) *valkeyv1alpha1.ValkeyNodeStatus {
		return &n.Status
	})
	if err == nil {
		t.Error("expected re-fetch error for a missing object")
	}
}
