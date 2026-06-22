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

package valkeynode

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// wantsPersistenceFinalizer reports whether the node should carry the
// persistent-volume-cleanup finalizer: iff persistence is set AND reclaimPolicy
// is Delete (default Retain ⇒ no finalizer, PVC survives). 04 §6.3.
func wantsPersistenceFinalizer(node *valkeyv1alpha1.ValkeyNode) bool {
	return node.Spec.Persistence != nil && node.Spec.Persistence.ReclaimPolicy == valkeyv1alpha1.ReclaimDelete
}

// reconcilePersistenceFinalizer adds/removes the persistent-volume-cleanup
// finalizer to match the desired state, and runs the teardown branch on
// deletion. It returns mutated=true (with a short requeue) when it changed the
// finalizer set or is mid-teardown, so the reconciler returns early and the next
// pass observes the new state (04 §3.1 step 1 / §6.3 / §6.4).
func (r *Reconciler) reconcilePersistenceFinalizer(
	ctx context.Context, node *valkeyv1alpha1.ValkeyNode,
) (mutated bool, res ctrl.Result, err error) {
	if !node.DeletionTimestamp.IsZero() {
		return r.reconcileTeardown(ctx, node)
	}

	switch {
	case wantsPersistenceFinalizer(node) && controllerutil.AddFinalizer(node, naming.FinalizerPVCCleanup):
		return true, ctrl.Result{RequeueAfter: requeueFinalizer}, r.update(ctx, node)
	case !wantsPersistenceFinalizer(node) && controllerutil.RemoveFinalizer(node, naming.FinalizerPVCCleanup):
		return true, ctrl.Result{RequeueAfter: requeueFinalizer}, r.update(ctx, node)
	}
	return false, ctrl.Result{}, nil
}

// reconcileTeardown handles the deletion branch: it is re-entrant and idempotent
// — it deletes the PVC (ignoring NotFound) then drops the finalizer. With the
// default Retain (no finalizer) it is never entered. A persistent PVC-delete
// failure surfaces a Warning + error and leaves the node Terminating rather than
// force-removing the finalizer (04 §6.3 / §6.4).
func (r *Reconciler) reconcileTeardown(
	ctx context.Context, node *valkeyv1alpha1.ValkeyNode,
) (mutated bool, res ctrl.Result, err error) {
	if !controllerutil.ContainsFinalizer(node, naming.FinalizerPVCCleanup) {
		// Nothing for this controller to clean; GC handles owned children.
		return true, ctrl.Result{}, nil
	}
	if err := r.cleanupPVC(ctx, node); err != nil {
		r.recorder.Eventf(node, nil, corev1.EventTypeWarning, "PVCCleanupFailed", "Cleanup", "%s", err.Error())
		return true, ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(node, naming.FinalizerPVCCleanup)
	return true, ctrl.Result{}, r.update(ctx, node)
}

// cleanupPVC deletes the node's data PVC, tolerating NotFound (already gone /
// never created). Only invoked when the finalizer is present, i.e. reclaimPolicy
// was Delete. 04 §6.3.
func (r *Reconciler) cleanupPVC(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) error {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: naming.NodePVCName(node.Name), Namespace: node.Namespace}
	if err := r.Get(ctx, key, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get pvc %s: %w", key, err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		// Deletion already in progress; nothing more to do this pass.
		return nil
	}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pvc %s: %w", key, err)
	}
	return nil
}

// update wraps Client.Update with a wrapped error for finalizer mutations.
func (r *Reconciler) update(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) error {
	if err := r.Update(ctx, node); err != nil {
		return fmt.Errorf("update node %s: %w", client.ObjectKeyFromObject(node), err)
	}
	return nil
}
