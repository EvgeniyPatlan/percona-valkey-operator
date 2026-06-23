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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// PVC condition reasons (03 §6 / 08 §4.2).
const (
	reasonPVCPending          = "PersistentVolumeClaimPending"
	reasonPVCBound            = "PersistentVolumeClaimBound"
	reasonResizeInProgress    = "PersistentVolumeClaimResizeInProgress"
	reasonResizeInfeasible    = "PersistentVolumeClaimResizeInfeasible"
	reasonResizePending       = "PersistentVolumeClaimResizePending"
	reasonResizeSatisfied     = "PersistentVolumeClaimSizeSatisfied"
	reasonImmutableViolation  = "PersistentVolumeClaimImmutable"
	reasonPersistenceDisabled = "PersistenceDisabled"
)

// guardPVCImmutable rejects a shrink or a storageClass change between the current
// and desired PVC specs (defence-in-depth behind CEL). Expand is allowed. 03 §2.5.
func guardPVCImmutable(cur, desired *corev1.PersistentVolumeClaim) error {
	if cur == nil || desired == nil {
		return nil
	}
	curReq := cur.Spec.Resources.Requests[corev1.ResourceStorage]
	desReq := desired.Spec.Resources.Requests[corev1.ResourceStorage]
	if desReq.Cmp(curReq) < 0 {
		return fmt.Errorf("persistence.size may only be expanded (current %s, desired %s)", curReq.String(), desReq.String())
	}
	if storageClassChanged(cur.Spec.StorageClassName, desired.Spec.StorageClassName) {
		return fmt.Errorf("persistence.storageClassName is immutable")
	}
	return nil
}

// storageClassChanged reports whether the desired storage class is an immutable
// *change* from the current one. A nil/empty desired class means "use the cluster
// default" (spec.persistence.storageClassName was left unset); the API server's
// default-StorageClass admission then stamps a concrete class onto the live PVC
// (e.g. "standard"). Comparing that defaulted live class against the nil desired
// must NOT be flagged as a change, or the guard fires a false positive on every
// reconcile of a cluster that omits storageClassName. The change is real only when
// the user EXPLICITLY pins a class that differs from the bound one.
func storageClassChanged(cur, desired *string) bool {
	if desired == nil || *desired == "" {
		return false // unset desired => default; never a violation.
	}
	if cur == nil {
		return false // live class not yet bound; nothing to compare against.
	}
	return *cur != *desired
}

// ensurePVC creates the standalone data PVC, or applies an expand-only resize to
// an existing bound one. For a StatefulSet-backed node the PVC is normally owned
// by the STS volumeClaimTemplate, so this only acts when the PVC already exists
// independently or to drive a resize; STS templates do not propagate size
// changes, so the reconciler patches the live PVC directly here. Returns nil
// (no-op) when persistence is unset. 04 §3.1 step 4 / 05 §9.
func (r *Reconciler) ensurePVC(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) error {
	if node.Spec.Persistence == nil {
		return nil
	}
	labels := naming.NodeLabels(node.Name, node.Labels)
	desired := buildPVCTemplate(node, labels)

	cur := &corev1.PersistentVolumeClaim{}
	// The StatefulSet controller materializes the volumeClaimTemplate as
	// <vctName>-<stsName>-0, NOT the bare NodePVCName (the VCT name), so the live
	// PVC must be read/resized under the suffixed name.
	key := types.NamespacedName{Name: naming.NodeStatefulSetPVCName(node.Name), Namespace: node.Namespace}
	if err := r.Get(ctx, key, cur); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get pvc %s: %w", key, err)
		}
		// Not present yet: for STS it will be created by the volumeClaimTemplate.
		// We do not create it directly to avoid fighting the STS controller.
		return nil
	}

	if err := guardPVCImmutable(cur, desired); err != nil {
		return err
	}

	// Only patch storage on a bound claim (the API server rejects request
	// mutations on pending/unbound claims), and only to grow it.
	if cur.Status.Phase != corev1.ClaimBound {
		return nil
	}
	curReq := cur.Spec.Resources.Requests[corev1.ResourceStorage]
	desReq := desired.Spec.Resources.Requests[corev1.ResourceStorage]
	if desReq.Cmp(curReq) <= 0 {
		return nil
	}
	patched := cur.DeepCopy()
	if patched.Spec.Resources.Requests == nil {
		patched.Spec.Resources.Requests = corev1.ResourceList{}
	}
	patched.Spec.Resources.Requests[corev1.ResourceStorage] = desReq
	if err := r.Update(ctx, patched); err != nil {
		return fmt.Errorf("expand pvc %s: %w", key, err)
	}
	return nil
}

// getPVC reads the node's data PVC, returning (nil, nil) when absent or when
// persistence is unset.
func (r *Reconciler) getPVC(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) (*corev1.PersistentVolumeClaim, error) {
	if node.Spec.Persistence == nil {
		return nil, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	// Read the StatefulSet-materialized PVC (<vctName>-<stsName>-0), not the bare
	// volumeClaimTemplate name which never exists as a standalone object.
	key := types.NamespacedName{Name: naming.NodeStatefulSetPVCName(node.Name), Namespace: node.Namespace}
	if err := r.Get(ctx, key, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get pvc %s: %w", key, err)
	}
	return pvc, nil
}

// pvcReadyCondition derives the PersistentVolumeClaimReady condition from the
// live PVC bind phase. 08 §4.2.
func pvcReadyCondition(pvc *corev1.PersistentVolumeClaim) (metav1.ConditionStatus, string, string) {
	if pvc == nil {
		return metav1.ConditionFalse, reasonPVCPending, "PersistentVolumeClaim does not exist yet"
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		phase := pvc.Status.Phase
		if phase == "" {
			phase = corev1.ClaimPending
		}
		return metav1.ConditionFalse, reasonPVCPending, fmt.Sprintf("PersistentVolumeClaim %s is %s", pvc.Name, phase)
	}
	return metav1.ConditionTrue, reasonPVCBound, fmt.Sprintf("PersistentVolumeClaim %s is bound", pvc.Name)
}

// pvcSizeReadyCondition derives the PersistentVolumeClaimSizeReady condition,
// surfacing ResizeInProgress/ResizeInfeasible reasons from the live PVC. 08 §4.2.
func pvcSizeReadyCondition(node *valkeyv1alpha1.ValkeyNode, pvc *corev1.PersistentVolumeClaim) (metav1.ConditionStatus, string, string) {
	if pvc == nil {
		return metav1.ConditionFalse, reasonResizePending, "PersistentVolumeClaim does not exist yet"
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return metav1.ConditionFalse, reasonResizePending, fmt.Sprintf("PersistentVolumeClaim %s not bound yet", pvc.Name)
	}

	for _, cond := range pvc.Status.Conditions {
		switch cond.Type {
		case corev1.PersistentVolumeClaimControllerResizeError, corev1.PersistentVolumeClaimNodeResizeError:
			return metav1.ConditionFalse, reasonResizeInfeasible, resizeMessage(cond.Message, pvc.Name, "resize failed")
		case corev1.PersistentVolumeClaimResizing:
			return metav1.ConditionFalse, reasonResizeInProgress, resizeMessage(cond.Message, pvc.Name, "resize is in progress")
		case corev1.PersistentVolumeClaimFileSystemResizePending:
			return metav1.ConditionFalse, reasonResizePending, resizeMessage(cond.Message, pvc.Name, "awaiting filesystem resize")
		}
	}

	switch pvc.Status.AllocatedResourceStatuses[corev1.ResourceStorage] {
	case corev1.PersistentVolumeClaimControllerResizeInfeasible, corev1.PersistentVolumeClaimNodeResizeInfeasible:
		return metav1.ConditionFalse, reasonResizeInfeasible, fmt.Sprintf("PersistentVolumeClaim %s resize cannot be satisfied", pvc.Name)
	case corev1.PersistentVolumeClaimControllerResizeInProgress, corev1.PersistentVolumeClaimNodeResizeInProgress:
		return metav1.ConditionFalse, reasonResizeInProgress, fmt.Sprintf("PersistentVolumeClaim %s resize is in progress", pvc.Name)
	case corev1.PersistentVolumeClaimNodeResizePending:
		return metav1.ConditionFalse, reasonResizePending, fmt.Sprintf("PersistentVolumeClaim %s awaiting node-side resize", pvc.Name)
	}

	capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]
	if !ok {
		return metav1.ConditionFalse, reasonResizePending, fmt.Sprintf("PersistentVolumeClaim %s has no reported capacity yet", pvc.Name)
	}
	if capacity.Cmp(node.Spec.Persistence.Size) < 0 {
		return metav1.ConditionFalse, reasonResizePending,
			fmt.Sprintf("PersistentVolumeClaim %s requested %s but capacity is %s", pvc.Name, node.Spec.Persistence.Size.String(), capacity.String())
	}
	return metav1.ConditionTrue, reasonResizeSatisfied,
		fmt.Sprintf("PersistentVolumeClaim %s satisfies requested size %s", pvc.Name, node.Spec.Persistence.Size.String())
}

// resizeMessage returns cond.Message or a sensible fallback.
func resizeMessage(msg, name, fallback string) string {
	if msg != "" {
		return msg
	}
	return fmt.Sprintf("PersistentVolumeClaim %s %s", name, fallback)
}
