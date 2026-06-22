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

package tls

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// testScheme registers the API types plus the unstructured cert-manager
// Certificate GVK so the fake client can store/read it without a typed dep.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := valkeyv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add api scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CertificateGVK)
	s.AddKnownTypeWithName(CertificateGVK, u)
	return s
}

// testNS is the single namespace used across pkg/tls tests.
const testNS = "ns"

func tlsSecret(name string, ca, crt, key []byte) *corev1.Secret {
	data := map[string][]byte{}
	if ca != nil {
		data[naming.TLSSecretKeyCA] = ca
	}
	if crt != nil {
		data[naming.TLSSecretKeyCert] = crt
	}
	if key != nil {
		data[naming.TLSSecretKeyKey] = key
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Type:       corev1.SecretTypeTLS,
		Data:       data,
	}
}

func certManagerCluster(name string, shards, replicas int32) *valkeyv1alpha1.PerconaValkeyCluster {
	return &valkeyv1alpha1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: valkeyv1alpha1.PerconaValkeyClusterSpec{
			Shards:   shards,
			Replicas: replicas,
			TLS: &valkeyv1alpha1.TLSConfig{
				CertManager: &valkeyv1alpha1.CertManagerSpec{
					IssuerRef: valkeyv1alpha1.IssuerRef{Name: "ca-issuer"},
				},
			},
		},
	}
}

// ----------------------------------------------------------------------------
// ComputeTLSHash / ValidateSecretData
// ----------------------------------------------------------------------------

func TestComputeTLSHashStableAndDistinct(t *testing.T) {
	t.Parallel()
	s1 := tlsSecret("internal-c-tls", []byte("CA"), []byte("CRT"), []byte("KEY"))
	s1copy := tlsSecret("internal-c-tls", []byte("CA"), []byte("CRT"), []byte("KEY"))

	h1, err := ComputeTLSHash(s1)
	if err != nil {
		t.Fatalf("hash s1: %v", err)
	}
	h1copy, err := ComputeTLSHash(s1copy)
	if err != nil {
		t.Fatalf("hash s1copy: %v", err)
	}
	if h1 != h1copy {
		t.Fatalf("hash not stable for identical material: %q vs %q", h1, h1copy)
	}
	if h1 == "" {
		t.Fatal("hash is empty for valid material")
	}

	// A real cert change must change the hash.
	s2 := tlsSecret("internal-c-tls", []byte("CA"), []byte("CRT-ROTATED"), []byte("KEY"))
	h2, err := ComputeTLSHash(s2)
	if err != nil {
		t.Fatalf("hash s2: %v", err)
	}
	if h1 == h2 {
		t.Fatal("hash unchanged after cert material change (would miss a roll)")
	}

	// Length-prefixing must prevent boundary-collision: moving a byte between
	// fields changes the hash even when concatenation would be identical.
	a := tlsSecret("x", []byte("AB"), []byte("C"), []byte("D"))
	b := tlsSecret("x", []byte("A"), []byte("BC"), []byte("D"))
	ha, _ := ComputeTLSHash(a)
	hb, _ := ComputeTLSHash(b)
	if ha == hb {
		t.Fatal("hash collided across field boundary (missing length prefix)")
	}
}

func TestComputeTLSHashFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		secret *corev1.Secret
	}{
		{"nil secret", nil},
		{"missing ca", tlsSecret("c", nil, []byte("CRT"), []byte("KEY"))},
		{"missing cert", tlsSecret("c", []byte("CA"), nil, []byte("KEY"))},
		{"missing key", tlsSecret("c", []byte("CA"), []byte("CRT"), nil)},
		{"empty ca", tlsSecret("c", []byte{}, []byte("CRT"), []byte("KEY"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ComputeTLSHash(tc.secret); err == nil {
				t.Fatal("expected fail-closed error, got nil")
			}
		})
	}
}

func TestValidateSecretDataDoesNotLeakMaterial(t *testing.T) {
	t.Parallel()
	err := ValidateSecretData("mytls", map[string][]byte{
		naming.TLSSecretKeyCA: []byte("SUPER-SECRET-CA"),
	})
	if err == nil {
		t.Fatal("expected error for missing keys")
	}
	if got := err.Error(); !contains(got, "mytls") || contains(got, "SUPER-SECRET-CA") {
		t.Fatalf("error must name the Secret but not echo material: %q", got)
	}
}

// ----------------------------------------------------------------------------
// DNSNames (SAN list)
// ----------------------------------------------------------------------------

func TestDNSNamesCoversServiceWildcardAndPerPod(t *testing.T) {
	t.Parallel()
	cl := certManagerCluster("c", 2, 1) // 2 shards, 1 replica => nodes 0,1 per shard
	names := DNSNames(cl)
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}

	mustHave := []string{
		"valkey-c",
		"valkey-c.ns",
		"valkey-c.ns.svc",
		"valkey-c.ns.svc.cluster.local",
		"*.valkey-c.ns.svc",
		"*.valkey-c.ns.svc.cluster.local",
		// per-pod: shard 0 nodes 0,1 and shard 1 nodes 0,1
		"valkey-c-0-0.valkey-c.ns.svc",
		"valkey-c-0-1.valkey-c.ns.svc",
		"valkey-c-1-0.valkey-c.ns.svc",
		"valkey-c-1-1.valkey-c.ns.svc",
		"valkey-c-1-1.valkey-c.ns.svc.cluster.local",
	}
	for _, want := range mustHave {
		if !set[want] {
			t.Errorf("DNSNames missing SAN %q; got %v", want, names)
		}
	}

	// Determinism + dedupe: a second call must be byte-identical with no dups.
	again := DNSNames(cl)
	if len(again) != len(names) {
		t.Fatalf("DNSNames not deterministic length: %d vs %d", len(again), len(names))
	}
	for i := range names {
		if names[i] != again[i] {
			t.Fatalf("DNSNames order not stable at %d: %q vs %q", i, names[i], again[i])
		}
	}
	if len(set) != len(names) {
		t.Fatalf("DNSNames contains duplicates: %v", names)
	}
}

// ----------------------------------------------------------------------------
// ValidateSecretRef (secret-ref mode)
// ----------------------------------------------------------------------------

func TestValidateSecretRef(t *testing.T) {
	t.Parallel()
	s := testScheme(t)
	good := tlsSecret("byo-tls", []byte("CA"), []byte("CRT"), []byte("KEY"))
	bad := tlsSecret("bad-tls", []byte("CA"), nil, []byte("KEY"))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(good, bad).Build()

	if err := ValidateSecretRef(context.Background(), c, "ns", "byo-tls"); err != nil {
		t.Fatalf("valid secret rejected: %v", err)
	}
	if err := ValidateSecretRef(context.Background(), c, "ns", "bad-tls"); err == nil {
		t.Fatal("malformed secret accepted (should fail closed)")
	}
	if err := ValidateSecretRef(context.Background(), c, "ns", "absent"); err == nil {
		t.Fatal("missing secret accepted (should fail closed)")
	}
}

// ----------------------------------------------------------------------------
// EnsureCertificate (cert-manager mode, unstructured)
// ----------------------------------------------------------------------------

func TestEnsureCertificateCreatesUnstructured(t *testing.T) {
	t.Parallel()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cl := certManagerCluster("c", 1, 1)
	cl.Spec.TLS.CertManager.IssuerRef.Kind = valkeyv1alpha1.IssuerKindClusterIssuer

	created, err := EnsureCertificate(context.Background(), c, cl, nil)
	if err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on first provision")
	}

	got := NewCertificateObject()
	if err := c.Get(context.Background(), types.NamespacedName{Name: naming.TLSSecretName("c"), Namespace: "ns"}, got); err != nil {
		t.Fatalf("get certificate: %v", err)
	}
	sn, _, _ := unstructured.NestedString(got.Object, "spec", "secretName")
	if sn != naming.TLSSecretName("c") {
		t.Errorf("secretName = %q, want %q", sn, naming.TLSSecretName("c"))
	}
	name, _, _ := unstructured.NestedString(got.Object, "spec", "issuerRef", "name")
	if name != "ca-issuer" {
		t.Errorf("issuerRef.name = %q, want ca-issuer", name)
	}
	kind, _, _ := unstructured.NestedString(got.Object, "spec", "issuerRef", "kind")
	if kind != "ClusterIssuer" {
		t.Errorf("issuerRef.kind = %q, want ClusterIssuer", kind)
	}
	group, _, _ := unstructured.NestedString(got.Object, "spec", "issuerRef", "group")
	if group != "cert-manager.io" {
		t.Errorf("issuerRef.group = %q, want cert-manager.io", group)
	}
	dns, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "dnsNames")
	if len(dns) == 0 {
		t.Fatal("certificate has no dnsNames")
	}

	// Idempotent: a second call must not report created and must not error.
	created2, err := EnsureCertificate(context.Background(), c, cl, nil)
	if err != nil {
		t.Fatalf("second EnsureCertificate: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on second reconcile (idempotent)")
	}
}

func TestEnsureCertificateDefaultsIssuerKind(t *testing.T) {
	t.Parallel()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cl := certManagerCluster("d", 1, 0) // kind left empty -> defaults to Issuer

	if _, err := EnsureCertificate(context.Background(), c, cl, nil); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	got := NewCertificateObject()
	if err := c.Get(context.Background(), types.NamespacedName{Name: naming.TLSSecretName("d"), Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	kind, _, _ := unstructured.NestedString(got.Object, "spec", "issuerRef", "kind")
	if kind != "Issuer" {
		t.Errorf("default issuerRef.kind = %q, want Issuer", kind)
	}
}

func TestEnsureCertificateRejectsWrongMode(t *testing.T) {
	t.Parallel()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	// secret-ref mode (no certManager) must not be sent to EnsureCertificate.
	cl := &valkeyv1alpha1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"},
		Spec:       valkeyv1alpha1.PerconaValkeyClusterSpec{TLS: &valkeyv1alpha1.TLSConfig{SecretName: "byo"}},
	}
	if _, err := EnsureCertificate(context.Background(), c, cl, nil); err == nil {
		t.Fatal("expected error when called without cert-manager mode")
	}

	// empty issuer name is rejected.
	cl2 := certManagerCluster("y", 1, 1)
	cl2.Spec.TLS.CertManager.IssuerRef.Name = ""
	if _, err := EnsureCertificate(context.Background(), c, cl2, nil); err == nil {
		t.Fatal("expected error for empty issuerRef.name")
	}
}

func TestEnsureCertificateSetsOwner(t *testing.T) {
	t.Parallel()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cl := certManagerCluster("o", 1, 1)

	called := false
	setOwner := func(obj metav1.Object) error {
		called = true
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: valkeyv1alpha1.GroupVersion.String(),
			Kind:       "PerconaValkeyCluster",
			Name:       cl.Name,
			UID:        "uid-1",
		}})
		return nil
	}
	if _, err := EnsureCertificate(context.Background(), c, cl, setOwner); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	if !called {
		t.Fatal("setOwner was not invoked")
	}
	got := NewCertificateObject()
	if err := c.Get(context.Background(), types.NamespacedName{Name: naming.TLSSecretName("o"), Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.GetOwnerReferences()) != 1 {
		t.Fatalf("owner reference not persisted: %v", got.GetOwnerReferences())
	}
}

// ----------------------------------------------------------------------------
// WaitForWebhookCert (M5 scaffold for M6)
// ----------------------------------------------------------------------------

func TestWaitForWebhookCertReady(t *testing.T) {
	t.Parallel()
	s := testScheme(t)
	secret := tlsSecret("webhook-tls", []byte("CA"), []byte("CRT"), []byte("KEY"))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	err := WaitForWebhookCert(context.Background(), c, types.NamespacedName{Name: "webhook-tls", Namespace: "ns"}, time.Second)
	if err != nil {
		t.Fatalf("WaitForWebhookCert with ready secret: %v", err)
	}
}

func TestWaitForWebhookCertTimesOutWhenAbsent(t *testing.T) {
	t.Parallel()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	err := WaitForWebhookCert(context.Background(), c, types.NamespacedName{Name: "absent", Namespace: "ns"}, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error when webhook secret is absent")
	}
}

func TestReasonConstantStable(t *testing.T) {
	t.Parallel()
	if ReasonWebhookCertNotReady != "WebhookCertNotReady" {
		t.Fatalf("ReasonWebhookCertNotReady drifted: %q", ReasonWebhookCertNotReady)
	}
}

func TestIsNoCertManagerCRD(t *testing.T) {
	t.Parallel()
	// NotFound (e.g. the Certificate object/CRD absent) is treated as
	// cert-manager-not-installed.
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "cert-manager.io", Resource: "certificates"}, "x")
	if !IsNoCertManagerCRD(notFound) {
		t.Error("NotFound should be detected as no cert-manager CRD")
	}
	// NoKindMatchError: the GVK is unknown to the RESTMapper (CRD not installed).
	noMatch := &apimeta.NoKindMatchError{GroupKind: CertificateGVK.GroupKind()}
	if !IsNoCertManagerCRD(noMatch) {
		t.Error("NoKindMatchError should be detected as no cert-manager CRD")
	}
	// An unrelated error must NOT be classified as a missing CRD.
	if IsNoCertManagerCRD(errSentinel) {
		t.Error("unrelated error misclassified as no cert-manager CRD")
	}
}

// errSentinel is a plain non-Kubernetes error for the negative case.
var errSentinel = fmt.Errorf("some transient API error")

// contains is a tiny substring helper to avoid importing strings just for tests.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
