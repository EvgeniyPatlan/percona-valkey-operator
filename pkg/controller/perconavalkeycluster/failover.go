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
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// failoverPollInterval / failoverPollTimeout are the proactive-failover poll
// cadence: poll INFO replication on the target every 1s up to 10s for role:master
// (05 §6 / 04 §10).
const (
	failoverPollInterval = 1 * time.Second
	failoverPollTimeout  = 10 * time.Second
)

// proactiveFailover gracefully demotes the live primary of the given shard before
// it is rolled, so a serving primary never goes down with the roll (05 §6 / 04
// §10, GO-3.16). It selects the highest-offset synced replica (GetSyncedReplicas
// excludes fail/pfail/fail? and requires master_link_status:up), issues a graceful
// CLUSTER FAILOVER against that replica, and polls its role until it reports
// primary (bounded 10s/1s). When the shard has NO synced replica the roll is
// DEFERRED (done=false, never FORCE while the primary is live) and the caller
// surfaces Degraded. Emits FailoverInitiated / FailoverCompleted / FailoverTimeout
// / FailoverFailed.
//
// pollRole is injectable so envtest can drive the role flip deterministically
// without a real engine and without a 10s wall-clock wait.
func (r *Reconciler) proactiveFailover(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, shardID string,
) (bool, error) {
	log := logf.FromContext(ctx)

	primary := state.PrimaryByShardID(shardID)
	if primary == nil {
		// Live primary role for the shard can't be determined — defer the roll
		// rather than risk an unknown-order restart (05 §6).
		log.V(1).Info("no live primary for shard, deferring roll", "shard", shardID)
		return false, nil
	}

	target := state.HighestOffsetReplica(primary.ID)
	if target == nil || target.Client() == nil {
		// No synced replica: defer the roll (never FORCE a live primary). The
		// caller marks Degraded; isolated nodes are re-MEET/REPLICATEd next pass.
		r.recorder.Eventf(cluster, nil, eventWarning, EventFailoverDeferred, "Failover",
			"shard %s has no synced replica; deferring primary roll", shardID)
		return false, nil
	}

	r.recorder.Eventf(cluster, nil, eventNormal, EventFailoverInitiated, "Failover",
		"initiating graceful failover of shard %s to replica %s", shardID, target.ID)
	if err := target.Client().ClusterFailover(ctx, valkey.FailoverGraceful); err != nil {
		r.recorder.Eventf(cluster, nil, eventWarning, EventFailoverFailed, "Failover",
			"CLUSTER FAILOVER on %s failed: %s", target.ID, err.Error())
		return false, err
	}

	if r.awaitRoleFlip(ctx, target) {
		r.recorder.Eventf(cluster, nil, eventNormal, EventFailoverCompleted, "Failover",
			"failover of shard %s completed; %s is now primary", shardID, target.ID)
		return true, nil
	}
	// Timeout: surface Degraded and retry next reconcile rather than escalating
	// to FORCE/TAKEOVER while the old primary is still live (05 §6).
	r.recorder.Eventf(cluster, nil, eventWarning, EventFailoverTimeout, "Failover",
		"failover of shard %s did not complete within %s; retrying", shardID, failoverPollTimeout)
	return false, nil
}

// awaitRoleFlip polls the target node's live role until it reports primary or the
// bounded timeout elapses. The poll function is the reconciler's injectable
// rolePoller so tests flip the role synchronously (no 10s sleep under envtest).
func (r *Reconciler) awaitRoleFlip(ctx context.Context, target *valkey.NodeState) bool {
	poll := r.rolePoll
	if poll == nil {
		poll = defaultRolePoll
	}
	deadline := time.Now().Add(failoverPollTimeout)
	for {
		if role := poll(ctx, target); role == valkey.RolePrimary {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(failoverPollInterval):
		}
	}
}

// defaultRolePoll reads the target's live role from INFO replication over its
// orchestration client (the production poll). A read error reports replica so the
// poll keeps waiting until the deadline.
func defaultRolePoll(ctx context.Context, target *valkey.NodeState) valkey.Role {
	raw, err := target.Client().Info(ctx, "replication")
	if err != nil {
		return valkey.RoleReplica
	}
	role, _, _, _ := valkey.ParseInfoReplicationTyped(raw)
	return role
}
