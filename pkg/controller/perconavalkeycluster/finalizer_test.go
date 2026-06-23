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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// certManagerCluster builds a cluster in cert-manager TLS mode.
func certManagerCluster(name string) *valkeyv1alpha1.PerconaValkeyCluster {
	c := aclTestCluster(name)
	c.Spec.TLS = &valkeyv1alpha1.TLSConfig{
		CertManager: &valkeyv1alpha1.CertManagerSpec{
			IssuerRef: valkeyv1alpha1.IssuerRef{Name: "issuer"},
		},
	}
	return c
}

// operatorTLSSecret builds the operator-issued TLS Secret (named
// naming.TLSSecretName) carrying the operator component labels cert-manager
// propagates from the Certificate.
func operatorTLSSecret(cluster string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.TLSSecretName(cluster),
			Namespace: aclTestNS,
			Labels:    naming.Labels(cluster, naming.ComponentValkey),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y"), "ca.crt": []byte("z")},
	}
}

func tlsSecretPresent(t *testing.T, r *Reconciler, cluster string) bool {
	t.Helper()
	s := &corev1.Secret{}
	err := r.Get(context.Background(), types.NamespacedName{Name: naming.TLSSecretName(cluster), Namespace: aclTestNS}, s)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get TLS secret: %v", err)
	}
	return true
}

// TestCleanupTLSDeletesOrphanedCertManagerSecret is the core teardown assertion:
// in cert-manager mode the operator-issued TLS Secret (owned by the Certificate,
// not the cluster, so owner-ref GC never reaps it) IS deleted by the finalizer.
func TestCleanupTLSDeletesOrphanedCertManagerSecret(t *testing.T) {
	t.Parallel()
	cluster := certManagerCluster("cm")
	r, _ := newACLReconciler(t, cluster, operatorTLSSecret("cm"))

	if !tlsSecretPresent(t, r, "cm") {
		t.Fatal("precondition: TLS secret should exist before cleanup")
	}
	if err := r.cleanupTLS(context.Background(), cluster); err != nil {
		t.Fatalf("cleanupTLS: %v", err)
	}
	if tlsSecretPresent(t, r, "cm") {
		t.Fatal("cert-manager TLS Secret was not deleted on teardown (leak)")
	}
}

// TestCleanupTLSIdempotentWhenSecretAbsent verifies the cleanup is a no-op (no
// error) when the Secret is already gone — a re-entrant teardown pass.
func TestCleanupTLSIdempotentWhenSecretAbsent(t *testing.T) {
	t.Parallel()
	cluster := certManagerCluster("gone")
	r, _ := newACLReconciler(t, cluster) // no TLS Secret seeded.

	if err := r.cleanupTLS(context.Background(), cluster); err != nil {
		t.Fatalf("cleanupTLS on absent Secret must be a no-op, got: %v", err)
	}
}

// TestCleanupTLSSkipsTLSOff verifies a TLS-off cluster has no material to clean
// up (the finalizer body short-circuits).
func TestCleanupTLSSkipsTLSOff(t *testing.T) {
	t.Parallel()
	cluster := aclTestCluster("plain") // Spec.TLS == nil.
	// Seed a same-named Secret to prove it is NOT touched when TLS is off.
	r, _ := newACLReconciler(t, cluster, operatorTLSSecret("plain"))

	if err := r.cleanupTLS(context.Background(), cluster); err != nil {
		t.Fatalf("cleanupTLS: %v", err)
	}
	if !tlsSecretPresent(t, r, "plain") {
		t.Fatal("TLS-off cluster must not delete any Secret")
	}
}

// TestCleanupTLSSkipsSecretRefMode verifies the user-owned (bring-your-own)
// Secret in secret-ref mode is NEVER deleted by the operator on teardown.
func TestCleanupTLSSkipsSecretRefMode(t *testing.T) {
	t.Parallel()
	cluster := aclTestCluster("byo")
	cluster.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "my-tls"}
	// Even if a same-named operator Secret existed, secret-ref mode must not delete.
	r, _ := newACLReconciler(t, cluster, operatorTLSSecret("byo"))

	if err := r.cleanupTLS(context.Background(), cluster); err != nil {
		t.Fatalf("cleanupTLS: %v", err)
	}
	if !tlsSecretPresent(t, r, "byo") {
		t.Fatal("secret-ref mode must never delete a Secret on teardown")
	}
}

// TestCleanupTLSLeavesUnlabeledSecretUntouched verifies the defensive guard: a
// same-named Secret that lacks the operator component label (e.g. a user's own
// Secret colliding on the name) is left untouched even in cert-manager mode, so
// teardown can never delete data the operator did not create.
func TestCleanupTLSLeavesUnlabeledSecretUntouched(t *testing.T) {
	t.Parallel()
	cluster := certManagerCluster("collide")
	unlabeled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.TLSSecretName("collide"),
			Namespace: aclTestNS,
			// No operator labels.
		},
		Type: corev1.SecretTypeTLS,
	}
	r, _ := newACLReconciler(t, cluster, unlabeled)

	if err := r.cleanupTLS(context.Background(), cluster); err != nil {
		t.Fatalf("cleanupTLS: %v", err)
	}
	if !tlsSecretPresent(t, r, "collide") {
		t.Fatal("an unlabeled (non-operator) Secret must not be deleted on teardown")
	}
}
