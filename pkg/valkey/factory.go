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
	"crypto/tls"
	"crypto/x509"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// TLS Secret data keys (the cert-manager / secret-ref convention, 03 §2.8).
const (
	tlsSecretKeyCA   = "ca.crt"
	tlsSecretKeyCert = "tls.crt"
	tlsSecretKeyKey  = "tls.key"
)

// factory is the real ClientFactory. It resolves per-node connection material
// (address, auth, TLS) from cluster-scoped Secrets via a controller client.
type factory struct {
	c client.Client
}

// NewClientFactory builds the production ClientFactory backed by the supplied
// controller client. The reconciler holds this; envtest injects a mock instead.
func NewClientFactory(c client.Client) ClientFactory {
	return &factory{c: c}
}

// NewClusterClientFactory builds the production ClusterClientFactory backed by
// the supplied controller client. The PerconaValkeyCluster controller holds
// this; envtest injects a scripted mock instead (CR-18).
func NewClusterClientFactory(c client.Client) ClusterClientFactory {
	return &factory{c: c}
}

// ForNode resolves the _operator credential + TLS for the node's cluster, builds
// the <status.podIP>:6379 dial address and returns a live ClusterClient (the
// full CLUSTER orchestration surface). It reuses the same auth/TLS resolution as
// the narrow ClientFactory.For path; NewClient already returns a ClusterClient,
// so this simply widens the returned type (05 §10).
func (f *factory) ForNode(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) (string, ClusterClient, error) {
	if node.Status.PodIP == "" {
		return "", nil, fmt.Errorf("node %s has no podIP yet", node.Name)
	}
	cluster := naming.NodeCluster(node.Name, node.Labels)
	auth := f.resolveAuth(ctx, cluster, node.Namespace)
	tlsConfig, err := f.resolveTLS(ctx, node, cluster)
	if err != nil {
		return "", nil, err
	}
	addr := Address(node.Status.PodIP)
	c, err := NewClient(addr, auth, tlsConfig)
	if err != nil {
		return "", nil, err
	}
	return addr, c, nil
}

// For resolves the _operator password from the cluster system-passwords Secret,
// builds the <status.podIP>:6379 address and the TLS config from spec.tls, then
// dials via NewClient. The cluster name is read from the node's
// valkey.percona.com/cluster label (OQ-2.1 interim convention). 05 §10.
func (f *factory) For(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) (ConfigClient, error) {
	if node.Status.PodIP == "" {
		return nil, fmt.Errorf("node %s has no podIP yet", node.Name)
	}
	cluster := naming.NodeCluster(node.Name, node.Labels)

	auth := f.resolveAuth(ctx, cluster, node.Namespace)

	tlsConfig, err := f.resolveTLS(ctx, node, cluster)
	if err != nil {
		return nil, err
	}

	return NewClient(Address(node.Status.PodIP), auth, tlsConfig)
}

// resolveAuth reads the _operator password from internal-<cluster>-system-passwords.
// A missing Secret/key yields empty credentials (unauthenticated connect) rather
// than an error — a freshly bootstrapped node may not have it yet, and NewClient
// falls back on WRONGPASS regardless (05 §10).
func (f *factory) resolveAuth(ctx context.Context, cluster, namespace string) Auth {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: naming.SystemPasswordsSecretName(cluster), Namespace: namespace}
	if err := f.c.Get(ctx, key, secret); err != nil {
		return Auth{}
	}
	pw, ok := secret.Data[naming.SystemUserOperator]
	if !ok || len(pw) == 0 {
		return Auth{}
	}
	return Auth{Username: naming.SystemUserOperator, Password: string(pw)}
}

// resolveTLS builds a *tls.Config from the node's referenced TLS Secret when
// spec.tls is set, with the ServerName pinned to the headless Service DNS so the
// per-pod cert SAN validates (05 §10). Returns (nil, nil) when TLS is disabled.
func (f *factory) resolveTLS(ctx context.Context, node *valkeyv1alpha1.ValkeyNode, cluster string) (*tls.Config, error) {
	if node.Spec.TLS == nil || node.Spec.TLS.SecretName == "" {
		return nil, nil
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: node.Spec.TLS.SecretName, Namespace: node.Namespace}
	if err := f.c.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("tls secret %s not found", node.Spec.TLS.SecretName)
		}
		return nil, fmt.Errorf("get tls secret %s: %w", node.Spec.TLS.SecretName, err)
	}

	caPEM, ok := secret.Data[tlsSecretKeyCA]
	if !ok {
		return nil, fmt.Errorf("tls secret %s missing %s", node.Spec.TLS.SecretName, tlsSecretKeyCA)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("tls secret %s: invalid %s", node.Spec.TLS.SecretName, tlsSecretKeyCA)
	}

	cfg := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
		ServerName: fmt.Sprintf("%s.%s.svc", naming.HeadlessServiceName(cluster), node.Namespace),
	}
	// Present a client cert for mTLS when the Secret carries one.
	if certPEM, okC := secret.Data[tlsSecretKeyCert]; okC {
		if keyPEM, okK := secret.Data[tlsSecretKeyKey]; okK {
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return nil, fmt.Errorf("tls secret %s: invalid keypair: %w", node.Spec.TLS.SecretName, err)
			}
			cfg.Certificates = []tls.Certificate{pair}
		}
	}
	return cfg, nil
}
