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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	pkgtls "valkey.percona.com/percona-valkey-operator/pkg/tls"
)

// tlsTestScheme registers API + core types plus the unstructured cert-manager
// Certificate GVK so the fake client can store the operator-provisioned
// Certificate without a typed cert-manager dependency.
func tlsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := valkeyv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("api scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(pkgtls.CertificateGVK)
	s.AddKnownTypeWithName(pkgtls.CertificateGVK, u)
	return s
}

func newTLSReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	s := tlsTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Reconciler{
		Client:             c,
		scheme:             s,
		recorder:           events.NewFakeRecorder(200),
		skipNameValidation: true,
	}, c
}

// tlsTestNS is the single namespace used across the TLS controller unit tests.
const tlsTestNS = "ns"

func tlsCluster(name string) *valkeyv1alpha1.PerconaValkeyCluster {
	return &valkeyv1alpha1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tlsTestNS},
		Spec: valkeyv1alpha1.PerconaValkeyClusterSpec{
			Mode:     valkeyv1alpha1.ModeCluster,
			Shards:   1,
			Replicas: 1,
		},
	}
}

func makeTLSSecret(name string, ca, crt, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tlsTestNS},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			naming.TLSSecretKeyCA:   ca,
			naming.TLSSecretKeyCert: crt,
			naming.TLSSecretKeyKey:  key,
		},
	}
}

// TestReconcileTLSOffNoop: spec.tls nil => no-op, no annotation, no error.
func TestReconcileTLSOffNoop(t *testing.T) {
	t.Parallel()
	r, _ := newTLSReconciler(t)
	cl := tlsCluster("c") // TLS nil

	if err := r.reconcileTLS(context.Background(), cl); err != nil {
		t.Fatalf("reconcileTLS (TLS off): %v", err)
	}
	if _, ok := cl.Annotations[naming.AnnTLSHash]; ok {
		t.Fatal("TLS off must not stamp a tlsHash annotation")
	}
}

// TestReconcileTLSSecretRefMissingFailsClosed: secret-ref mode, Secret absent =>
// non-nil error (the dispatcher routes this to Degraded/TLSError), no stamp.
func TestReconcileTLSSecretRefMissingFailsClosed(t *testing.T) {
	t.Parallel()
	r, _ := newTLSReconciler(t)
	cl := tlsCluster("c")
	cl.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "byo-tls"}

	err := r.reconcileTLS(context.Background(), cl)
	if err == nil {
		t.Fatal("missing secret-ref Secret must fail closed")
	}
	if _, ok := cl.Annotations[naming.AnnTLSHash]; ok {
		t.Fatal("failed TLS must not stamp a tlsHash")
	}
}

// TestReconcileTLSSecretRefMalformedFailsClosed: secret-ref mode, Secret present
// but missing tls.key => fail closed.
func TestReconcileTLSSecretRefMalformedFailsClosed(t *testing.T) {
	t.Parallel()
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "byo-tls", Namespace: "ns"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			naming.TLSSecretKeyCA:   []byte("CA"),
			naming.TLSSecretKeyCert: []byte("CRT"),
			// tls.key intentionally absent
		},
	}
	r, _ := newTLSReconciler(t, bad)
	cl := tlsCluster("c")
	cl.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "byo-tls"}

	if err := r.reconcileTLS(context.Background(), cl); err == nil {
		t.Fatal("malformed secret-ref Secret must fail closed")
	}
}

// TestReconcileTLSSecretRefValidStamps: secret-ref mode, valid Secret => stamps a
// non-empty tlsHash; stable across reconciles; changes when cert changes.
func TestReconcileTLSSecretRefValidStampsAndRotates(t *testing.T) {
	t.Parallel()
	secret := makeTLSSecret("byo-tls", []byte("CA"), []byte("CRT"), []byte("KEY"))
	r, c := newTLSReconciler(t, secret)
	cl := tlsCluster("c")
	cl.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "byo-tls"}

	if err := r.reconcileTLS(context.Background(), cl); err != nil {
		t.Fatalf("reconcileTLS: %v", err)
	}
	h1 := cl.Annotations[naming.AnnTLSHash]
	if h1 == "" {
		t.Fatal("valid secret-ref must stamp a tlsHash")
	}

	// Stable: a second reconcile with no change must keep the same hash.
	if err := r.reconcileTLS(context.Background(), cl); err != nil {
		t.Fatalf("reconcileTLS (2): %v", err)
	}
	if cl.Annotations[naming.AnnTLSHash] != h1 {
		t.Fatalf("tlsHash churned with no cert change: %q -> %q", h1, cl.Annotations[naming.AnnTLSHash])
	}

	// Rotate the cert material -> the hash must change (drives the roll).
	rotated := makeTLSSecret("byo-tls", []byte("CA"), []byte("CRT-NEW"), []byte("KEY"))
	if err := c.Update(context.Background(), rotated); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}
	if err := r.reconcileTLS(context.Background(), cl); err != nil {
		t.Fatalf("reconcileTLS (rotated): %v", err)
	}
	if cl.Annotations[naming.AnnTLSHash] == h1 {
		t.Fatal("tlsHash did not change after a real cert change")
	}
}

// TestReconcileTLSCertManagerProvisions: cert-manager mode creates the
// unstructured Certificate with correct SANs/issuerRef/secretName; before
// cert-manager issues the Secret the hash stays empty (no phantom roll).
func TestReconcileTLSCertManagerProvisions(t *testing.T) {
	t.Parallel()
	r, c := newTLSReconciler(t)
	cl := tlsCluster("cm")
	cl.Spec.TLS = &valkeyv1alpha1.TLSConfig{
		CertManager: &valkeyv1alpha1.CertManagerSpec{
			IssuerRef: valkeyv1alpha1.IssuerRef{Name: "ca-issuer", Kind: valkeyv1alpha1.IssuerKindClusterIssuer},
		},
	}

	if err := r.reconcileTLS(context.Background(), cl); err != nil {
		t.Fatalf("reconcileTLS (cert-manager): %v", err)
	}

	// The Certificate exists with the expected shape.
	cert := pkgtls.NewCertificateObject()
	if err := c.Get(context.Background(), types.NamespacedName{Name: naming.TLSSecretName("cm"), Namespace: "ns"}, cert); err != nil {
		t.Fatalf("certificate not created: %v", err)
	}
	sn, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	if sn != naming.TLSSecretName("cm") {
		t.Errorf("certificate secretName = %q, want %q", sn, naming.TLSSecretName("cm"))
	}
	name, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	if name != "ca-issuer" {
		t.Errorf("issuerRef.name = %q, want ca-issuer", name)
	}
	dns, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	foundHeadless, foundPod := false, false
	for _, d := range dns {
		if d == "valkey-cm.ns.svc" {
			foundHeadless = true
		}
		if d == "valkey-cm-0-0.valkey-cm.ns.svc" {
			foundPod = true
		}
	}
	if !foundHeadless || !foundPod {
		t.Errorf("SAN list missing headless/per-pod entries: %v", dns)
	}
	// owner-ref set on the Certificate (GC + re-enqueue).
	if len(cert.GetOwnerReferences()) != 1 {
		t.Errorf("certificate owner-ref not set: %v", cert.GetOwnerReferences())
	}

	// Secret not yet issued by cert-manager => hash stays empty, no phantom roll.
	if _, ok := cl.Annotations[naming.AnnTLSHash]; ok {
		t.Fatal("cert-manager mode stamped a hash before the Secret exists (phantom roll)")
	}
}

// TestReconcileTLSCertManagerHashesAfterIssue: once cert-manager has written the
// TLS Secret, the operator stamps a stable tlsHash; an absent Secret in
// cert-manager mode is NOT a fail-closed error (transient issuing state).
func TestReconcileTLSCertManagerHashesAfterIssue(t *testing.T) {
	t.Parallel()
	r, c := newTLSReconciler(t)
	cl := tlsCluster("cm")
	cl.Spec.TLS = &valkeyv1alpha1.TLSConfig{
		CertManager: &valkeyv1alpha1.CertManagerSpec{IssuerRef: valkeyv1alpha1.IssuerRef{Name: "ca"}},
	}

	// First pass: Certificate created, no Secret yet -> no error, no hash.
	if err := r.reconcileTLS(context.Background(), cl); err != nil {
		t.Fatalf("reconcileTLS pre-issue: %v", err)
	}
	if _, ok := cl.Annotations[naming.AnnTLSHash]; ok {
		t.Fatal("hash stamped before cert-manager issued the Secret")
	}

	// cert-manager writes the Secret named naming.TLSSecretName(cluster).
	issued := makeTLSSecret(naming.TLSSecretName("cm"), []byte("CA"), []byte("CRT"), []byte("KEY"))
	if err := c.Create(context.Background(), issued); err != nil {
		t.Fatalf("create issued secret: %v", err)
	}

	// Second pass: hash now stamped.
	if err := r.reconcileTLS(context.Background(), cl); err != nil {
		t.Fatalf("reconcileTLS post-issue: %v", err)
	}
	h := cl.Annotations[naming.AnnTLSHash]
	if h == "" {
		t.Fatal("hash not stamped after cert-manager issued the Secret")
	}

	// Third pass: stable.
	if err := r.reconcileTLS(context.Background(), cl); err != nil {
		t.Fatalf("reconcileTLS steady: %v", err)
	}
	if cl.Annotations[naming.AnnTLSHash] != h {
		t.Fatalf("hash churned in steady state: %q -> %q", h, cl.Annotations[naming.AnnTLSHash])
	}
}

// TestComputeTLSHashCertManagerAbsentSecretNotFatal: computeTLSHash returns no
// error (and echoes any prior hash) when the cert-manager Secret is absent.
func TestComputeTLSHashCertManagerAbsentSecretNotFatal(t *testing.T) {
	t.Parallel()
	r, _ := newTLSReconciler(t)
	cl := tlsCluster("cm")
	cl.Spec.TLS = &valkeyv1alpha1.TLSConfig{
		CertManager: &valkeyv1alpha1.CertManagerSpec{IssuerRef: valkeyv1alpha1.IssuerRef{Name: "ca"}},
	}
	cl.Annotations = map[string]string{naming.AnnTLSHash: "prior"}

	h, err := r.computeTLSHash(context.Background(), cl)
	if err != nil {
		t.Fatalf("absent cert-manager Secret must not be fatal: %v", err)
	}
	if h != "prior" {
		t.Fatalf("expected prior hash echoed, got %q", h)
	}
}

// TestTLSSecretNameMode picks the user Secret name in secret-ref mode and the
// operator-provisioned name in cert-manager mode.
func TestTLSSecretNameMode(t *testing.T) {
	t.Parallel()
	ref := tlsCluster("c")
	ref.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "my-byo"}
	if got := tlsSecretName(ref); got != "my-byo" {
		t.Errorf("secret-ref name = %q, want my-byo", got)
	}

	cm := tlsCluster("c")
	cm.Spec.TLS = &valkeyv1alpha1.TLSConfig{CertManager: &valkeyv1alpha1.CertManagerSpec{IssuerRef: valkeyv1alpha1.IssuerRef{Name: "ca"}}}
	if got := tlsSecretName(cm); got != naming.TLSSecretName("c") {
		t.Errorf("cert-manager name = %q, want %q", got, naming.TLSSecretName("c"))
	}
}

// TestNodeTLSConfig locks the cross-package mount seam: the ValkeyNode must carry
// a concrete TLS SecretName in BOTH provisioning modes so buildVolumes mounts the
// real cert Secret at /tls and the rendered tls-*-file directives resolve (07
// §3.1, §3.3). In cert-manager mode the cluster's spec.tls.secretName is empty, so
// the node must instead receive the operator-provisioned naming.TLSSecretName —
// passing spec.tls through verbatim would leave the pod without a /tls mount and
// crash-loop. TLS off => nil (no mount, no directives).
func TestNodeTLSConfig(t *testing.T) {
	t.Parallel()

	off := tlsCluster("c")
	off.Spec.TLS = nil
	if got := nodeTLSConfig(off); got != nil {
		t.Errorf("TLS off must yield nil node TLS, got %+v", got)
	}

	ref := tlsCluster("c")
	ref.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "my-byo"}
	if got := nodeTLSConfig(ref); got == nil || got.SecretName != "my-byo" {
		t.Errorf("secret-ref node SecretName = %+v, want my-byo", got)
	}

	cm := tlsCluster("c")
	cm.Spec.TLS = &valkeyv1alpha1.TLSConfig{CertManager: &valkeyv1alpha1.CertManagerSpec{IssuerRef: valkeyv1alpha1.IssuerRef{Name: "ca"}}}
	got := nodeTLSConfig(cm)
	if got == nil || got.SecretName != naming.TLSSecretName("c") {
		t.Errorf("cert-manager node SecretName = %+v, want %q", got, naming.TLSSecretName("c"))
	}
	// The resolved node config must never leak the certManager pointer (the node
	// never provisions a Certificate; it only mounts the resolved Secret).
	if got != nil && got.CertManager != nil {
		t.Error("node TLS config must not carry certManager (mount-only contract)")
	}

	// The hardening knobs + DH-params Secret ref must propagate so the node
	// resources builder can mount the DH-params Secret (07 §3.2).
	hard := tlsCluster("c")
	hard.Spec.TLS = &valkeyv1alpha1.TLSConfig{
		SecretName:     "my-byo",
		AuthClients:    valkeyv1alpha1.TLSAuthClientsRequire,
		Ciphers:        "HIGH:!aNULL",
		CipherSuites:   "TLS_AES_256_GCM_SHA384",
		DHParamsSecret: &valkeyv1alpha1.SecretRef{Name: "dh", Key: "dh-params.pem"},
	}
	hn := nodeTLSConfig(hard)
	if hn == nil {
		t.Fatal("hardened node TLS config must not be nil")
	}
	if hn.AuthClients != valkeyv1alpha1.TLSAuthClientsRequire || hn.Ciphers != "HIGH:!aNULL" || hn.CipherSuites != "TLS_AES_256_GCM_SHA384" {
		t.Errorf("hardening knobs not propagated: %+v", hn)
	}
	if hn.DHParamsSecret == nil || hn.DHParamsSecret.Name != "dh" || hn.DHParamsSecret.Key != "dh-params.pem" {
		t.Errorf("DHParamsSecret not propagated: %+v", hn.DHParamsSecret)
	}
	// Must be deep-copied, not aliased to the cluster's pointer.
	if hn.DHParamsSecret == hard.Spec.TLS.DHParamsSecret {
		t.Error("DHParamsSecret must be deep-copied, not aliased")
	}
}

// TestStampTLSHash covers the package helper: empty is a no-op; non-empty stamps.
func TestStampTLSHash(t *testing.T) {
	t.Parallel()
	node := &valkeyv1alpha1.ValkeyNode{}
	stampTLSHash(node, "")
	if node.Annotations != nil {
		t.Fatal("empty hash must not allocate annotations (no phantom roll)")
	}
	stampTLSHash(node, "deadbeef")
	if node.Annotations[naming.AnnTLSHash] != "deadbeef" {
		t.Fatalf("stampTLSHash did not set annotation: %v", node.Annotations)
	}
}

// TestReasonAndEventConstants locks the reason/event vocabulary the dispatcher
// and tests rely on (07 §3.3/§3.4).
func TestReasonAndEventConstants(t *testing.T) {
	t.Parallel()
	if ReasonTLSError != "TLSError" {
		t.Errorf("ReasonTLSError drifted: %q", ReasonTLSError)
	}
	if !strings.HasPrefix(EventTLSCertificateProvisioned, "TLS") {
		t.Errorf("EventTLSCertificateProvisioned unexpected: %q", EventTLSCertificateProvisioned)
	}
	if EventTLSHashUpdated != "TLSHashUpdated" {
		t.Errorf("EventTLSHashUpdated drifted: %q", EventTLSHashUpdated)
	}
}
