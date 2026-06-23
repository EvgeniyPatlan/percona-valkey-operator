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
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	opmetrics "valkey.percona.com/percona-valkey-operator/pkg/metrics"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// EventTLSSecretCleanedUp is emitted when the delete-ssl finalizer deletes the
// orphaned operator-issued (cert-manager-written) TLS Secret on teardown — the
// one not reaped by owner-ref GC. Declared here in the finalizer leg (not
// status.go) to keep the leg's event vocabulary self-contained.
const EventTLSSecretCleanedUp = "TLSSecretCleanedUp"

// ensureFinalizers adds the cluster teardown finalizers (delete-pods-in-order +
// delete-ssl) if absent and persists them, leaving them on the in-memory object
// so the pipeline continues in the same pass (04 §6). The delete-ssl finalizer is
// registered now; its TLS-cleanup body is a no-op stub until M5 provisions TLS.
//
// It persists via a metadata-only PATCH (not a full Update): a full Update
// round-trips spec through the typed client, whose `omitempty` drops a zero value
// such as spec.replicas:0 — the API server then re-applies the +kubebuilder
// default=1, silently bumping a legitimately replica-less shard back to one
// replica (05 §1: node count is shards × (1 + replicas), so replicas:0 is valid).
// Patching only the finalizers leaves the stored spec byte-for-byte untouched and
// also avoids whole-object update conflicts (04 §9).
func (r *Reconciler) ensureFinalizers(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	base := cluster.DeepCopy()
	added := controllerutil.AddFinalizer(cluster, naming.FinalizerDeletePodsInOrder)
	if controllerutil.AddFinalizer(cluster, naming.FinalizerDeleteSSL) {
		added = true
	}
	if !added {
		return nil
	}
	// Patch a COPY (not the live object): controller-runtime writes the PATCH
	// response back into whatever object it is handed, which would replace the
	// in-memory cluster's spec with the server's PERSISTED spec. The new security
	// fields (spec.auth, spec.disableCommands) are materialized only by the Go-side
	// CheckNSetDefaults (no CRD defaults) and carry `omitempty`, so the persisted
	// spec drops them back to nil — the charter's omitempty+defaults round-trip
	// footgun. Patching a copy persists the metadata-only finalizer change while
	// leaving the already-defaulted in-memory cluster (which AddFinalizer above also
	// updated with the finalizers) intact for the rest of the pipeline, so the
	// config render + roll hash stay deterministic across reconciles.
	patchTarget := cluster.DeepCopy()
	if err := r.Patch(ctx, patchTarget, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("add cluster finalizers: %w", err)
	}
	// Mirror the server's resourceVersion (advanced by the metadata PATCH) onto the
	// in-memory object so a later status writeback patches the current revision
	// without a stale-conflict, without copying back the omitempty-stripped spec.
	cluster.ResourceVersion = patchTarget.ResourceVersion
	return nil
}

// handleDeletion runs the ordered-teardown finalizer branch (04 §6.1, GO-3.19).
// It is idempotent and re-entrant: each pass re-derives the outstanding work from
// live state and performs the next step, removing the finalizers only once the
// teardown has verifiably completed. The order is replicas-before-primaries,
// shard by shard (highest shard index first), so the last primary releases its
// slots last and gossip/slots wind down cleanly. Owner-ref GC also reaps the
// ValkeyNodes, but explicit ordered deletion avoids quorum thrash mid-teardown.
// Once every ValkeyNode is gone, delete-ssl's no-op stub runs and both finalizers
// are removed; the API server then GCs the owned Service/ConfigMap/Secret/PDB.
func (r *Reconciler) handleDeletion(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(cluster, naming.FinalizerDeletePodsInOrder) &&
		!controllerutil.ContainsFinalizer(cluster, naming.FinalizerDeleteSSL) {
		// Our finalizers are gone; nothing left to do (GC proceeds).
		return ctrl.Result{}, nil
	}

	nodes, err := r.listClusterNodes(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, cluster, ReasonValkeyNodeListError, err)
	}

	// Delete the next ValkeyNode in ordered (replicas-before-primaries, highest
	// shard first) sequence — one per pass so a crash re-runs from the top.
	if next := nextTeardownNode(nodes); next != nil {
		if next.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, next); err != nil && !apierrorsIsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("ordered teardown delete %s: %w", next.Name, err)
			}
			r.recorder.Eventf(cluster, next, eventNormal, EventValkeyNodeDeleted, "Teardown",
				"ordered teardown deleted ValkeyNode %s", next.Name)
		}
		// Still nodes outstanding — requeue and re-derive next pass.
		log.V(1).Info("ordered teardown in progress, nodes remain", "remaining", len(nodes.Items))
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}

	// All ValkeyNodes gone: run the (stubbed) TLS cleanup, then drop finalizers.
	if err := r.cleanupTLS(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("teardown TLS cleanup: %w", err)
	}
	controllerutil.RemoveFinalizer(cluster, naming.FinalizerDeletePodsInOrder)
	controllerutil.RemoveFinalizer(cluster, naming.FinalizerDeleteSSL)
	if err := r.Update(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove cluster finalizers: %w", err)
	}
	// Reap the per-cluster business gauges so a removed cluster leaves no stale
	// valkey_operator_cluster_* series behind (the action counters are monotonic
	// event records and intentionally survive).
	opmetrics.DeleteCluster(cluster.Namespace, cluster.Name)
	log.Info("cluster teardown complete; finalizers removed, GC proceeds")
	return ctrl.Result{}, nil
}

// cleanupTLS is the delete-ssl finalizer body: it deletes operator-issued TLS
// material that owner-ref GC does NOT reap on cluster teardown (04 §6.1).
//
// In cert-manager mode the operator provisions a cert-manager Certificate named
// naming.TLSSecretName(cluster) that IS owner-referenced to the cluster (so it is
// GC'd automatically). cert-manager, however, writes the actual TLS Secret of the
// SAME name with an owner reference to the CERTIFICATE — not to our cluster — so
// when the cluster (and with it the Certificate) is deleted, that Secret is
// orphaned: nothing in the owner-ref graph reaps it, and it lingers holding the
// cluster's private key. This finalizer body deletes it explicitly.
//
// It is scoped to cert-manager mode only: in secret-ref (bring-your-own) mode the
// Secret is user-owned and the operator must NEVER delete it; TLS-off clusters
// have no material at all. The delete is idempotent — an already-absent Secret is
// fine — and it refuses to touch a Secret it does not own (no controller owner-ref
// to this cluster), so a name collision with a user Secret can never cause the
// operator to delete data it did not create.
func (r *Reconciler) cleanupTLS(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	// Only cert-manager mode leaves an orphaned operator-issued Secret. TLS off or
	// secret-ref (user-owned) mode: nothing for the operator to clean up.
	if cluster.Spec.TLS == nil || cluster.Spec.TLS.CertManager == nil {
		return nil
	}

	log := logf.FromContext(ctx)
	name := naming.TLSSecretName(cluster.Name)

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, secret)
	if apierrorsIsNotFound(err) {
		return nil // already gone (cert-manager GC or a prior pass).
	}
	if err != nil {
		return fmt.Errorf("teardown: get TLS Secret %s: %w", name, err)
	}

	// Defensive: only delete a Secret that carries the operator's component label.
	// cert-manager copies the labels we set on the Certificate onto the Secret it
	// issues, so the operator-provisioned Secret is identifiable; a same-named
	// user Secret (which we must not touch) would not carry it.
	if !operatorOwnedTLSSecret(secret, cluster.Name) {
		log.V(1).Info("teardown: TLS Secret not operator-owned, leaving it untouched", "secret", name)
		return nil
	}

	if err := r.Delete(ctx, secret); err != nil && !apierrorsIsNotFound(err) {
		return fmt.Errorf("teardown: delete TLS Secret %s: %w", name, err)
	}
	log.Info("teardown: deleted orphaned operator-issued TLS Secret", "secret", name)
	r.recorder.Eventf(cluster, nil, eventNormal, EventTLSSecretCleanedUp, "TeardownTLS",
		"deleted orphaned operator-issued TLS Secret %s", name)
	return nil
}

// operatorOwnedTLSSecret reports whether the TLS Secret was issued for this
// cluster by the operator's cert-manager Certificate — identified by the operator
// component label cert-manager propagates from the Certificate onto the Secret. A
// Secret lacking that label (e.g. a user's same-named Secret) is left untouched so
// teardown can never delete data the operator did not create.
func operatorOwnedTLSSecret(secret *corev1.Secret, cluster string) bool {
	want := naming.Labels(cluster, naming.ComponentValkey)
	for k, v := range want {
		if secret.Labels[k] != v {
			return false
		}
	}
	return true
}

// nextTeardownNode returns the single ValkeyNode to delete next in ordered
// teardown sequence: highest shard-index first, and within a shard the highest
// node-index (replicas) before node-index 0 (the primary), so replicas die before
// their primary and the highest shard winds down first. Nodes already being
// deleted are skipped only if a not-yet-deleting node remains; otherwise the
// in-flight one is returned so the caller keeps requeuing until it is gone.
func nextTeardownNode(nodes *valkeyv1alpha1.ValkeyNodeList) *valkeyv1alpha1.ValkeyNode {
	if len(nodes.Items) == 0 {
		return nil
	}
	ordered := make([]*valkeyv1alpha1.ValkeyNode, 0, len(nodes.Items))
	for i := range nodes.Items {
		ordered = append(ordered, &nodes.Items[i])
	}
	slices.SortFunc(ordered, func(a, b *valkeyv1alpha1.ValkeyNode) int {
		if sa, sb := teardownShard(a), teardownShard(b); sa != sb {
			return sb - sa // highest shard index first.
		}
		return nodeIndexOf(b) - nodeIndexOf(a) // replicas first.
	})
	// Prefer a node not yet being deleted; fall back to the first (in-flight) one.
	for _, n := range ordered {
		if n.DeletionTimestamp.IsZero() {
			return n
		}
	}
	return ordered[0]
}

// teardownShard reads a node's shard-index label as an int (0 on parse error).
func teardownShard(node *valkeyv1alpha1.ValkeyNode) int {
	v, _ := strconv.Atoi(node.Labels[naming.LabelShardIndex])
	return v
}
