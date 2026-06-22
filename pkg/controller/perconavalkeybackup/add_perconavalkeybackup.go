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

package perconavalkeybackup

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllercfg "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	"valkey.percona.com/percona-valkey-operator/pkg/controller"
)

// controllerName is the controller-runtime name for the backup controller.
const controllerName = "perconavalkeybackup"

func init() {
	// Register with the central controller fan-out so cmd/manager wires us in
	// without importing this package by name (02 §3, §9).
	controller.Register(func(mgr manager.Manager) error {
		return (&Reconciler{}).SetupWithManager(mgr)
	})
}

// SetupWithManager wires the controller: For(PerconaValkeyBackup), Owns the
// backup Job (owner-ref enqueue so a Job phase change re-reconciles the backup —
// GO-4.9), and Watches the referenced PerconaValkeyCluster mapped back to the
// backups that target it (so a cluster becoming Ready unblocks a Starting backup).
// It defaults the injectable storeFactory to backup.NewStore; tests override it
// before SetupWithManager (06 §8.1 mockable seam).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.scheme = mgr.GetScheme()
	r.recorder = mgr.GetEventRecorder(controllerName)
	if r.storeFactory == nil {
		r.storeFactory = backup.NewStore
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyv1alpha1.PerconaValkeyBackup{}).
		Owns(&batchv1.Job{}).
		Watches(&valkeyv1alpha1.PerconaValkeyCluster{}, handler.EnqueueRequestsFromMapFunc(r.mapClusterToBackups)).
		Named(controllerName).
		WithOptions(controllercfg.Options{SkipNameValidation: ptrTo(r.skipNameValidation)}).
		Complete(r)
}

// mapClusterToBackups maps a PerconaValkeyCluster to reconcile requests for every
// PerconaValkeyBackup in the same namespace that targets it via spec.clusterName,
// so a cluster status change re-evaluates blocked backups (GO-4.8 topology-freeze).
func (r *Reconciler) mapClusterToBackups(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*valkeyv1alpha1.PerconaValkeyCluster)
	if !ok {
		return nil
	}
	list := &valkeyv1alpha1.PerconaValkeyBackupList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		bk := &list.Items[i]
		if bk.Spec.ClusterName != cluster.Name {
			continue
		}
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: bk.Name, Namespace: bk.Namespace},
		})
	}
	return reqs
}

// ptrTo returns a pointer to v.
func ptrTo[T any](v T) *T { return &v }
