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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

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
	patch := client.MergeFrom(cluster.DeepCopy())
	added := controllerutil.AddFinalizer(cluster, naming.FinalizerDeletePodsInOrder)
	if controllerutil.AddFinalizer(cluster, naming.FinalizerDeleteSSL) {
		added = true
	}
	if !added {
		return nil
	}
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("add cluster finalizers: %w", err)
	}
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
	log.Info("cluster teardown complete; finalizers removed, GC proceeds")
	return ctrl.Result{}, nil
}

// cleanupTLS is the delete-ssl finalizer body: a no-op stub until M5 provisions
// operator-issued TLS material (04 §6.1 / scope: TLS provisioning is M5). It
// exists so the finalizer is real and the teardown flow is complete now.
func (r *Reconciler) cleanupTLS(_ context.Context, _ *valkeyv1alpha1.PerconaValkeyCluster) error {
	// TODO(M5): delete operator-issued TLS material not GC'd by owner refs.
	return nil
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
