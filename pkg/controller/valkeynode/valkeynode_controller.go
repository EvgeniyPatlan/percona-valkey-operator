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

// Package valkeynode implements the ValkeyNode (vkn) controller: the internal
// single-pod infrastructure controller. It converges exactly ONE Valkey pod —
// a 1-replica StatefulSet (durable) or Deployment (cache), its PVC and mounted
// config — applies the live-settable CONFIG SET allowlist hot, rolls the pod on
// a serverConfigHash change, and publishes status.{ready,role,podIP} read from
// the live pod and INFO. It NEVER reasons about slots, shards, or sibling nodes
// (docs/architecture/04-control-plane.md §1, §3).
package valkeynode

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// Requeue intervals (04 §9 requeue taxonomy).
const (
	requeueFinalizer = 1 * time.Second  // finalizer set mutated
	requeueNotReady  = 10 * time.Second // pod not Ready, before live config
	requeueSteady    = 60 * time.Second // node steady re-verify
)

// Reconciler converges exactly ONE Valkey pod. It never reasons about
// slots/shards/siblings.
type Reconciler struct {
	client.Client
	scheme        *runtime.Scheme
	recorder      events.EventRecorder
	clientFactory valkey.ClientFactory
	// skipNameValidation lets parallel envtest specs register more than one
	// manager-backed controller of this kind in a single process; production
	// SetupWithManager leaves it false.
	skipNameValidation bool
}

// +kubebuilder:rbac:groups=valkey.percona.com,resources=valkeynodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.percona.com,resources=valkeynodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.percona.com,resources=valkeynodes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims/status,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile runs the ValkeyNode pipeline phases 1-7 (04 §3.1).
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Phase 0: fetch. NotFound ⇒ GC handles owned children.
	node := &valkeyv1alpha1.ValkeyNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Phase 1: persistence finalizer (+ deletion teardown branch).
	if mutated, res, err := r.reconcilePersistenceFinalizer(ctx, node); err != nil || mutated {
		return res, err
	}
	if !node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Phase 2: workloadType dispatch guard (immutable; Deployment forbids persistence).
	if err := guardWorkloadType(node); err != nil {
		return ctrl.Result{}, r.failNode(ctx, node, "InvalidWorkloadType", err)
	}

	// Phase 3 (ConfigMap mount) + Phase 4 (workload + PVC).
	if err := r.ensureConfigMap(ctx, node); err != nil {
		return ctrl.Result{}, r.failNode(ctx, node, "ConfigMap", err)
	}
	if err := r.ensurePVC(ctx, node); err != nil {
		return ctrl.Result{}, r.failNode(ctx, node, "PersistentVolumeClaim", err)
	}
	if err := r.reconcileWorkload(ctx, node); err != nil {
		return ctrl.Result{}, r.failNode(ctx, node, "Workload", err)
	}

	// PVC conditions reflect the live PVC before readiness gating.
	if err := r.setPVCConditions(ctx, node); err != nil {
		return ctrl.Result{}, r.failNode(ctx, node, "PersistentVolumeClaim", err)
	}

	// Phase 5: live config BEFORE readiness derivation.
	pod, err := r.getManagedPod(ctx, node)
	if err != nil {
		return ctrl.Result{}, r.failNode(ctx, node, "PodLookup", err)
	}
	refreshInMemoryStatus(node, pod)

	if !isPodReady(pod) {
		// Pod not ready yet: derive (clears role/ready) and eagerly write status.
		if derr := r.deriveStatus(ctx, node, pod, nil); derr != nil {
			return ctrl.Result{}, r.failNode(ctx, node, "Status", derr)
		}
		if werr := r.writeStatus(ctx, node); werr != nil {
			return ctrl.Result{}, werr
		}
		log.V(1).Info("pod not ready, requeue", "node", node.Name)
		return ctrl.Result{RequeueAfter: requeueNotReady}, nil
	}

	vc, err := r.clientFactory.For(ctx, node)
	if err != nil {
		return ctrl.Result{}, r.failNode(ctx, node, "Connect", err)
	}
	defer func() { _ = vc.Close() }()

	if err := r.applyLiveConfig(ctx, node, vc); err != nil {
		// Fail-closed: force status.ready=false so the cluster roll halts on this
		// node (04 §3.1 step 5/6). The LiveConfigApplied=False condition is already
		// set by applyLiveConfig; reflect it in Ready and write eagerly.
		node.Status.Ready = false
		setCondition(node, valkeyv1alpha1.NodeConditionReady, metav1.ConditionFalse, "LiveConfigApplyFailed", err.Error())
		if werr := r.writeStatus(ctx, node); werr != nil {
			return ctrl.Result{}, werr
		}
		return ctrl.Result{RequeueAfter: requeueSteady}, nil
	}

	// Phase 6: status ready/role/podIP from the live pod + INFO.
	if err := r.deriveStatus(ctx, node, pod, vc); err != nil {
		return ctrl.Result{}, r.failNode(ctx, node, "Status", err)
	}
	node.Status.ObservedGeneration = node.Generation
	if err := r.writeStatus(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	// Phase 7: steady requeue.
	return ctrl.Result{RequeueAfter: requeueSteady}, nil
}

// guardWorkloadType enforces the workloadType invariants at reconcile time
// (defence-in-depth behind CEL): a recognised type, and no persistence with
// Deployment (04 §3.1 step 2 / 03 §4.1 rule 1).
func guardWorkloadType(node *valkeyv1alpha1.ValkeyNode) error {
	switch node.Spec.WorkloadType {
	case valkeyv1alpha1.WorkloadStatefulSet:
		return nil
	case valkeyv1alpha1.WorkloadDeployment:
		if node.Spec.Persistence != nil {
			return fmt.Errorf("persistence is forbidden with workloadType Deployment")
		}
		return nil
	case "":
		// Empty defaults to StatefulSet (the CRD marker default), but a node built
		// without defaulting should still be guarded; treat empty as StatefulSet.
		return nil
	default:
		return fmt.Errorf("unsupported workloadType %q", node.Spec.WorkloadType)
	}
}

// reconcileWorkload dispatches on workloadType to create/update the 1-replica
// StatefulSet or Deployment via CreateOrUpdate with the owner ref (04 §3.1 step 4).
func (r *Reconciler) reconcileWorkload(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) error {
	labels := naming.NodeLabels(node.Name, node.Labels)
	switch effectiveWorkloadType(node) {
	case valkeyv1alpha1.WorkloadDeployment:
		desired, err := buildDeployment(node, labels)
		if err != nil {
			return err
		}
		obj := &appsv1.Deployment{}
		obj.Name, obj.Namespace = desired.Name, desired.Namespace
		_, err = k8s.CreateOrUpdate(ctx, r.Client, r.scheme, node, obj, func() error {
			obj.Labels = desired.Labels
			obj.Spec = desired.Spec
			return nil
		})
		return err
	default:
		desired, err := buildStatefulSet(node, labels)
		if err != nil {
			return err
		}
		obj := &appsv1.StatefulSet{}
		obj.Name, obj.Namespace = desired.Name, desired.Namespace
		_, err = k8s.CreateOrUpdate(ctx, r.Client, r.scheme, node, obj, func() error {
			obj.Labels = desired.Labels
			// VolumeClaimTemplates are immutable after creation; only set them on
			// create to avoid an update error.
			if obj.CreationTimestamp.IsZero() {
				obj.Spec = desired.Spec
				return nil
			}
			vcts := obj.Spec.VolumeClaimTemplates
			obj.Spec = desired.Spec
			obj.Spec.VolumeClaimTemplates = vcts
			return nil
		})
		return err
	}
}

// effectiveWorkloadType resolves an empty workloadType to the StatefulSet default.
func effectiveWorkloadType(node *valkeyv1alpha1.ValkeyNode) valkeyv1alpha1.WorkloadType {
	if node.Spec.WorkloadType == "" {
		return valkeyv1alpha1.WorkloadStatefulSet
	}
	return node.Spec.WorkloadType
}

// ensureConfigMap renders the node's own ConfigMap when the parent did NOT
// supply one. When spec.serverConfigMapName is set the ConfigMap is owned by the
// cluster controller (M3) and this step is SKIPPED (04 §3.1 step 3).
func (r *Reconciler) ensureConfigMap(ctx context.Context, node *valkeyv1alpha1.ValkeyNode) error {
	if node.Spec.ServerConfigMapName != "" {
		return nil
	}
	labels := naming.NodeLabels(node.Name, node.Labels)
	cm := &corev1.ConfigMap{}
	cm.Name, cm.Namespace = naming.NodeConfigMapName(node.Name), node.Namespace
	_, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, node, cm, func() error {
		cm.Labels = labels
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		// M2 standalone-node placeholder config. The cluster controller renders the
		// real valkey.conf in M3 and supplies it via serverConfigMapName.
		if _, ok := cm.Data["valkey.conf"]; !ok {
			cm.Data["valkey.conf"] = "# rendered by the cluster controller (M3); standalone placeholder\n"
		}
		return nil
	})
	return err
}

// failNode records a Warning event, sets Ready=False with the given reason,
// eagerly writes status, and returns the error so controller-runtime backs off.
func (r *Reconciler) failNode(ctx context.Context, node *valkeyv1alpha1.ValkeyNode, reason string, err error) error {
	r.recorder.Eventf(node, nil, corev1.EventTypeWarning, reason, reason, "%s", err.Error())
	node.Status.Ready = false
	setCondition(node, valkeyv1alpha1.NodeConditionReady, metav1.ConditionFalse, reason, err.Error())
	if werr := r.writeStatus(ctx, node); werr != nil {
		logf.FromContext(ctx).Error(werr, "status writeback failed after node error")
	}
	return err
}
