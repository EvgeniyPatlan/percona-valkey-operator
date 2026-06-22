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
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// Lease parameters for the per-cluster backup lock (06 §4.7). The lock is a
// single coordination.k8s.io/Lease per cluster, shared with the control plane.
const (
	// leaseDurationSeconds is the validity window of a held Lease. A holder that
	// stops renewing (operator crash, Job eviction) ages out past this and the
	// next actor reacquires it; the smart-update gate fails open (§4.7, §4.8). The
	// holder refreshes renewTime on every Running reconcile pass (cadence
	// requeueJobWatch), well inside this window, leaderelection-style.
	leaseDurationSeconds int32 = 30
)

// LeaseName returns the per-cluster mutual-exclusion Lease name
// valkey-<cluster>-backup-lock (06 §4.7). It is exported so the M6 smart-update
// gate (and the restore controller) read the same Lease.
func LeaseName(cluster string) string {
	return "valkey-" + cluster + "-backup-lock"
}

// leaseHolderID returns the holder identity stamped into the Lease for a backup:
// "backup/<namespace>/<name>". A distinct holder string lets the control plane
// attribute the lock and lets a backup recognise its OWN held Lease on a requeue
// (idempotent re-acquire) versus one held by a restore/smart-update.
func leaseHolderID(bk *valkeyv1alpha1.PerconaValkeyBackup) string {
	return "backup/" + bk.Namespace + "/" + bk.Name
}

// now returns the reconciler's clock time (injectable for deterministic tests).
func (r *Reconciler) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// acquireClusterLease tries to acquire (or re-acquire / renew) the per-cluster
// backup Lease for this backup. It returns got=true when this backup now holds a
// fresh Lease. It returns got=false (no error) when another actor holds a still-
// valid Lease — the caller leaves the backup Starting and requeues (06 §4.7).
//
// Fail-open semantics (06 §4.7): an absent Lease is created and acquired; a Lease
// whose renewTime is stale beyond leaseDurationSeconds is treated as free and
// reacquired (the previous holder crashed or was evicted). Only a fresh Lease
// held by a DIFFERENT holder blocks acquisition.
func (r *Reconciler) acquireClusterLease(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) (bool, error) {
	name := LeaseName(bk.Spec.ClusterName)
	holder := leaseHolderID(bk)
	nowTime := r.now()

	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Name: name, Namespace: bk.Namespace}
	err := r.Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
		return r.createLease(ctx, bk, name, holder, nowTime)
	}
	if err != nil {
		return false, fmt.Errorf("get backup lease %q: %w", name, err)
	}

	if held, byUs := leaseHeldByOther(lease, holder, nowTime); held && !byUs {
		// A different actor holds a still-valid Lease — back off.
		return false, nil
	}

	// Free / expired / already ours: (re)claim and renew.
	return r.renewLeaseObject(ctx, lease, holder, nowTime)
}

// createLease creates the per-cluster Lease held by this backup.
func (r *Reconciler) createLease(
	ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, name, holder string, nowTime time.Time,
) (bool, error) {
	dur := leaseDurationSeconds
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: bk.Namespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			AcquireTime:          &metav1.MicroTime{Time: nowTime},
			RenewTime:            &metav1.MicroTime{Time: nowTime},
		},
	}
	if err := r.Create(ctx, lease); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost the create race; the next reconcile re-evaluates the holder.
			return false, nil
		}
		return false, fmt.Errorf("create backup lease %q: %w", name, err)
	}
	return true, nil
}

// renewLeaseObject claims/renews an existing Lease for this holder and persists
// the renewTime (and holder/acquireTime when reclaiming an expired Lease).
func (r *Reconciler) renewLeaseObject(ctx context.Context, lease *coordinationv1.Lease, holder string, nowTime time.Time) (bool, error) {
	dur := leaseDurationSeconds
	reclaiming := lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = &dur
	if reclaiming || lease.Spec.AcquireTime == nil {
		lease.Spec.AcquireTime = &metav1.MicroTime{Time: nowTime}
	}
	lease.Spec.RenewTime = &metav1.MicroTime{Time: nowTime}
	if err := r.Update(ctx, lease); err != nil {
		if apierrors.IsConflict(err) {
			// Concurrent renew/claim; re-evaluate next pass rather than error.
			return false, nil
		}
		return false, fmt.Errorf("renew backup lease %q: %w", lease.Name, err)
	}
	return true, nil
}

// renewClusterLease refreshes the renewTime of a Lease this backup already holds
// while its Job runs (06 §4.7 auto-renew). It is a no-op (no error) if the Lease
// is gone or held by someone else — the caller should not treat that as fatal.
func (r *Reconciler) renewClusterLease(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) error {
	name := LeaseName(bk.Spec.ClusterName)
	holder := leaseHolderID(bk)
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Name: name, Namespace: bk.Namespace}
	if err := r.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get backup lease %q for renew: %w", name, err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		return nil
	}
	_, err := r.renewLeaseObject(ctx, lease, holder, r.now())
	return err
}

// releaseClusterLease releases the per-cluster Lease if this backup holds it, by
// clearing the holder identity and zeroing the renewTime so the next actor sees
// it as free (06 §4.7 release on terminal). It is idempotent: a missing Lease or
// one held by another actor is a no-op success.
func (r *Reconciler) releaseClusterLease(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) error {
	name := LeaseName(bk.Spec.ClusterName)
	holder := leaseHolderID(bk)
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Name: name, Namespace: bk.Namespace}
	if err := r.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get backup lease %q for release: %w", name, err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		return nil
	}
	base := lease.DeepCopy()
	lease.Spec.HolderIdentity = nil
	lease.Spec.RenewTime = nil
	lease.Spec.AcquireTime = nil
	if err := r.Patch(ctx, lease, client.MergeFrom(base)); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("release backup lease %q: %w", name, err)
	}
	return nil
}

// leaseHeldByOther reports whether the Lease is currently held by a still-valid
// holder, and whether that holder is us. A Lease is "held" only if it has a
// holder identity AND its renewTime is within leaseDurationSeconds of now;
// otherwise it is expired/free and fails open (06 §4.7).
func leaseHeldByOther(lease *coordinationv1.Lease, holder string, nowTime time.Time) (held, byUs bool) {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return false, false
	}
	if lease.Spec.RenewTime == nil {
		return false, false
	}
	dur := leaseDurationSeconds
	if lease.Spec.LeaseDurationSeconds != nil {
		dur = *lease.Spec.LeaseDurationSeconds
	}
	expiry := lease.Spec.RenewTime.Add(time.Duration(dur) * time.Second)
	if !nowTime.Before(expiry) {
		// renewTime is stale beyond leaseDurationSeconds — treat as free.
		return false, false
	}
	return true, *lease.Spec.HolderIdentity == holder
}

// IsBackupRunning reports whether a backup currently holds the per-cluster Lease
// in the given namespace, i.e. whether the cluster controller's smart-update path
// must pause (the consumer side of the §4.7 mutual exclusion, equivalent to
// Percona's isBackupRunning()). It fails open: a missing or expired Lease returns
// false (assume no backup running) so cluster rolls/slot moves resume rather than
// wedging on a phantom holder. M6 calls this from the smart-update gate.
func IsBackupRunning(ctx context.Context, c client.Client, namespace, cluster string, nowTime time.Time) (bool, error) {
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Name: LeaseName(cluster), Namespace: namespace}
	if err := c.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get backup lease for cluster %q: %w", cluster, err)
	}
	// Any still-valid holder (backup, restore or smart-update) counts as "busy".
	held, _ := leaseHeldByOther(lease, "", nowTime)
	return held, nil
}

// clusterSnapshotReady reports whether the source cluster is in a state safe to
// snapshot: it must exist, be Ready, and not be mid-rebalance/scale (the topology
// freeze precondition, 06 §4.4). A nil error with ready=false means "not yet —
// requeue"; the returned reason explains why for the status/event.
func (r *Reconciler) clusterSnapshotReady(
	ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup,
) (ready bool, reason string, err error) {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	key := types.NamespacedName{Name: bk.Spec.ClusterName, Namespace: bk.Namespace}
	if gerr := r.Get(ctx, key, cluster); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return false, "source cluster not found", nil
		}
		return false, "", fmt.Errorf("get cluster %q for snapshot-readiness: %w", bk.Spec.ClusterName, gerr)
	}
	if cluster.Status.State != valkeyv1alpha1.StateReady {
		return false, fmt.Sprintf("source cluster %q is %q, not Ready", cluster.Name, cluster.Status.State), nil
	}
	return true, "", nil
}
