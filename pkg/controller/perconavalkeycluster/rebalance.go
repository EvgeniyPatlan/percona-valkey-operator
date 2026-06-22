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
	"fmt"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// rebalanceSlots issues ONE deterministic slot move per reconcile via the atomic
// CLUSTER MIGRATESLOTS (05 §4 / 04 §2.1 step14, GO-3.14). It only runs once all
// effective shards are at the target count; PlanRebalanceMove returns nil when
// the topology is mid scale-out (fewer/more effective primaries than desired) or
// already balanced (±1 slot), in which case rebalance is a no-op.
//
// One move per reconcile is the deliberate ~30s pacing (05 §4): it gives
// cluster-aware clients time to absorb each -MOVED and keeps ownership stable
// between moves. Before issuing, migrateSlots guards on CLUSTER GETSLOTMIGRATIONS
// (no in-flight migration) and that the destination is gossip-visible from the
// source (CR-6: never re-issue an in-flight range). On Valkey < 9.0 the atomic
// subcommand returns "unknown subcommand", wrapped by the client as an actionable
// upgrade error. Emits SlotsRebalancing / SlotsRebalancePending / SlotRebalanceFailed.
func (r *Reconciler) rebalanceSlots(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
) (bool, error) {
	log := logf.FromContext(ctx)

	move := valkey.PlanRebalanceMove(state, int(cluster.Spec.Shards))
	if move == nil {
		return false, nil // balanced or not ready to rebalance.
	}

	moved, err := r.migrateSlots(ctx, cluster, state, move)
	if err != nil {
		r.recorder.Eventf(cluster, nil, eventWarning, EventSlotRebalanceFailed, "Rebalance",
			"slot move %s (%s -> %s) failed: %s", valkey.FormatSlotRanges(move.Ranges), move.SrcID, move.DstID, err.Error())
		return false, fmt.Errorf("rebalance migrate: %w", err)
	}
	if !moved {
		// A guard (migration in flight / dst not yet gossip-visible / benign
		// not-served) deferred the move; requeue and re-plan against fresh state.
		log.V(1).Info("rebalance move deferred, waiting", "src", move.SrcID, "dst", move.DstID)
		r.recorder.Eventf(cluster, nil, eventNormal, EventSlotsRebalancePending, "Rebalance",
			"waiting to migrate %s from %s to %s", valkey.FormatSlotRanges(move.Ranges), move.SrcID, move.DstID)
		return true, nil
	}
	r.recorder.Eventf(cluster, nil, eventNormal, EventSlotsRebalancing, "Rebalance",
		"migrating %s slot(s) from %s to %s", valkey.FormatSlotRanges(move.Ranges), move.SrcID, move.DstID)
	return true, nil
}

// migrateSlots performs the guarded, single atomic CLUSTER MIGRATESLOTS for one
// planned move. It returns moved=true when the migration was issued, moved=false
// when a precondition guard deferred it (the caller requeues), and an error only
// on a genuine (non-benign) failure. Guards (05 §4):
//   - the destination ID must be gossip-visible from the source (else "unknown
//     node"); when not yet visible, defer;
//   - CLUSTER GETSLOTMIGRATIONS on the source must report no in-flight migration
//     (CR-6: never re-issue an in-flight range);
//   - an IsSlotsNotServedByNode reply (a concurrent move already completed) is
//     benign — defer and re-plan against fresh state next pass.
func (r *Reconciler) migrateSlots(
	ctx context.Context, _ *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState, move *valkey.Move,
) (bool, error) {
	src := state.NodeByID(move.SrcID)
	if src == nil || src.Client() == nil {
		return false, nil // source not scraped this pass; retry.
	}

	// Destination must be known to the source's gossip table before migration.
	if !state.GossipVisible(move.SrcID, move.DstID) {
		return false, nil
	}

	// No in-flight migration on the source (CR-6 single-move guard).
	migrations, err := src.Client().ClusterGetSlotMigrations(ctx)
	if err != nil {
		return false, fmt.Errorf("CLUSTER GETSLOTMIGRATIONS %s: %w", src.Addr, err)
	}
	if valkey.AnyMigrationInProgress(migrations) {
		return false, nil // a move is in flight; wait it out.
	}

	if err := src.Client().ClusterMigrateSlots(ctx, move.Ranges, move.DstID); err != nil {
		if valkey.IsSlotsNotServedByNode(err) {
			// A concurrent move already moved these slots — benign, re-plan.
			return false, nil
		}
		return false, err
	}
	return true, nil
}
