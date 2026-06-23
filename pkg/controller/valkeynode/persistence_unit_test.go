package valkeynode

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

func pvcWith(phase corev1.PersistentVolumeClaimPhase, capacity string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey-n-data"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
	if capacity != "" {
		pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}
	}
	return pvc
}

func TestPVCReadyCondition(t *testing.T) {
	if s, _, _ := pvcReadyCondition(nil); s != metav1.ConditionFalse {
		t.Error("nil PVC should be not-ready")
	}
	if s, r, _ := pvcReadyCondition(pvcWith(corev1.ClaimPending, "")); s != metav1.ConditionFalse || r != reasonPVCPending {
		t.Errorf("pending PVC: status=%v reason=%v", s, r)
	}
	if s, r, _ := pvcReadyCondition(pvcWith(corev1.ClaimBound, "1Gi")); s != metav1.ConditionTrue || r != reasonPVCBound {
		t.Errorf("bound PVC: status=%v reason=%v", s, r)
	}
}

func TestPVCSizeReadyCondition(t *testing.T) {
	node := unitNode("n")
	node.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{Size: resource.MustParse("2Gi")}

	if s, _, _ := pvcSizeReadyCondition(node, nil); s != metav1.ConditionFalse {
		t.Error("nil PVC size should be not-ready")
	}
	if s, _, _ := pvcSizeReadyCondition(node, pvcWith(corev1.ClaimPending, "")); s != metav1.ConditionFalse {
		t.Error("pending PVC size should be not-ready")
	}

	// Bound but capacity below requested -> resize pending.
	if s, r, _ := pvcSizeReadyCondition(node, pvcWith(corev1.ClaimBound, "1Gi")); s != metav1.ConditionFalse || r != reasonResizePending {
		t.Errorf("undersized: status=%v reason=%v", s, r)
	}
	// Bound and satisfied.
	if s, r, _ := pvcSizeReadyCondition(node, pvcWith(corev1.ClaimBound, "2Gi")); s != metav1.ConditionTrue || r != reasonResizeSatisfied {
		t.Errorf("satisfied: status=%v reason=%v", s, r)
	}

	// Resizing condition surfaces ResizeInProgress.
	resizing := pvcWith(corev1.ClaimBound, "1Gi")
	resizing.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: corev1.PersistentVolumeClaimResizing, Message: "growing"}}
	if s, r, m := pvcSizeReadyCondition(node, resizing); s != metav1.ConditionFalse || r != reasonResizeInProgress || m != "growing" {
		t.Errorf("resizing: status=%v reason=%v msg=%v", s, r, m)
	}

	// Controller resize error surfaces ResizeInfeasible.
	infeasible := pvcWith(corev1.ClaimBound, "1Gi")
	infeasible.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: corev1.PersistentVolumeClaimControllerResizeError}}
	if _, r, _ := pvcSizeReadyCondition(node, infeasible); r != reasonResizeInfeasible {
		t.Errorf("controller resize error reason = %v, want infeasible", r)
	}

	// AllocatedResourceStatuses infeasible path.
	allocInfeasible := pvcWith(corev1.ClaimBound, "1Gi")
	allocInfeasible.Status.AllocatedResourceStatuses = map[corev1.ResourceName]corev1.ClaimResourceStatus{
		corev1.ResourceStorage: corev1.PersistentVolumeClaimNodeResizeInfeasible,
	}
	if _, r, _ := pvcSizeReadyCondition(node, allocInfeasible); r != reasonResizeInfeasible {
		t.Errorf("alloc infeasible reason = %v, want infeasible", r)
	}

	// Bound with no reported capacity -> resize pending.
	if _, r, _ := pvcSizeReadyCondition(node, pvcWith(corev1.ClaimBound, "")); r != reasonResizePending {
		t.Errorf("no-capacity reason = %v, want resize pending", r)
	}
}

func TestResizeMessage(t *testing.T) {
	if got := resizeMessage("custom", "n", "fb"); got != "custom" {
		t.Errorf("resizeMessage with msg = %q", got)
	}
	if got := resizeMessage("", "valkey-n-data", "resize failed"); got != "PersistentVolumeClaim valkey-n-data resize failed" {
		t.Errorf("resizeMessage fallback = %q", got)
	}
}

func TestStorageClassChanged(t *testing.T) {
	fast, fast2, slow, empty := "fast", "fast", "slow", ""

	// Unset desired (nil or "") => "use default"; never a change, even when the
	// live PVC has a defaulted concrete class (the false-positive the guard must
	// not raise on a cluster that omits storageClassName).
	if storageClassChanged(nil, nil) {
		t.Error("nil/nil must not be a change")
	}
	if storageClassChanged(&slow, nil) {
		t.Error("nil desired must not be a change even when live class is set")
	}
	if storageClassChanged(&slow, &empty) {
		t.Error("empty desired must not be a change")
	}
	// Live class not yet bound: nothing to compare against.
	if storageClassChanged(nil, &fast) {
		t.Error("nil live (unbound) must not be a change")
	}
	// Explicit, matching class => not a change.
	if storageClassChanged(&fast, &fast2) {
		t.Error("fast->fast must not be a change")
	}
	// Explicit, differing class => the real immutable violation.
	if !storageClassChanged(&fast, &slow) {
		t.Error("fast->slow must be a change")
	}
}

func TestBoolToStatus(t *testing.T) {
	if boolToStatus(true) != metav1.ConditionTrue || boolToStatus(false) != metav1.ConditionFalse {
		t.Error("boolToStatus mapping wrong")
	}
}

func TestValkeyNodeOwnerName(t *testing.T) {
	ctrlTrue := true
	ctrlFalse := false
	refs := []metav1.OwnerReference{
		{Kind: "Other", Name: "x", Controller: &ctrlTrue},
		{Kind: "ValkeyNode", Name: "n-0-0", Controller: &ctrlTrue},
	}
	if got := valkeyNodeOwnerName(refs); got != "n-0-0" {
		t.Errorf("owner name = %q, want n-0-0", got)
	}
	// Non-controller ValkeyNode ref is ignored.
	nonCtrl := []metav1.OwnerReference{{Kind: "ValkeyNode", Name: "n", Controller: &ctrlFalse}}
	if got := valkeyNodeOwnerName(nonCtrl); got != "" {
		t.Errorf("non-controller ref should be ignored, got %q", got)
	}
	if got := valkeyNodeOwnerName(nil); got != "" {
		t.Errorf("nil refs = %q, want empty", got)
	}
}

func TestEffectiveWorkloadType(t *testing.T) {
	n := unitNode("n")
	n.Spec.WorkloadType = ""
	if effectiveWorkloadType(n) != valkeyv1alpha1.WorkloadStatefulSet {
		t.Error("empty workloadType should default to StatefulSet")
	}
	n.Spec.WorkloadType = valkeyv1alpha1.WorkloadDeployment
	if effectiveWorkloadType(n) != valkeyv1alpha1.WorkloadDeployment {
		t.Error("explicit Deployment preserved")
	}
}

func TestWantsPersistenceFinalizer(t *testing.T) {
	n := unitNode("n")
	if wantsPersistenceFinalizer(n) {
		t.Error("no persistence => no finalizer")
	}
	n.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi"), ReclaimPolicy: valkeyv1alpha1.ReclaimRetain}
	if wantsPersistenceFinalizer(n) {
		t.Error("Retain => no finalizer")
	}
	n.Spec.Persistence.ReclaimPolicy = valkeyv1alpha1.ReclaimDelete
	if !wantsPersistenceFinalizer(n) {
		t.Error("Delete => finalizer")
	}
}
