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
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// cleanupContainerName is the name of the cleanup Job container.
const cleanupContainerName = "valkey-cleanup"

// ensureFinalizer adds the percona.com/delete-backup finalizer if absent and
// persists it via a metadata-only MergeFrom PATCH (never a full Update, which
// would round-trip spec through omitempty + kubebuilder defaults and silently
// mutate fields — the M3 bug). It returns added=true when it added the finalizer
// so the caller knows the in-memory object now carries it.
func (r *Reconciler) ensureFinalizer(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) error {
	patch := client.MergeFrom(bk.DeepCopy())
	if !controllerutil.AddFinalizer(bk, naming.FinalizerDeleteBackup) {
		return nil
	}
	if err := r.Patch(ctx, bk, patch); err != nil {
		return fmt.Errorf("add backup finalizer: %w", err)
	}
	return nil
}

// handleDeletion runs the percona.com/delete-backup finalizer branch (06 §6,
// §6.1). It is idempotent and re-entrant — the finalizer-audit re-drives it for
// any backup stuck in Terminating. The order is: (1) spawn a cleanup Job that
// deletes manifest-FIRST then RDBs then the empty prefix; (2) only once the
// cleanup Job has Completed (artifacts confirmed gone) remove the finalizer so
// the API server GCs the CR. A failed/missing cleanup keeps the finalizer (the CR
// stays Terminating) and emits BackupCleanupFailed — a loud artifact leak rather
// than a silent one.
func (r *Reconciler) handleDeletion(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(bk, naming.FinalizerDeleteBackup) {
		// Our finalizer is gone; nothing left to do (GC proceeds).
		return ctrl.Result{}, nil
	}

	// Release the Lease if we somehow still hold it (deleted mid-backup) so the
	// cluster never wedges on a deleted backup's lock.
	if err := r.releaseClusterLease(ctx, bk); err != nil {
		return ctrl.Result{}, err
	}

	rs, err := r.resolveStorageForCleanup(ctx, bk)
	if err != nil {
		// Storage spec unresolvable (cluster gone / storage renamed): we cannot run
		// a cleanup Job, but the artifacts may be orphaned. Per 06 §6 we keep the
		// finalizer and surface it loudly so an operator reclaims the prefix.
		r.recorder.Eventf(bk, nil, eventWarning, EventBackupCleanupFailed, "Cleanup",
			"cannot resolve storage to clean up backup %s artifacts (%s): finalizer retained", bk.Name, err.Error())
		return ctrl.Result{RequeueAfter: requeueCleanupRetry}, nil
	}

	job, err := r.getCleanupJob(ctx, bk)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		if cerr := r.createCleanupJob(ctx, bk, rs); cerr != nil {
			return ctrl.Result{}, cerr
		}
		return ctrl.Result{RequeueAfter: requeueCleanupRetry}, nil
	}

	switch {
	case jobConditionTrue(job, batchv1.JobComplete):
		// Artifacts confirmed deleted: drop the finalizer via MergeFrom PATCH.
		r.recorder.Eventf(bk, nil, eventNormal, EventArtifactsDeleted, "Cleanup",
			"backup %s artifacts deleted (manifest-first); removing finalizer", bk.Name)
		return ctrl.Result{}, r.removeFinalizer(ctx, bk)
	case jobConditionTrue(job, batchv1.JobFailed):
		_, msg := jobFailureReason(job)
		r.recorder.Eventf(bk, job, eventWarning, EventBackupCleanupFailed, "Cleanup",
			"cleanup Job for backup %s failed (%s); finalizer retained, will retry", bk.Name, msg)
		// Delete the failed Job (Background propagation so its pods go too and the
		// object is reaped even where the GC controller is not running) so the next
		// pass re-creates a fresh one. Cleanup deletes are already-gone-safe in
		// cmd/valkey-backup --cleanup, so the retry is idempotent (06 §6.1).
		bg := metav1.DeletePropagationBackground
		if derr := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &bg}); derr != nil && !apierrors.IsNotFound(derr) {
			return ctrl.Result{}, fmt.Errorf("delete failed cleanup job: %w", derr)
		}
		return ctrl.Result{RequeueAfter: requeueCleanupRetry}, nil
	default:
		// Cleanup Job still running — requeue.
		return ctrl.Result{RequeueAfter: requeueCleanupRetry}, nil
	}
}

// removeFinalizer drops the percona.com/delete-backup finalizer via a
// metadata-only MergeFrom PATCH (never a full Update).
func (r *Reconciler) removeFinalizer(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) error {
	patch := client.MergeFrom(bk.DeepCopy())
	if !controllerutil.RemoveFinalizer(bk, naming.FinalizerDeleteBackup) {
		return nil
	}
	if err := r.Patch(ctx, bk, patch); err != nil {
		return fmt.Errorf("remove backup finalizer: %w", err)
	}
	return nil
}

// resolveStorageForCleanup re-resolves the storage spec for the cleanup Job.
// Unlike checkNSetDefaults it does NOT re-validate the credentials Secret (the
// Job mounts and uses it; the operator only needs the storage coordinates and
// Secret name to build the Job).
func (r *Reconciler) resolveStorageForCleanup(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) (*resolvedStorage, error) {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	key := types.NamespacedName{Name: bk.Spec.ClusterName, Namespace: bk.Namespace}
	if err := r.Get(ctx, key, cluster); err != nil {
		return nil, fmt.Errorf("get cluster %q: %w", bk.Spec.ClusterName, err)
	}
	return resolveStorage(cluster, bk)
}

// getCleanupJob fetches this backup's cleanup Job by name, (nil,nil) when absent.
func (r *Reconciler) getCleanupJob(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	key := types.NamespacedName{Name: cleanupJobName(bk), Namespace: bk.Namespace}
	if err := r.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get cleanup job %q: %w", key.Name, err)
	}
	return job, nil
}

// cleanupJobName returns the cleanup Job name: valkey-<backup>-cleanup.
func cleanupJobName(bk *valkeyv1alpha1.PerconaValkeyBackup) string {
	return naming.ResourcePrefix + bk.Name + "-cleanup"
}

// createCleanupJob builds and creates the cmd/valkey-backup --cleanup Job (06 §6,
// §6.1): it deletes manifest.json FIRST, then the RDB shards, then the empty
// prefix. The Job is NOT owner-referenced to the backup CR (the CR is mid-delete;
// an owner-ref would have the API server GC the Job out from under us). The
// credentials Secret is referenced (env) but never owner-referenced by us (06 §6
// — the Secret must outlive the cluster).
func (r *Reconciler) createCleanupJob(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, rs *resolvedStorage) error {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	key := types.NamespacedName{Name: bk.Spec.ClusterName, Namespace: bk.Namespace}
	if err := r.Get(ctx, key, cluster); err != nil {
		return fmt.Errorf("get cluster %q for cleanup job: %w", bk.Spec.ClusterName, err)
	}

	labels := naming.Labels(bk.Spec.ClusterName, backupComponent)
	labels[labelBackup] = bk.Name

	backoff := int32(2)
	deadline := defaultActiveDeadlineSeconds
	// Cleanup only touches object storage: it needs --cleanup plus the cluster/
	// backup names and storage coordinates (via VALKEY_BACKUP_* env), but NO engine
	// connection (no seed node / auth / TLS). The credential VALUES are mounted from
	// the credentials Secret as EnvFrom below (06 §6, §8.2).
	container := corev1.Container{
		Name:  cleanupContainerName,
		Image: backupImage(cluster),
		Args:  []string{"--cleanup"},
		Env: backupEnvFromParams(backup.JobEnvParams{
			Cluster: bk.Spec.ClusterName,
			Backup:  bk.Name,
			Spec:    rs.spec,
		}),
		Resources: backupJobResources(),
	}
	if rs.credsSecret != "" {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: rs.credsSecret},
			},
		}}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cleanupJobName(bk),
			Namespace: bk.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cluster.Spec.Backup.ServiceAccountName,
					Containers:         []corev1.Container{container},
				},
			},
		},
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create cleanup job %q: %w", job.Name, err)
	}
	return nil
}
