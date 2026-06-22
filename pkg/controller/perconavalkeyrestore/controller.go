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

// Package perconavalkeyrestore implements the PerconaValkeyRestore (pvk-restore)
// controller: it resolves a backup source (backupName XOR backupSource), reads
// the manifest, validates engine/crVersion compatibility and shard count,
// provisions a fresh PerconaValkeyCluster from clusterTemplate (the NewCluster
// strategy), seeds each shard's RDB via a download init container (boot with
// appendonly no, re-enable AOF post-load), re-forms the exact manifest slot map
// via the M3 MEET/ADDSLOTSRANGE/REPLICATE helpers, and marks Succeeded only when
// all 16384 slots are proven covered.
//
// The phase machine maps the doc-06 §7.2 conceptual phases (Pending →
// Provisioning → Seeding → Forming → Validating → Succeeded/Failed) onto the
// locked v1alpha1 RestoreState enum via status.go; the conceptual phase is tracked
// in an operator-managed annotation (patched with client.MergeFrom, never a full
// Update) so the CRD stays frozen in this leg while the full phase logic ships.
// See docs/implementation/05-phase4-backup-restore.md (GO-4.13 .. GO-4.15).
package perconavalkeyrestore

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// requeueAfter is the backoff between reconciles while the restore waits on an
// asynchronous step (the target cluster seeding / re-forming). It mirrors the
// cluster controller's steady cadence so a restore advances promptly once the
// cluster controller makes progress, without hot-looping.
const requeueAfter = 5 * time.Second

// Reconciler drives one PerconaValkeyRestore: source resolution, target-cluster
// provisioning, RDB seeding, topology re-form and slot-coverage validation.
type Reconciler struct {
	client.Client
	scheme   *runtime.Scheme
	recorder events.EventRecorder
	// storeFactory is the injectable ArtifactStore seam used to read the backup
	// manifest from object storage (06 §7.5). Tests inject a backup.FakeStore;
	// production resolves a concrete backend. Defaulted in SetupWithManager.
	//
	// TODO(GO-4.13): use storeFactory to ReadManifest from the resolved source.
	storeFactory StoreFactory
	// skipNameValidation lets parallel envtest specs register more than one
	// manager-backed controller of this kind in a single process.
	skipNameValidation bool
}

// StoreFactory builds an ArtifactStore from a resolved StorageConfig (the
// restore-side seam over backup.NewStore so tests can inject a FakeStore).
type StoreFactory func(ctx context.Context, cfg backup.StorageConfig) (backup.ArtifactStore, error)

// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeybackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyclusters,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=valkey.percona.com,resources=valkeynodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;get;update

// Reconcile is the PerconaValkeyRestore reconcile loop and phase machine (06 §7).
// It fetches the CR (NotFound-tolerant), short-circuits terminal restores, then
// dispatches on the conceptual phase (tracked in an annotation, projected onto the
// locked status.state enum):
//
//	Pending      -> resolve source (backupName XOR backupSource), read manifest,
//	                validate compat + full slot coverage, then advance Provisioning.
//	Provisioning -> provision/adopt the target cluster with the seed + restored-from
//	                markers (appendonly no so dump.rdb loads, 06 §7.4), advance Seeding.
//	Seeding      -> wait until the markers are in place (the cluster/node controller
//	                injects the seed init container); advance Forming.
//	Forming      -> wait until the cluster controller re-forms topology
//	                (ClusterFormed), then advance Validating (06 §7.5 steps 1-3).
//	Validating   -> gate Succeeded on proven full 16384-slot coverage + cluster Ready
//	                (06 §7.5 steps 4-5); a gap leaves the cluster for inspection (§9.3).
//
// Every transition persists the phase via a MergeFrom patch (never a full Update).
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rst := &valkeyv1alpha1.PerconaValkeyRestore{}
	if err := r.Get(ctx, req.NamespacedName, rst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if isTerminal(rst) {
		return ctrl.Result{}, nil
	}

	switch currentPhase(rst) {
	case phasePending:
		return r.reconcilePending(ctx, rst)
	case phaseProvisioning:
		return r.reconcileProvisioning(ctx, rst)
	case phaseSeeding:
		return r.reconcileSeeding(ctx, rst)
	case phaseForming:
		return r.reconcileForming(ctx, rst)
	case phaseValidating:
		return r.reconcileValidating(ctx, rst)
	default:
		// An unknown phase is recorded loudly rather than silently looping.
		return ctrl.Result{}, r.fail(ctx, rst, EventRestoreFailed, errUnknownPhase(currentPhase(rst)))
	}
}
