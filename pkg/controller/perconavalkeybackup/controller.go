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

// Package perconavalkeybackup implements the PerconaValkeyBackup (pvk-backup)
// controller: the thin orchestrator that turns one PerconaValkeyBackup CR into a
// set of N per-shard RDB snapshots plus a manifest in object storage. It resolves
// the named storage and credential Secret, acquires the per-cluster Lease and
// sets the backup-in-progress marker (mutual exclusion with restore/smart-update),
// creates and watches the single cmd/valkey-backup Job, hydrates status from the
// manifest, gates Succeeded on full 16384-slot coverage, and drives finalizer-
// based artifact GC (manifest-first, crash-safe).
//
// FOUNDATION STUB (M4 shared foundation): this file ships a compiling Reconciler
// + SetupWithManager + an empty Reconcile so sibling legs B/C never touch the
// shared controller registry or cmd/manager. The phase machine, Lease, Job
// builder and finalizer are filled in by GO-4.6 .. GO-4.10 / GO-4.12.
// See docs/implementation/05-phase4-backup-restore.md.
package perconavalkeybackup

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// Requeue intervals for the backup phase machine (04 §9 requeue taxonomy).
const (
	// requeueLockBackoff is the backoff while waiting for the per-cluster Lease /
	// the source cluster to become snapshot-ready (06 §4.7).
	requeueLockBackoff = 5 * time.Second
	// requeueJobWatch is the poll cadence while a backup Job is in flight.
	requeueJobWatch = 3 * time.Second
	// requeueCleanupRetry is the backoff while a finalizer cleanup Job runs or a
	// stuck Terminating backup is re-driven by the finalizer-audit (06 §6.1).
	requeueCleanupRetry = 5 * time.Second
)

// Reconciler orchestrates one PerconaValkeyBackup into a backup-set in object
// storage. It owns the single backup Job and never moves data itself (06 §4.1).
type Reconciler struct {
	client.Client
	scheme   *runtime.Scheme
	recorder events.EventRecorder
	// storeFactory is the injectable ArtifactStore seam (06 §8.1). Tests inject a
	// backup.FakeStore; production resolves a concrete S3/GCS/Azure backend from
	// the named storage + credentials Secret. Defaulted in SetupWithManager.
	storeFactory StoreFactory
	// clock is the injectable time source (Lease renew/expiry, completion stamps,
	// scheduling). Tests inject a fake clock; nil falls back to time.Now.
	clock func() time.Time
	// skipNameValidation lets parallel envtest specs register more than one
	// manager-backed controller of this kind in a single process; production
	// SetupWithManager leaves it false.
	skipNameValidation bool
}

// StoreFactory builds an ArtifactStore from a resolved StorageConfig. It is the
// controller-side seam over backup.NewStore so tests can inject a FakeStore.
type StoreFactory func(ctx context.Context, cfg backup.StorageConfig) (backup.ArtifactStore, error)

// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeybackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeybackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeybackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=valkey.percona.com,resources=valkeynodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;get;update

// Reconcile is the PerconaValkeyBackup reconcile loop and phase machine
// ("" → Starting → Running → Succeeded/Failed) (06 §4.6). It dispatches first on
// the deletion timestamp (finalizer cleanup, 06 §6) then on status.state.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("backup", req.String())

	bk := &valkeyv1alpha1.PerconaValkeyBackup{}
	if err := r.Get(ctx, req.NamespacedName, bk); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion branch: run the manifest-first finalizer cleanup (06 §6, §6.1).
	if !bk.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, bk)
	}

	// Register the artifact-GC finalizer up front so a delete that arrives mid-life
	// always finds it (06 §6). Persisted via metadata-only PATCH.
	if err := r.ensureFinalizer(ctx, bk); err != nil {
		return ctrl.Result{}, err
	}

	if isTerminal(bk) {
		// Terminal states are sticky; nothing to do (retention is schedule-driven).
		return ctrl.Result{}, nil
	}

	return r.advance(ctx, log, bk)
}

// advance runs one step of the non-terminal phase machine. New/Starting resolves
// storage, acquires the Lease and creates the Job; Running watches the Job to a
// terminal verdict (06 §4.6).
func (r *Reconciler) advance(ctx context.Context, log logr.Logger, bk *valkeyv1alpha1.PerconaValkeyBackup) (ctrl.Result, error) {
	rs, err := r.checkNSetDefaults(ctx, bk)
	if err != nil {
		return r.terminalFail(ctx, bk, ReasonStorageError, err.Error())
	}

	switch bk.Status.State {
	case valkeyv1alpha1.BackupStateRunning:
		return r.reconcileRunning(ctx, bk, rs)
	default: // New / Starting
		return r.reconcileStart(ctx, log, bk, rs)
	}
}

// reconcileStart performs the topology-freeze acquire (Lease + cluster-ready
// precondition) and, once both hold, creates the backup Job and moves to Running
// (06 §4.4, §4.6, §4.7). On Lease contention or a not-yet-Ready cluster it leaves
// the backup Starting and requeues with backoff.
func (r *Reconciler) reconcileStart(
	ctx context.Context, log logr.Logger, bk *valkeyv1alpha1.PerconaValkeyBackup, rs *resolvedStorage,
) (ctrl.Result, error) {
	ready, reason, err := r.clusterSnapshotReady(ctx, bk)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		setState(bk, valkeyv1alpha1.BackupStateStarting, ReasonClusterNotReady+": "+reason)
		if werr := r.writeStatus(ctx, bk); werr != nil {
			return ctrl.Result{}, werr
		}
		return ctrl.Result{RequeueAfter: requeueLockBackoff}, nil
	}

	got, err := r.acquireClusterLease(ctx, bk)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !got {
		r.recorder.Eventf(bk, nil, eventNormal, EventBackupWaitingForLock, "Backup",
			"another backup/restore/smart-update holds the cluster lock; waiting")
		setState(bk, valkeyv1alpha1.BackupStateStarting, ReasonWaitingForLock+": waiting for per-cluster backup lock")
		if werr := r.writeStatus(ctx, bk); werr != nil {
			return ctrl.Result{}, werr
		}
		return ctrl.Result{RequeueAfter: requeueLockBackoff}, nil
	}

	if err := r.createBackupJob(ctx, bk, rs); err != nil {
		// Release the Lease so a failed Job-create does not wedge the cluster.
		_ = r.releaseClusterLease(ctx, bk)
		return r.terminalFail(ctx, bk, ReasonJobCreateError, err.Error())
	}
	setState(bk, valkeyv1alpha1.BackupStateRunning, ReasonJobRunning+": backup Job created")
	log.V(1).Info("backup Job created, entering Running", "job", backupJobName(bk))
	if werr := r.writeStatus(ctx, bk); werr != nil {
		return ctrl.Result{}, werr
	}
	return ctrl.Result{RequeueAfter: requeueJobWatch}, nil
}

// reconcileRunning watches the in-flight backup Job to a terminal verdict and,
// on any terminal outcome, releases the Lease (06 §4.6, §4.7, §4.8).
func (r *Reconciler) reconcileRunning(
	ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, rs *resolvedStorage,
) (ctrl.Result, error) {
	done, err := r.watchJob(ctx, bk, rs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		if werr := r.writeStatus(ctx, bk); werr != nil {
			return ctrl.Result{}, werr
		}
		return ctrl.Result{RequeueAfter: requeueJobWatch}, nil
	}
	// Terminal: release the Lease so the cluster resumes smart-update/rebalance.
	if rerr := r.releaseClusterLease(ctx, bk); rerr != nil {
		return ctrl.Result{}, rerr
	}
	if werr := r.writeStatus(ctx, bk); werr != nil {
		return ctrl.Result{}, werr
	}
	return ctrl.Result{}, nil
}

// terminalFail records a terminal failure, releases the Lease (best-effort) and
// writes status. It returns no requeue (terminal states are sticky).
func (r *Reconciler) terminalFail(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, reason, msg string) (ctrl.Result, error) {
	r.failBackup(bk, reason, msg)
	if rerr := r.releaseClusterLease(ctx, bk); rerr != nil {
		return ctrl.Result{}, rerr
	}
	return ctrl.Result{}, r.writeStatus(ctx, bk)
}
