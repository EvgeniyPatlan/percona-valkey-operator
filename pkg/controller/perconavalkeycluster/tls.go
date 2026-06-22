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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	pkgtls "valkey.percona.com/percona-valkey-operator/pkg/tls"
)

// TLS condition reasons (07 §3.3, §3.4). Owned by the TLS leg (M5 GO-5.6/5.8); a
// malformed/missing cert Secret fails the reconcile closed with Degraded/TLSError.
const (
	// ReasonTLSError marks a malformed or missing TLS cert Secret (secret-ref mode
	// missing one of ca.crt/tls.crt/tls.key, or a renewed Secret that fails to
	// parse). The reconcile fails CLOSED — never a silent plaintext fallback.
	ReasonTLSError = "TLSError"
)

// TLS event reasons (07 §3.3, §3.4). Owned by the TLS leg.
const (
	// EventTLSCertificateProvisioned is emitted when the operator creates/updates a
	// cert-manager Certificate for cert-manager mode.
	EventTLSCertificateProvisioned = "TLSCertificateProvisioned"
	// EventTLSHashUpdated is emitted when a real cert change bumps the tlsHash and
	// the operator stamps it onto the ValkeyNodes to drive the roll.
	EventTLSHashUpdated = "TLSHashUpdated"
)

// reconcileTLS provisions or validates the cluster's TLS material and propagates
// the resulting tlsHash so a real cert change rolls the pods.
//
// SEAM (M5 GO-5.6/5.7/5.8 — TLS leg fills this): when spec.tls is nil this is a
// no-op (TLS off). When spec.tls.certManager.issuerRef is set the leg provisions a
// cert-manager.io/v1 Certificate (via UNSTRUCTURED — no typed cert-manager Go dep)
// with DNS SANs for the headless Service + per-pod <cluster>-<sh>-<n> names, into
// naming.TLSSecretName(cluster). When spec.tls.secretName is set the leg validates
// the referenced Secret carries ca.crt/tls.crt/tls.key and fails CLOSED
// (Degraded/ReasonTLSError) if any is missing. In both modes it computes the
// tlsHash (sha256 of the cert material identity), stamps naming.AnnTLSHash onto
// each ValkeyNode (see stampTLSHash) — the resources builder propagates that onto
// the pod template — and reuses the existing one-at-a-time roll (replicas before
// primary, proactive failover) so the cert change rolls cleanly (07 §3.3, §3.4).
// It is dispatched in reconcileInfra BEFORE the ConfigMap+nodes so the cert Secret
// exists before any pod mounts it.
func (r *Reconciler) reconcileTLS(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	if cluster.Spec.TLS == nil {
		return nil
	}

	// cert-manager mode: provision the Certificate (unstructured, no typed dep)
	// BEFORE reading its Secret. The Certificate owns naming.TLSSecretName(cluster);
	// cert-manager then writes the three keys into it asynchronously.
	if cluster.Spec.TLS.CertManager != nil {
		created, err := pkgtls.EnsureCertificate(ctx, r.Client, cluster, func(obj metav1.Object) error {
			return k8s.SetControllerOwnerRef(cluster, obj, r.scheme)
		})
		if err != nil {
			if pkgtls.IsNoCertManagerCRD(err) {
				return fmt.Errorf("cert-manager mode requires cert-manager installed (Certificate CRD): %w", err)
			}
			return fmt.Errorf("ensure cert-manager Certificate: %w", err)
		}
		if created {
			r.recorder.Eventf(cluster, nil, eventNormal, EventTLSCertificateProvisioned,
				"ProvisionTLSCertificate", "Provisioned cert-manager Certificate %s", naming.TLSSecretName(cluster.Name))
		}
	}

	// Compute the roll-triggering hash from the live cert Secret. In cert-manager
	// mode the Secret may not exist yet (cert-manager not finished) — that is a
	// transient, expected state, NOT a fail-closed error, so the hash stays empty
	// and the roll machinery stays quiescent until the cert lands. In secret-ref
	// mode a missing/malformed Secret IS fatal (fail closed -> Degraded/TLSError).
	hash, err := r.computeTLSHash(ctx, cluster)
	if err != nil {
		return err
	}
	if hash != "" {
		if cluster.Annotations == nil {
			cluster.Annotations = map[string]string{}
		}
		// Emit a one-shot event when the stamped hash actually changes (a real
		// cert change), so rotation is auditable; never on a steady reconcile.
		if cluster.Annotations[naming.AnnTLSHash] != hash {
			r.recorder.Eventf(cluster, nil, eventNormal, EventTLSHashUpdated,
				"UpdateTLSHash", "TLS material changed; stamped tlsHash to roll pods")
		}
		cluster.Annotations[naming.AnnTLSHash] = hash
	}
	return nil
}

// tlsSecretName returns the live cert Secret name for the cluster's TLS mode:
// the user-supplied spec.tls.secretName in secret-ref mode, else the
// operator-provisioned naming.TLSSecretName in cert-manager mode (07 §3.3).
func tlsSecretName(cluster *valkeyv1alpha1.PerconaValkeyCluster) string {
	if cluster.Spec.TLS != nil && cluster.Spec.TLS.SecretName != "" {
		return cluster.Spec.TLS.SecretName
	}
	return naming.TLSSecretName(cluster.Name)
}

// nodeTLSConfig resolves the TLSConfig to stamp onto a ValkeyNode so the node
// always mounts the ACTUAL cert Secret regardless of provisioning mode. The node
// resources builder (buildVolumes) mounts the TLS volume from Spec.TLS.SecretName
// only; in cert-manager mode the cluster's spec.tls.secretName is empty (only
// certManager is set), so passing spec.tls through verbatim would leave the node
// without a /tls mount even though the rendered config carries tls-*-file paths —
// a guaranteed crash-loop. This collapses both modes to a concrete SecretName
// (the user's in secret-ref mode, the operator-provisioned naming.TLSSecretName in
// cert-manager mode) so the mount and the rendered tls-*-file directives always
// agree (07 §3.1, §3.3). Returns nil when TLS is off (no mount, no directives).
//
// The DH-params Secret reference (and the cipher/authClients hardening knobs) are
// propagated verbatim so the node resources builder can mount the DH-params Secret
// at the path the cluster-side config renderer points tls-dh-params-file at; a
// rendered tls-dh-params-file with no matching mount would crash-loop the pod
// (07 §3.2). DHParamsSecret is deep-copied so the node spec never aliases the
// cluster's pointer.
func nodeTLSConfig(cluster *valkeyv1alpha1.PerconaValkeyCluster) *valkeyv1alpha1.TLSConfig {
	if cluster.Spec.TLS == nil {
		return nil
	}
	return &valkeyv1alpha1.TLSConfig{
		SecretName:     tlsSecretName(cluster),
		AuthClients:    cluster.Spec.TLS.AuthClients,
		Ciphers:        cluster.Spec.TLS.Ciphers,
		CipherSuites:   cluster.Spec.TLS.CipherSuites,
		DHParamsSecret: cluster.Spec.TLS.DHParamsSecret.DeepCopy(),
	}
}

// computeTLSHash returns the roll-triggering hash for the cluster's current TLS
// material (sha256 over ca.crt/tls.crt/tls.key identity). The value MUST be stable
// across reconciles for unchanged material and change ONLY on a real cert change
// (07 §3.4), so it never causes a phantom roll.
//
// SEAM (M5 GO-5.6 — TLS leg fills this): read naming.TLSSecretName(cluster) (or
// spec.tls.secretName) and hash the three cert keys deterministically. The actual
// hashing helper lives in pkg/tls (ComputeTLSHash); this controller-side stub just
// reserves the call site. Returns "" (no hash / TLS off) until implemented, which
// keeps AnnTLSHash absent and the roll machinery quiescent.
func (r *Reconciler) computeTLSHash(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) (string, error) {
	if cluster.Spec.TLS == nil {
		return "", nil
	}
	secretRefMode := cluster.Spec.TLS.SecretName != ""
	name := tlsSecretName(cluster)

	secret, err := pkgtls.LoadSecret(ctx, r.Client, cluster.Namespace, name)
	if err != nil {
		if secretRefMode {
			// Bring-your-own Secret is missing/unreadable: fail CLOSED (07 §3.3).
			return "", err
		}
		// cert-manager mode: the Secret has not been issued yet. This is an
		// expected transient state, not an error — keep the hash empty so no
		// phantom roll fires, and the previously-stamped hash (if any) stays put.
		logf.FromContext(ctx).V(1).Info("TLS cert Secret not yet present (cert-manager issuing)", "secret", name)
		return cluster.Annotations[naming.AnnTLSHash], nil
	}

	// Both modes validate the three keys and hash them; a malformed Secret fails
	// CLOSED here (-> Degraded/TLSError) rather than a silent plaintext fallback.
	hash, err := pkgtls.ComputeTLSHash(secret)
	if err != nil {
		return "", err
	}
	return hash, nil
}

// stampTLSHash records the tlsHash onto the ValkeyNode object as the
// naming.AnnTLSHash annotation, so the resources builder (applyTLSHashAnnotation)
// copies it onto the pod template and a real cert change rolls the workload via
// the existing config-hash roll machinery (07 §3.4). An empty hash (TLS off / not
// yet computed) is a no-op so it never causes a phantom roll. It is a package
// function (not a method) because it mutates only the node; it is called from the
// node-build path (buildValkeyNodeSpec) alongside the serverConfigHash stamping.
func stampTLSHash(node *valkeyv1alpha1.ValkeyNode, hash string) {
	if hash == "" {
		return
	}
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[naming.AnnTLSHash] = hash
}
