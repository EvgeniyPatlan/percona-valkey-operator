package valkey

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("client-go scheme: %v", err)
	}
	if err := valkeyv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("valkey scheme: %v", err)
	}
	return s
}

func newTestNode() *valkeyv1alpha1.ValkeyNode {
	return &valkeyv1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mycluster-0-0",
			Namespace: "valkey",
			Labels:    map[string]string{naming.LabelCluster: "mycluster"},
		},
	}
}

// factoryInternal exposes the unexported resolvers for direct unit testing
// without dialing a live engine (NewClient would attempt a real connection).
func factoryInternal(f ClientFactory) *factory { return f.(*factory) }

func TestFactoryResolveAuth(t *testing.T) {
	s := testScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: naming.SystemPasswordsSecretName("mycluster"), Namespace: "valkey"},
		Data:       map[string][]byte{naming.SystemUserOperator: []byte("s3cr3t")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	f := factoryInternal(NewClientFactory(c))

	auth := f.resolveAuth(context.Background(), "mycluster", "valkey")
	if auth.Username != naming.SystemUserOperator || auth.Password != "s3cr3t" {
		t.Errorf("resolveAuth = %+v, want _operator/s3cr3t", auth)
	}
}

func TestFactoryResolveAuthMissing(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	f := factoryInternal(NewClientFactory(c))
	auth := f.resolveAuth(context.Background(), "mycluster", "valkey")
	if auth.Username != "" || auth.Password != "" {
		t.Errorf("missing secret should yield empty auth, got %+v", auth)
	}
}

func TestFactoryResolveTLSDisabled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	f := factoryInternal(NewClientFactory(c))
	cfg, err := f.resolveTLS(context.Background(), newTestNode(), "mycluster")
	if err != nil {
		t.Fatalf("resolveTLS err: %v", err)
	}
	if cfg != nil {
		t.Error("TLS disabled should yield nil config")
	}
}

func TestFactoryResolveTLSFromSecret(t *testing.T) {
	s := testScheme(t)
	node := newTestNode()
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "mycluster-tls"}

	caPEM, certPEM, keyPEM := generateTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster-tls", Namespace: "valkey"},
		Data: map[string][]byte{
			tlsSecretKeyCA:   caPEM,
			tlsSecretKeyCert: certPEM,
			tlsSecretKeyKey:  keyPEM,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	f := factoryInternal(NewClientFactory(c))

	cfg, err := f.resolveTLS(context.Background(), node, "mycluster")
	if err != nil {
		t.Fatalf("resolveTLS err: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected TLS config")
	}
	if cfg.ServerName != "valkey-mycluster.valkey.svc" {
		t.Errorf("ServerName = %q, want valkey-mycluster.valkey.svc", cfg.ServerName)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("expected 1 client cert, got %d", len(cfg.Certificates))
	}
	if cfg.RootCAs == nil {
		t.Error("expected RootCAs to be populated")
	}
}

func TestFactoryResolveTLSMissingSecret(t *testing.T) {
	node := newTestNode()
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "nope"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	f := factoryInternal(NewClientFactory(c))
	if _, err := f.resolveTLS(context.Background(), node, "mycluster"); err == nil {
		t.Error("expected error for missing TLS secret")
	}
}

func TestFactoryForNoPodIP(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	f := NewClientFactory(c)
	if _, err := f.For(context.Background(), newTestNode()); err == nil {
		t.Error("expected error when podIP is empty")
	}
}
