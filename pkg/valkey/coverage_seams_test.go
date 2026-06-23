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

package valkey

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// --- factory.ForNode (the ClusterClientFactory production path) --------------

// TestForNodeNoPodIP covers the early no-podIP guard on the wide ForNode path
// (distinct from the narrow For path already covered).
func TestForNodeNoPodIP(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	f := NewClusterClientFactory(c) // also covers NewClusterClientFactory.
	if _, _, err := f.ForNode(context.Background(), newTestNode()); err == nil {
		t.Error("expected error when podIP is empty")
	}
}

// TestForNodeTLSResolveError covers the ForNode TLS-resolution error branch: a
// node referencing a missing TLS Secret fails before any dial.
func TestForNodeTLSResolveError(t *testing.T) {
	node := newTestNode()
	node.Status.PodIP = "10.0.0.9"
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "absent-tls"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	f := NewClusterClientFactory(c)
	if _, _, err := f.ForNode(context.Background(), node); err == nil {
		t.Error("expected error when the node's TLS Secret is missing")
	}
}

// --- factory.resolveTLS error branches (CA-key / invalid-PEM / keypair) -------

// TestResolveTLSMissingCAKey covers the missing-ca.crt branch.
func TestResolveTLSMissingCAKey(t *testing.T) {
	node := newTestNode()
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "valkey"},
		Data:       map[string][]byte{tlsSecretKeyCert: []byte("x"), tlsSecretKeyKey: []byte("y")},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	f := factoryInternal(NewClientFactory(c))
	if _, err := f.resolveTLS(context.Background(), node, "mycluster"); err == nil {
		t.Error("expected error when ca.crt key is absent")
	}
}

// TestResolveTLSInvalidCA covers the invalid-PEM (AppendCertsFromPEM fails) branch.
func TestResolveTLSInvalidCA(t *testing.T) {
	node := newTestNode()
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "valkey"},
		Data:       map[string][]byte{tlsSecretKeyCA: []byte("not-a-pem")},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	f := factoryInternal(NewClientFactory(c))
	if _, err := f.resolveTLS(context.Background(), node, "mycluster"); err == nil {
		t.Error("expected error for an invalid CA PEM")
	}
}

// TestResolveTLSInvalidKeypair covers the bad-client-keypair branch (valid CA,
// but tls.crt/tls.key are not a parseable pair).
func TestResolveTLSInvalidKeypair(t *testing.T) {
	node := newTestNode()
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}
	caPEM, _, _ := generateTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "valkey"},
		Data: map[string][]byte{
			tlsSecretKeyCA:   caPEM,
			tlsSecretKeyCert: []byte("garbage-cert"),
			tlsSecretKeyKey:  []byte("garbage-key"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	f := factoryInternal(NewClientFactory(c))
	if _, err := f.resolveTLS(context.Background(), node, "mycluster"); err == nil {
		t.Error("expected error for an invalid client keypair")
	}
}

// TestResolveTLSCAOnlyNoClientCert covers the CA-only path (server auth, no mTLS
// client cert presented): a valid CA with no tls.crt/tls.key still yields a config.
func TestResolveTLSCAOnlyNoClientCert(t *testing.T) {
	node := newTestNode()
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}
	caPEM, _, _ := generateTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "valkey"},
		Data:       map[string][]byte{tlsSecretKeyCA: caPEM},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	f := factoryInternal(NewClientFactory(c))
	cfg, err := f.resolveTLS(context.Background(), node, "mycluster")
	if err != nil {
		t.Fatalf("resolveTLS: %v", err)
	}
	if cfg == nil || len(cfg.Certificates) != 0 {
		t.Fatalf("CA-only config should carry no client certs, got %+v", cfg)
	}
}

// TestResolveAuthEmptyOperatorKey covers the present-Secret-but-empty-key branch
// of resolveAuth (yields empty auth so NewClient connects unauthenticated).
func TestResolveAuthEmptyOperatorKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: naming.SystemPasswordsSecretName("mycluster"), Namespace: "valkey"},
		Data:       map[string][]byte{naming.SystemUserOperator: {}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	f := factoryInternal(NewClientFactory(c))
	if auth := f.resolveAuth(context.Background(), "mycluster", "valkey"); auth.Username != "" {
		t.Errorf("empty operator key must yield empty auth, got %+v", auth)
	}
}

// --- topology seams: AnyMigrationInProgress / GossipVisible / PrimaryByShardID --

func TestAnyMigrationInProgress(t *testing.T) {
	t.Parallel()
	if AnyMigrationInProgress(nil) {
		t.Error("nil migrations must report no migration in progress")
	}
	if AnyMigrationInProgress([]SlotMigration{{State: "success"}, {State: "failed"}}) {
		t.Error("all-terminal migrations must report none in progress")
	}
	if !AnyMigrationInProgress([]SlotMigration{{State: "success"}, {State: "running"}}) {
		t.Error("a non-terminal migration must report in progress")
	}
}

func TestGossipVisible(t *testing.T) {
	t.Parallel()
	state := healthyState(t)
	// p0 and r0 are in each other's CLUSTER NODES dump, so the peer table is shared.
	if !state.GossipVisible("p0", "p1") {
		t.Error("p1 should be gossip-visible from p0 (shared peer table)")
	}
	if state.GossipVisible("p0", "nonexistent") {
		t.Error("an unknown destination must not be gossip-visible")
	}
	if state.GossipVisible("nonexistent-src", "p1") {
		t.Error("an unknown source must report not visible")
	}
}

func TestPrimaryByShardID(t *testing.T) {
	t.Parallel()
	state := healthyState(t)
	// healthyState stamps ShardID = "shard-<id>"; the primary of p0's shard is p0.
	p := state.PrimaryByShardID("shard-p0")
	if p == nil || p.ID != "p0" {
		t.Fatalf("PrimaryByShardID(shard-p0) = %v, want p0", p)
	}
	if state.PrimaryByShardID("no-such-shard") != nil {
		t.Error("an unknown shard ID must return nil")
	}
}

// --- scrape.go error paths (each scrape command failing aborts that node) -----

// TestScrapeNodeCommandErrors drives a failure at each scrape step so every
// early-return error branch in ScrapeNode is covered.
func TestScrapeNodeCommandErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	ctx := context.Background()

	type step struct {
		name  string
		setup func(m *MockClusterClient)
	}
	steps := []step{
		{"myid", func(m *MockClusterClient) {
			m.EXPECT().ClusterMyID(ctx).Return("", boom)
		}},
		{"myshardid", func(m *MockClusterClient) {
			m.EXPECT().ClusterMyID(ctx).Return("id", nil)
			m.EXPECT().ClusterMyShardID(ctx).Return("", boom)
		}},
		{"inforepl", func(m *MockClusterClient) {
			m.EXPECT().ClusterMyID(ctx).Return("id", nil)
			m.EXPECT().ClusterMyShardID(ctx).Return("s", nil)
			m.EXPECT().Info(ctx, "replication").Return("", boom)
		}},
		{"clusterinfo", func(m *MockClusterClient) {
			m.EXPECT().ClusterMyID(ctx).Return("id", nil)
			m.EXPECT().ClusterMyShardID(ctx).Return("s", nil)
			m.EXPECT().Info(ctx, "replication").Return("role:master", nil)
			m.EXPECT().ClusterInfo(ctx).Return("", boom)
		}},
		{"clusternodes", func(m *MockClusterClient) {
			m.EXPECT().ClusterMyID(ctx).Return("id", nil)
			m.EXPECT().ClusterMyShardID(ctx).Return("s", nil)
			m.EXPECT().Info(ctx, "replication").Return("role:master", nil)
			m.EXPECT().ClusterInfo(ctx).Return("cluster_state:ok", nil)
			m.EXPECT().ClusterNodes(ctx).Return("", boom)
		}},
	}

	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			m := NewMockClusterClient(ctrl)
			s.setup(m)
			if _, err := ScrapeNode(ctx, "10.0.0.1:6379", m); err == nil {
				t.Fatalf("step %q: expected scrape error", s.name)
			}
		})
	}
}
