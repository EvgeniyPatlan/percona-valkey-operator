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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// cert-manager API coordinates. The operator deliberately depends on cert-manager
// via UNSTRUCTURED objects (no typed cert-manager Go dependency — frozen M5
// decision), so a stale/absent cert-manager API version never breaks the build.
const (
	certManagerGroup   = "cert-manager.io"
	certManagerVersion = "v1"
	certificateKind    = "Certificate"
	// IssuerKindIssuer / IssuerKindClusterIssuer are the only valid issuer kinds
	// (CEL-defaulted to Issuer when empty, 03 §2.8).
	defaultIssuerKind = "Issuer"
)

// CertificateGVK is the GroupVersionKind for the cert-manager Certificate the
// operator provisions in cert-manager mode. Exported so the controller and tests
// can build a matching empty object for Get/decode.
var CertificateGVK = schema.GroupVersionKind{
	Group:   certManagerGroup,
	Version: certManagerVersion,
	Kind:    certificateKind,
}

// NewCertificateObject returns an empty unstructured Certificate with the GVK set,
// suitable for a client.Get or as the target of CreateOrUpdate.
func NewCertificateObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CertificateGVK)
	return u
}

// certificateSpec builds the cert-manager Certificate .spec map for the cluster's
// serving cert: secretName = the operator-provisioned TLS Secret, issuerRef from
// spec.tls.certManager, dnsNames = DNSNames(cluster), and usages covering both
// server and client auth (the operator dials nodes as a TLS client over the same
// material, 05 §5). Returned as a plain map[string]interface{} for the
// unstructured object.
func certificateSpec(cluster *valkeyv1alpha1.PerconaValkeyCluster) map[string]interface{} {
	cm := cluster.Spec.TLS.CertManager
	kind := string(cm.IssuerRef.Kind)
	if kind == "" {
		kind = defaultIssuerKind
	}
	dnsNames := DNSNames(cluster)
	// Convert to []interface{} for unstructured.
	dns := make([]interface{}, len(dnsNames))
	for i, n := range dnsNames {
		dns[i] = n
	}
	return map[string]interface{}{
		"secretName": naming.TLSSecretName(cluster.Name),
		"dnsNames":   dns,
		"issuerRef": map[string]interface{}{
			"name":  cm.IssuerRef.Name,
			"kind":  kind,
			"group": certManagerGroup,
		},
		"usages": []interface{}{"server auth", "client auth"},
		"privateKey": map[string]interface{}{
			"algorithm": "ECDSA",
			"size":      int64(256),
		},
	}
}

// EnsureCertificate provisions (creates or updates) the cluster's cert-manager
// Certificate in cert-manager mode (07 §3.3) via an UNSTRUCTURED object — no typed
// cert-manager dependency. The Certificate is named naming.TLSSecretName(cluster)
// (it owns and writes that Secret), carries DNS SANs from DNSNames, and is
// owner-referenced to the cluster so it is GC'd with it. It returns whether the
// object was newly created (so the caller can emit a one-shot Event).
//
// Callers MUST guarantee cert-manager mode (spec.tls.certManager set); EnsureCert
// errors otherwise rather than guessing. setOwner sets the controller owner-ref
// (the controller passes k8s.SetControllerOwnerRef; tests may pass a no-op).
func EnsureCertificate(
	ctx context.Context,
	c client.Client,
	cluster *valkeyv1alpha1.PerconaValkeyCluster,
	setOwner func(obj metav1.Object) error,
) (bool, error) {
	if cluster.Spec.TLS == nil || cluster.Spec.TLS.CertManager == nil {
		return false, fmt.Errorf("EnsureCertificate called without cert-manager mode")
	}
	if cluster.Spec.TLS.CertManager.IssuerRef.Name == "" {
		return false, fmt.Errorf("tls.certManager.issuerRef.name is empty")
	}

	cert := NewCertificateObject()
	cert.SetName(naming.TLSSecretName(cluster.Name))
	cert.SetNamespace(cluster.Namespace)

	res, err := controllerutil.CreateOrUpdate(ctx, c, cert, func() error {
		cert.SetLabels(naming.Labels(cluster.Name, naming.ComponentValkey))
		if err := unstructured.SetNestedMap(cert.Object, certificateSpec(cluster), "spec"); err != nil {
			return fmt.Errorf("set certificate spec: %w", err)
		}
		if setOwner != nil {
			if err := setOwner(cert); err != nil {
				return fmt.Errorf("set owner reference on certificate: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("createOrUpdate Certificate %s/%s: %w", cert.GetNamespace(), cert.GetName(), err)
	}
	return res == controllerutil.OperationResultCreated, nil
}

// IsNoCertManagerCRD reports whether err indicates the cert-manager CRD
// (Certificate) is not installed in the cluster — a NoKindMatchError / NotFound on
// the GVK. The controller maps this to a clear Degraded/TLSError ("cert-manager not
// installed") rather than an opaque API error.
func IsNoCertManagerCRD(err error) bool {
	return apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err)
}
