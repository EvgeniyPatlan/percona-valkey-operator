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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// Ready condition reasons.
const (
	reasonReady       = "Ready"
	reasonPodNotReady = "PodNotReady"
	reasonGated       = "GatesNotMet"
)

// roleFromInfo maps the engine INFO role token to the Percona-API NodeRole. The
// CR NEVER surfaces master/slave (05 §10). Unknown ⇒ "" (role not yet readable).
func roleFromInfo(infoRepl map[string]string) valkeyv1alpha1.NodeRole {
	switch infoRepl[valkey.InfoKeyRole] {
	case valkey.InfoRoleMaster:
		return valkeyv1alpha1.NodeRolePrimary
	case valkey.InfoRoleSlave:
		return valkeyv1alpha1.NodeRoleReplica
	default:
		return ""
	}
}

// getManagedPod selects the single pod managed by this node by listing on the
// Charter topology labels scoped to the node's namespace (OQ-2.2 interim). It
// returns (nil, nil) when 0 or >1 pods match (treated as not-Ready), so a
// mid-roll transient (old+new pod) does not derive a wrong role.
func (r *Reconciler) getManagedPod(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) (*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(node.Namespace),
		client.MatchingLabels(selectorLabels(naming.NodeLabels(node.Name, node.Labels)))); err != nil {
		return nil, fmt.Errorf("list pods for node %s: %w", node.Name, err)
	}
	if len(pods.Items) != 1 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

// refreshInMemoryStatus updates the in-memory node.Status.{PodName,PodIP,Ready}
// from the live pod BEFORE applyLiveConfig consumes them, beating the status-lag
// pitfall (04 §3 pitfall). Ready here is only the pod's own Ready condition; the
// final gated Ready is computed in deriveStatus.
func refreshInMemoryStatus(node *valkeyv1alpha1.ValkeyNode, pod *corev1.Pod) {
	if pod == nil {
		node.Status.PodName = ""
		node.Status.PodIP = ""
		node.Status.Ready = false
		return
	}
	node.Status.PodName = pod.Name
	node.Status.PodIP = pod.Status.PodIP
	node.Status.Ready = isPodReady(pod)
}

// setPVCConditions sets/clears the PVC conditions from the live PVC. When
// persistence is unset both conditions are forced True (no PVC to gate on) so
// the readiness gate in deriveStatus passes.
func (r *Reconciler) setPVCConditions(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) error {
	if node.Spec.Persistence == nil {
		setCondition(node, valkeyv1alpha1.NodeConditionPVCReady, metav1.ConditionTrue, reasonPersistenceDisabled, "persistence disabled")
		setCondition(node, valkeyv1alpha1.NodeConditionPVCSizeReady, metav1.ConditionTrue, reasonPersistenceDisabled, "persistence disabled")
		return nil
	}
	pvc, err := r.getPVC(ctx, node)
	if err != nil {
		return err
	}
	rs, rr, rm := pvcReadyCondition(pvc)
	setCondition(node, valkeyv1alpha1.NodeConditionPVCReady, rs, rr, rm)
	ss, sr, sm := pvcSizeReadyCondition(node, pvc)
	setCondition(node, valkeyv1alpha1.NodeConditionPVCSizeReady, ss, sr, sm)
	return nil
}

// deriveStatus sets podName/podIP/role and the Ready condition from the live pod
// and INFO. Role is read from live INFO replication only when the pod is Ready
// (never from nodeIndex). Ready is True iff pod Ready AND PVC gates AND
// LiveConfigApplied=True (04 §3.1 step 6 / 03 §6).
func (r *Reconciler) deriveStatus(
	ctx context.Context, node *valkeyv1alpha1.ValkeyNode, pod *corev1.Pod, vc valkey.ConfigClient,
) error {
	if pod == nil {
		node.Status.PodName = ""
		node.Status.PodIP = ""
		node.Status.Ready = false
		node.Status.Role = ""
		setCondition(node, valkeyv1alpha1.NodeConditionReady, metav1.ConditionFalse, reasonPodNotReady, "pod does not exist yet")
		return nil
	}
	node.Status.PodName = pod.Name
	node.Status.PodIP = pod.Status.PodIP

	podReady := isPodReady(pod)
	if podReady && vc != nil {
		repl, err := vc.InfoReplication(ctx)
		if err != nil {
			return fmt.Errorf("read INFO replication: %w", err)
		}
		node.Status.Role = roleFromInfo(repl)
	}

	gatesOK := conditionTrue(node, valkeyv1alpha1.NodeConditionPVCReady) &&
		conditionTrue(node, valkeyv1alpha1.NodeConditionPVCSizeReady) &&
		conditionTrue(node, valkeyv1alpha1.NodeConditionLiveConfigApplied)
	ready := podReady && gatesOK
	node.Status.Ready = ready

	switch {
	case ready:
		setCondition(node, valkeyv1alpha1.NodeConditionReady, metav1.ConditionTrue, reasonReady, "pod is running and ready")
	case !podReady:
		setCondition(node, valkeyv1alpha1.NodeConditionReady, metav1.ConditionFalse, reasonPodNotReady, "pod is not ready")
	default:
		setCondition(node, valkeyv1alpha1.NodeConditionReady, metav1.ConditionFalse, reasonGated, "pod ready but gating conditions not met")
	}
	return nil
}

// writeStatus persists the node status subresource via the shared re-fetch+patch
// helper (04 §9 re-fetch-before-update).
func (r *Reconciler) writeStatus(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) error {
	return k8s.WriteStatus(ctx, r.Client, node, func(n *valkeyv1alpha1.ValkeyNode) *valkeyv1alpha1.ValkeyNodeStatus {
		return &n.Status
	})
}
