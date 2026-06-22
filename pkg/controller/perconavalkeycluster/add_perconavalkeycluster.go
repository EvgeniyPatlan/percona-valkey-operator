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

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllercfg "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller"
	"valkey.percona.com/percona-valkey-operator/pkg/platform"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// controllerName is the controller-runtime name for the cluster controller.
const controllerName = "perconavalkeycluster"

func init() {
	// Register with the central fan-out so cmd/manager wires us in without
	// importing this package by name (02 §3, §9).
	controller.Register(func(mgr manager.Manager) error {
		return (&Reconciler{}).SetupWithManager(mgr)
	})
}

// SetupWithManager wires the controller: For(PerconaValkeyCluster), Owns the
// Service/ConfigMap/Secret/PodDisruptionBudget/ValkeyNode children (owner-ref
// enqueue + GC; a ValkeyNode status flip re-enqueues the owner so steps 6/15 see
// progress), and Watches PerconaValkeyBackup/PerconaValkeyRestore mapped to
// spec.clusterName (pause/resume + backup gating, wired here for 2b). Leader
// election is enabled at the manager level (cmd/manager). It defaults the
// injectable ClusterClientFactory to the real valkey-go factory; tests override
// it before SetupWithManager (04 §3 / 05 §10 mockable seam, CR-18). 04 §5 View A.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.scheme = mgr.GetScheme()
	r.recorder = mgr.GetEventRecorder(controllerName)
	if r.clientFactory == nil {
		r.clientFactory = valkey.NewClusterClientFactory(mgr.GetClient())
	}
	if r.platform == "" {
		r.platform = detectPlatform()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyv1alpha1.PerconaValkeyCluster{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&valkeyv1alpha1.ValkeyNode{}).
		Watches(&valkeyv1alpha1.PerconaValkeyBackup{}, handler.EnqueueRequestsFromMapFunc(mapBackupToCluster)).
		Watches(&valkeyv1alpha1.PerconaValkeyRestore{}, handler.EnqueueRequestsFromMapFunc(mapRestoreToCluster)).
		Named(controllerName).
		WithOptions(controllercfg.Options{SkipNameValidation: ptrTo(r.skipNameValidation)}).
		Complete(r)
}

// detectPlatform maps the runtime platform to the leaf-safe API Platform value
// (OQ-1.3: the reconciler converts platform.Detect() at the call site so
// pkg/apis stays leaf — it never imports pkg/platform).
func detectPlatform() valkeyv1alpha1.Platform {
	if platform.Detect() == platform.OpenShift {
		return valkeyv1alpha1.PlatformOpenShift
	}
	return valkeyv1alpha1.PlatformVanilla
}

// mapBackupToCluster maps a PerconaValkeyBackup to its target cluster's reconcile
// request via spec.clusterName (04 §5: restore/backup pause-resume + gating).
func mapBackupToCluster(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*valkeyv1alpha1.PerconaValkeyBackup)
	if !ok || b.Spec.ClusterName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: b.Spec.ClusterName, Namespace: b.Namespace}}}
}

// mapRestoreToCluster maps a PerconaValkeyRestore to its target cluster's
// reconcile request via spec.clusterName (04 §5).
func mapRestoreToCluster(_ context.Context, obj client.Object) []reconcile.Request {
	rst, ok := obj.(*valkeyv1alpha1.PerconaValkeyRestore)
	if !ok || rst.Spec.ClusterName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: rst.Spec.ClusterName, Namespace: rst.Namespace}}}
}

// ptrTo returns a pointer to v.
func ptrTo[T any](v T) *T { return &v }
