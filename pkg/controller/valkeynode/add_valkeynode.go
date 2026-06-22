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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllercfg "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// controllerName is the controller-runtime name for the ValkeyNode controller.
const controllerName = "valkeynode"

// Owner-reference kind strings used to walk Pod → workload → ValkeyNode.
const (
	kindValkeyNode  = "ValkeyNode"
	kindStatefulSet = "StatefulSet"
	kindReplicaSet  = "ReplicaSet"
	kindDeployment  = "Deployment"
)

func init() {
	// Register with the central controller fan-out so cmd/manager wires us in
	// without importing this package by name (02 §3, §9).
	controller.Register(func(mgr manager.Manager) error {
		return (&Reconciler{}).SetupWithManager(mgr)
	})
}

// SetupWithManager wires the controller: For(ValkeyNode), Owns the workload
// kinds + ConfigMap, and Watches Pods mapped to their owning ValkeyNode so a pod
// Ready/IP change enqueues without the 60s steady wait (04 §5 View B). It
// defaults the injectable ClientFactory to the real valkey-go factory; tests
// override it before SetupWithManager (04 §3 mockable seam).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.scheme = mgr.GetScheme()
	r.recorder = mgr.GetEventRecorder(controllerName)
	if r.clientFactory == nil {
		r.clientFactory = valkey.NewClientFactory(mgr.GetClient())
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyv1alpha1.ValkeyNode{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPodToValkeyNode)).
		Named(controllerName).
		WithOptions(controllercfg.Options{SkipNameValidation: ptrTo(r.skipNameValidation)}).
		Complete(r)
}

// mapPodToValkeyNode maps a Pod to a reconcile request for its owning ValkeyNode
// by walking the owner-reference chain: a Pod is owned by the StatefulSet /
// Deployment (or its ReplicaSet), which is owned by the ValkeyNode. Because the
// Pod's direct controller is the workload (not the node), we resolve the node by
// the Charter cluster/shard/node labels the pod carries instead, scoped to the
// pod namespace (04 §5 View B).
func (r *Reconciler) mapPodToValkeyNode(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	nodeName := nodeNameFromPod(ctx, r.Client, pod)
	if nodeName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: nodeName, Namespace: pod.Namespace}}}
}

// nodeNameFromPod resolves the owning ValkeyNode name for a pod by walking up the
// workload owner reference (Pod → STS/Deployment/ReplicaSet → ValkeyNode). It
// reads the workload's controller owner ref of kind ValkeyNode. Returns "" when
// the pod is not part of a ValkeyNode workload.
func nodeNameFromPod(ctx context.Context, c client.Client, pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case kindStatefulSet:
			return nodeFromWorkloadOwner(ctx, c, &appsv1.StatefulSet{}, ref.Name, pod.Namespace)
		case kindReplicaSet:
			return nodeFromReplicaSet(ctx, c, ref.Name, pod.Namespace)
		}
	}
	return ""
}

// nodeFromWorkloadOwner fetches a workload object and returns its controlling
// ValkeyNode owner-reference name, if any.
func nodeFromWorkloadOwner(ctx context.Context, c client.Client, obj client.Object, name, namespace string) string {
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		return ""
	}
	return valkeyNodeOwnerName(obj.GetOwnerReferences())
}

// nodeFromReplicaSet resolves Pod → ReplicaSet → Deployment → ValkeyNode.
func nodeFromReplicaSet(ctx context.Context, c client.Client, rsName, namespace string) string {
	rs := &appsv1.ReplicaSet{}
	if err := c.Get(ctx, types.NamespacedName{Name: rsName, Namespace: namespace}, rs); err != nil {
		return ""
	}
	for _, ref := range rs.OwnerReferences {
		if ref.Kind == kindDeployment {
			return nodeFromWorkloadOwner(ctx, c, &appsv1.Deployment{}, ref.Name, namespace)
		}
	}
	return ""
}

// valkeyNodeOwnerName returns the name of a ValkeyNode controller owner ref.
func valkeyNodeOwnerName(refs []metav1.OwnerReference) string {
	for _, ref := range refs {
		if ref.Kind == kindValkeyNode && ref.Controller != nil && *ref.Controller {
			return ref.Name
		}
	}
	return ""
}
