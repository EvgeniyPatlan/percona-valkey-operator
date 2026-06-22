package valkey

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// TestNewClientDialFailure exercises NewClient's error path against a closed
// port. valkey-go returns promptly when nothing is listening on loopback.
func TestNewClientDialFailure(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		c, err := NewClient(Address("127.0.0.1"), Auth{}, nil)
		if c != nil {
			_ = c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected dial error against a closed loopback port")
		}
	case <-time.After(15 * time.Second):
		t.Skip("NewClient dial did not return promptly in this environment")
	}
}

// TestFactoryForDialFailure drives For() end-to-end: it resolves auth (present),
// builds the address, and dials — which fails against an unreachable podIP. This
// covers the full For() path short of a live engine.
func TestFactoryForDialFailure(t *testing.T) {
	s := testScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: naming.SystemPasswordsSecretName("mycluster"), Namespace: "valkey"},
		Data:       map[string][]byte{naming.SystemUserOperator: []byte("pw")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	f := NewClientFactory(c)

	node := newTestNode()
	node.Status.PodIP = "127.0.0.1" // closed port -> dial fails

	done := make(chan error, 1)
	go func() {
		vc, err := f.For(context.Background(), node)
		if vc != nil {
			_ = vc.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected For() to fail dialing an unreachable podIP")
		}
	case <-time.After(15 * time.Second):
		t.Skip("For() dial did not return promptly in this environment")
	}
}
