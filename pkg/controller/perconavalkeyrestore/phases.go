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

package perconavalkeyrestore

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// errUnknownPhase builds the error recorded when the phase annotation holds a value
// this build does not understand (corruption / a forward-version annotation).
func errUnknownPhase(p restorePhase) error {
	return fmt.Errorf("restore is in an unknown phase %q", p)
}

// reconcilePending resolves the source, reads the manifest FIRST (06 §7.5), and runs
// every pre-provision gate (one-of source, schema, full slot coverage, partial gate,
// shard count). On success it records the validated coverage and advances to
// Provisioning; any failure is terminal-Failed before a cluster is created (06 §9.3).
func (r *Reconciler) reconcilePending(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	r.recorder.Eventf(rst, nil, eventNormal, EventRestoreStarted, "Resolve", "Resolving restore source")

	src, err := r.resolveSource(ctx, rst)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "SourceResolveFailed", err)
	}

	man, err := r.readManifest(ctx, src)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "ManifestUnavailable", err)
	}

	verdict, err := validateCompat(rst, man)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "IncompatibleSource", err)
	}

	log.V(1).Info("restore source validated", "cluster", src.Cluster, "backup", src.Backup,
		"shards", len(man.Shards), "coverage", verdict.Detail)
	if err := r.setPhaseAnnotation(ctx, rst, map[string]string{
		annTargetCluster: targetClusterName(rst),
		annRestoredSlots: verdict.Detail,
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.advance(ctx, rst, phaseProvisioning, "source validated; provisioning target cluster"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileProvisioning provisions (or adopts) the target cluster with the seed +
// restored-from markers and advances to Seeding. Re-reads the manifest (it is the
// source of truth for shard count and is cheap from a FakeStore / object store).
func (r *Reconciler) reconcileProvisioning(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore) (ctrl.Result, error) {
	src, err := r.resolveSource(ctx, rst)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "SourceResolveFailed", err)
	}
	man, err := r.readManifest(ctx, src)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "ManifestUnavailable", err)
	}

	cluster, err := r.provisionTargetCluster(ctx, rst, man)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "ProvisionFailed", err)
	}

	r.recorder.Eventf(rst, cluster, eventNormal, EventRestoreSeeding, "Seed",
		"Seeding %d shard(s) into %s with appendonly no so dump.rdb loads", len(man.Shards), cluster.Name)
	if err := r.advance(ctx, rst, phaseSeeding, "target cluster provisioned; seeding RDB per shard"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileSeeding waits until the seed override + restored-from markers are in place
// on the target cluster (the cluster/node controller injects the seed init container
// and boots the engine with appendonly no — 06 §7.4). It is the assertion point for
// the CR-8 "never let AOF shadow the seeded RDB" invariant. Once the markers hold it
// advances to Forming.
func (r *Reconciler) reconcileSeeding(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore) (ctrl.Result, error) {
	cluster, err := r.getTargetCluster(ctx, rst)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "TargetMissing", err)
	}
	if cluster == nil {
		// Target not yet observable in cache; back off and retry.
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// The appendonly-no seed override MUST be present or the engine would load an
	// empty AOF and silently restore zero keys (06 §7.4, R3). Re-stamp if missing
	// (e.g. the markers were stripped) rather than proceeding unsafely.
	if !seedOverrideApplied(cluster) || !restoreMarkerApplied(cluster) {
		src, rerr := r.resolveSource(ctx, rst)
		if rerr != nil {
			return ctrl.Result{}, r.fail(ctx, rst, "SourceResolveFailed", rerr)
		}
		man, merr := r.readManifest(ctx, src)
		if merr != nil {
			return ctrl.Result{}, r.fail(ctx, rst, "ManifestUnavailable", merr)
		}
		if serr := r.stampRestoreMarkers(ctx, cluster, rst, man); serr != nil {
			return ctrl.Result{}, r.fail(ctx, rst, "SeedMarkerFailed", serr)
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if err := r.advance(ctx, rst, phaseForming, "RDB seeded (appendonly no); waiting for topology re-form"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileForming waits for the cluster controller to re-form topology — MEET all
// seeded primaries, ADDSLOTSRANGE the exact manifest slot map, REPLICATE the replicas
// (06 §7.5 steps 1-3) — surfaced as the cluster's ClusterFormed condition. Once the
// cluster has formed it advances to Validating.
func (r *Reconciler) reconcileForming(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore) (ctrl.Result, error) {
	cluster, err := r.getTargetCluster(ctx, rst)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "TargetMissing", err)
	}
	if cluster == nil || !clusterFormed(cluster) {
		// Still re-forming; the Watches(cluster) mapping re-enqueues on progress.
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	r.recorder.Eventf(rst, cluster, eventNormal, EventRestoreClusterFormed, "Formed",
		"Target cluster %s re-formed topology; validating slot coverage", cluster.Name)
	if err := r.advance(ctx, rst, phaseValidating, "cluster re-formed; validating 16384-slot coverage"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileValidating gates Succeeded on proven full 16384-slot coverage AND the
// target cluster reaching Ready (all replica links up — 06 §7.5 steps 4-5). It does
// NOT auto-delete a cluster that fails to reach full coverage; it leaves the
// partially-built cluster for inspection (06 §9.3) and keeps requeuing so a slow
// re-form still completes, only declaring failure when the cluster itself reports a
// terminal Failed state.
func (r *Reconciler) reconcileValidating(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore) (ctrl.Result, error) {
	cluster, err := r.getTargetCluster(ctx, rst)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, rst, "TargetMissing", err)
	}
	if cluster == nil {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if cluster.Status.State == valkeyv1alpha1.StateFailed {
		return ctrl.Result{}, r.fail(ctx, rst, "ClusterFailed",
			fmt.Errorf("target cluster %s reported Failed during re-form (%s) — left for inspection", cluster.Name, cluster.Status.Reason))
	}

	if !clusterSlotsComplete(cluster) || !clusterReady(cluster) {
		// Not all 16384 slots assigned / links not yet up. Keep waiting — the
		// cluster controller is still re-forming (06 §7.5).
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	r.recorder.Eventf(rst, cluster, eventNormal, EventRestoreSucceeded, "Succeeded",
		"Restore complete: target cluster %s reports all 16384 slots assigned and Ready", cluster.Name)
	if err := r.advance(ctx, rst, phaseSucceeded, "restore complete: all 16384 slots covered, cluster Ready"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// getTargetCluster fetches the restore's target PerconaValkeyCluster. It returns
// (nil, nil) when the cluster is not (yet) present so a caller can back off — except
// while Forming/Validating, where a vanished target is genuinely missing. The phase
// handlers decide how to treat nil.
func (r *Reconciler) getTargetCluster(
	ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore,
) (*valkeyv1alpha1.PerconaValkeyCluster, error) {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	key := types.NamespacedName{Name: targetClusterName(rst), Namespace: rst.Namespace}
	if err := r.Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get target cluster %s: %w", key, err)
	}
	return cluster, nil
}
