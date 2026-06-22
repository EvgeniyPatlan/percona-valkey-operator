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
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeybackup"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// This file is the M6 SMART-UPDATE SEAM (GO-6.8/6.9/6.10/6.12). It owns the
// failover-aware engine rolling-upgrade GATE and the engine-downgrade policy that
// FRONT the EXISTING M3 step-6 roll (reconcileValkeyNodes -> rollNode ->
// proactiveFailover). It does NOT re-implement the roll or the failover — those
// are M3 GO-3.10/3.16; M6 only (a) gates them on cluster health + no-backup-running
// and (b) refuses unsafe engine downgrades before any roll (09 §5, §7).
//
// Integration point: reconcileValkeyNodes (step 6, nodes.go) already orders
// replicas-before-primary by LIVE role, performs proactive failover before a live
// primary, and consults shouldGateEngineRoll for the backup/restore gate. The
// smart-update leg adds smartUpdateAllowed (the full Ready + slots=16384 +
// replicas-synced + no-backup-running gate, GO-6.8) and the downgrade-policy check
// (GO-6.12) in front of the image stamp, reusing the M3 mechanism verbatim (no
// second roll path, GO-6.9; the primary-roll proactiveFailover reuse is GO-6.10,
// already wired in nodes.go rollNode).
//
// The upgrade gate/downgrade reason+event vocabulary is declared HERE (not
// status.go, not versioncheck.go) so the two M6 seam files stay disjoint.

// engineFloor is the Valkey feature line at/above which atomic slot migration
// (CLUSTER MIGRATESLOTS / GETSLOTMIGRATIONS) is available, so scale-in/out is
// supported (09 §7, surfacing the M3 GO-3.14/3.15 command gate). An engine
// resolved below this line draws the ReasonMigrateSlotsUnsupported advisory.
const engineFloor = 9

// engineJumpWarnMinors is the forward feature-line distance (in minor lines)
// at/above which a single engine upgrade is flagged as a multi-minor jump
// (informational only; the roll proceeds — 09 §7). A one-line step (e.g. 8.0 ->
// 9.0) is routine; two or more (e.g. 8.0 -> 9.2 spanning multiple minors, or a
// major-line skip) earns the ReasonEngineJumpWarning event.
const engineJumpWarnMinors = 2

// Smart-update gate + downgrade-policy reasons/events (09 §5, §7; GO-6.8/6.10/6.12).
// Declared in the smart-update seam to avoid collision with status.go and
// versioncheck.go.
const (
	// ReasonUpgradeGatedBackupRunning holds the engine roll while a
	// PerconaValkeyBackup is Running (deleting a pod mid-backup corrupts the
	// stream, 09 §4/§5). Maps to a Normal "deferred" event, never Degraded.
	ReasonUpgradeGatedBackupRunning = "UpgradeGatedBackupRunning"
	// ReasonUpgradeGatedNotReady holds the roll while the cluster is not Ready
	// (e.g. a concurrent crVersion bump put it Progressing, 09 §4/E9).
	ReasonUpgradeGatedNotReady = "UpgradeGatedNotReady"
	// ReasonUpgradeGatedSlotsIncomplete holds the roll until all 16384 slots are
	// assigned (cluster mode, 09 §5 health gate).
	ReasonUpgradeGatedSlotsIncomplete = "UpgradeGatedSlotsIncomplete"
	// ReasonUpgradeGatedReplicasUnsynced holds the roll until per-shard replicas
	// report master_link_status:up (09 §5 health gate).
	ReasonUpgradeGatedReplicasUnsynced = "UpgradeGatedReplicasUnsynced"
	// ReasonUnsupportedDowngrade refuses an engine feature-line downgrade
	// (e.g. 9.0.x -> 8.0.x): sets Degraded, no roll, no mutation (09 §7).
	ReasonUnsupportedDowngrade = "UnsupportedDowngrade"
	// ReasonEngineJumpWarning warns on an allowed engine multi-minor forward jump
	// (e.g. 8.0 -> 9.2): informational, the roll proceeds (09 §7).
	ReasonEngineJumpWarning = "EngineJumpWarning"
	// ReasonMigrateSlotsUnsupported advises that a resolved engine < 9.0 cannot do
	// atomic slot migration, so scale-in/out is unsupported on it (09 §7,
	// surfacing the M3 GO-3.14/3.15 command gate at the upgrade layer).
	ReasonMigrateSlotsUnsupported = "MigrateSlotsUnsupported"
)

// engineChangeKind classifies the current->target engine feature-line move
// (09 §7). It governs whether the smart update refuses (downgrade), warns (a
// multi-minor forward jump), or proceeds silently.
type engineChangeKind int

const (
	// engineChangeNone means the feature line is unchanged or could not be
	// determined (no engine roll to police — the roll is config-driven).
	engineChangeNone engineChangeKind = iota
	// engineChangePatch is a forward patch-level move within the same feature
	// line (e.g. 9.0.1 -> 9.0.2): always permitted, no event.
	engineChangePatch
	// engineChangeMinorForward is a forward feature-line move within the
	// routine one-step distance (e.g. 8.0 -> 9.0): permitted, no event.
	engineChangeMinorForward
	// engineChangeMultiMinorJump is a forward feature-line move spanning
	// engineJumpWarnMinors or more (e.g. 8.0 -> 9.2): permitted but warned.
	engineChangeMultiMinorJump
	// engineChangeFeatureLineDowngrade is a backward feature-line move
	// (e.g. 9.0.x -> 8.0.x): refused outright (no roll, Degraded).
	engineChangeFeatureLineDowngrade
)

// engineVersion is the parsed feature line + patch of an engine image tag
// (09 §7: Valkey numbers feature lines 7.2/8.0/9.0; a change of the leading
// line is a feature-line change, a change within it is a patch-level change).
// known is false when the tag carried no parseable numeric version.
type engineVersion struct {
	major int
	minor int
	patch int
	known bool
}

// reconcileSmartUpdate is the engine smart-update HOOK the M3 step-6 roll calls.
// It fronts ONLY the engine-image roll (the two-axis model, 09 §1): when the
// pending roll does not change the engine image (a pure config-hash roll, M3's
// own mechanism), it returns allowed=true immediately so the existing M3 ordering
// runs unchanged — the smart-update gate and downgrade policy apply only to an
// engine upgrade, never to a config roll.
//
// When an engine-image change IS pending it (a) refuses an unsafe feature-line
// downgrade with Degraded/UnsupportedDowngrade and no roll (09 §7), warns on a
// multi-minor forward jump / sub-9.0 floor; then (b) gates the roll on cluster
// health (all 16384 slots assigned + per-shard replicas synced + no failed
// primary + not genuinely Degraded) and NO PerconaValkeyBackup running (CR-14).
// While gated the caller requeues and touches no data pods; when allowed, the
// existing M3 reconcileValkeyNodes ordering (replicas-before-primary, one shard
// at a time, proactive failover before a live primary) does the actual roll
// unchanged (GO-6.9/6.10, no second roll path).
//
// It returns allowed=true when the roll may proceed this pass, and a reason that
// the caller surfaces (a gate/downgrade reason from this file) when allowed=false.
func (r *Reconciler) reconcileSmartUpdate(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
) (allowed bool, reason string) {
	current, changing, err := r.pendingEngineChange(ctx, cluster)
	if err != nil {
		// A node-list read failure must not silently bypass the gate; hold this
		// pass and retry (a transient API hiccup, surfaced as gated-not-ready).
		logf.FromContext(ctx).V(1).Info("engine-change detection failed; deferring roll",
			"cluster", cluster.Name, "err", err.Error())
		return false, ReasonUpgradeGatedNotReady
	}
	if !changing {
		// Config-only roll (no engine-image change): not a smart engine update —
		// let the M3 roll path run unchanged (09 §1 two-axis separation).
		return true, ""
	}

	// (B) Downgrade policy (09 §7, GO-6.12): classify the engine feature-line move
	// and refuse a feature-line downgrade BEFORE any gate or roll. A multi-minor
	// forward jump is warned (informational), and a resolved engine below the 9.0
	// slot-migration floor draws the MigrateSlotsUnsupported advisory; both let
	// the roll proceed.
	if blocked, dReason := r.applyEngineDowngradePolicy(cluster, current); blocked {
		return false, dReason
	}

	// (C) Gates (09 §4, §5, GO-6.8): block — do NOT touch data pods, just requeue
	// — while a backup runs or the cluster is not healthy enough to roll safely.
	if ok, gReason := r.smartUpdateAllowed(ctx, cluster, state); !ok {
		return false, gReason
	}

	return true, ""
}

// smartUpdateAllowed is the GO-6.8 health/backup gate that fronts the engine
// roll. It passes only when no PerconaValkeyBackup holds the per-cluster Lease
// (CR-14), the cluster is not genuinely Degraded, all 16384 slots are assigned
// (cluster mode), every per-shard replica reports master_link_status:up, and no
// shard primary is failed — all read from the FRESH live scrape, not the stale
// Ready condition (the condition lags the in-pass scrape and flips Progressing
// mid-roll, which would deadlock an otherwise-healthy one-at-a-time roll). The
// order surfaces the most-specific reason first: a running backup wins, then a
// genuine Degraded, then the finer slots / replication / failed-primary checks.
// While any gate fails the caller requeues without rolling a node (09 §4, §5).
func (r *Reconciler) smartUpdateAllowed(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, state *valkey.ClusterState,
) (ok bool, reason string) {
	// Backup-running gate (CR-14): never roll mid-backup — deleting a pod
	// corrupts the snapshot stream (09 §4/§5). Read the M4 per-cluster Lease.
	if r.backupRunning(ctx, cluster) {
		return false, ReasonUpgradeGatedBackupRunning
	}

	// Degraded backstop (09 §4/E9): a genuine impairment (quorum loss, or a
	// crVersion-downgrade rejection) holds the engine roll. Degraded is a stable
	// signal set deliberately, unlike the transient Progressing a one-at-a-time
	// roll itself produces, so it does not self-deadlock the roll.
	if conditionTrue(cluster, CondDegraded) {
		return false, ReasonUpgradeGatedNotReady
	}

	// Health gates from the live scrape (the freshest signal). A nil state
	// (nothing scraped yet) is treated as not-yet-healthy.
	if state == nil {
		return false, ReasonUpgradeGatedNotReady
	}
	if len(state.GetUnassignedSlots()) != 0 {
		return false, ReasonUpgradeGatedSlotsIncomplete
	}
	if len(state.FailedPrimaries()) != 0 {
		return false, ReasonUpgradeGatedNotReady
	}
	if !state.IsReplicationInSync() {
		return false, ReasonUpgradeGatedReplicasUnsynced
	}

	return true, ""
}

// backupRunning reports whether a PerconaValkeyBackup currently holds the
// per-cluster backup Lease (the consumer side of the M4 §4.7 mutual exclusion,
// GO-3.22/GO-6.8). It fails open: a missing/expired Lease, or any read error,
// is treated as "no backup running" so a phantom holder never wedges the roll
// forever (the same fail-open contract as perconavalkeybackup.IsBackupRunning).
func (r *Reconciler) backupRunning(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	running, err := perconavalkeybackup.IsBackupRunning(ctx, r.Client, cluster.Namespace, cluster.Name, time.Now())
	if err != nil {
		logf.FromContext(ctx).V(1).Info("backup-lease read failed; treating as no backup running",
			"cluster", cluster.Name, "err", err.Error())
		return false
	}
	return running
}

// applyEngineDowngradePolicy applies the 09 §7 engine-version policy for an
// engine roll whose current running image is `current` and whose target is
// cluster.Spec.Image. A feature-line downgrade is refused (Degraded set here,
// blocked=true, reason ReasonUnsupportedDowngrade — no roll, no mutation); a
// multi-minor forward jump emits an informational ReasonEngineJumpWarning; a
// sub-9.0 target emits the ReasonMigrateSlotsUnsupported advisory. Only a
// feature-line downgrade blocks; the warning/advisory let the roll proceed.
func (r *Reconciler) applyEngineDowngradePolicy(
	cluster *valkeyv1alpha1.PerconaValkeyCluster, current string,
) (blocked bool, reason string) {
	target := cluster.Spec.Image
	switch classifyEngineChange(current, target) {
	case engineChangeFeatureLineDowngrade:
		setCondition(cluster, CondDegraded, metav1.ConditionTrue, ReasonUnsupportedDowngrade,
			"refusing engine feature-line downgrade "+current+" -> "+target)
		r.recorder.Eventf(cluster, nil, eventWarning, ReasonUnsupportedDowngrade, "SmartUpdate",
			"refusing unsupported engine downgrade from %q to %q (feature-line downgrade is not supported)", current, target)
		return true, ReasonUnsupportedDowngrade
	case engineChangeMultiMinorJump:
		r.recorder.Eventf(cluster, nil, eventNormal, ReasonEngineJumpWarning, "SmartUpdate",
			"engine multi-minor jump from %q to %q; the roll proceeds (consider the version-service Recommended pin)", current, target)
	case engineChangeNone, engineChangePatch, engineChangeMinorForward:
		// No event for a same-line / routine one-step-forward move.
	}

	// Sub-9.0 floor advisory (independent of the change kind): a resolved engine
	// below the slot-migration floor cannot scale in/out (09 §7).
	if tv := parseEngineVersion(target); tv.known && tv.major < engineFloor {
		r.recorder.Eventf(cluster, nil, eventWarning, ReasonMigrateSlotsUnsupported, "SmartUpdate",
			"resolved engine %q is below the Valkey %d.0 floor; atomic slot migration (scale-in/out) is unsupported", target, engineFloor)
	}
	return false, ""
}

// pendingEngineChange reports whether an engine-image roll is pending and the
// engine image currently stamped on the live nodes (the "current" side of the
// downgrade comparison). It returns changing=true with the old image when some
// existing node still carries an image other than cluster.Spec.Image (the node
// the pending roll will move); when every node already carries spec.image (a
// config-only roll, or no nodes yet) it returns changing=false — the smart-update
// gate and downgrade policy do not apply to a non-engine roll (09 §1 two axes).
func (r *Reconciler) pendingEngineChange(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
) (current string, changing bool, err error) {
	nodes, err := r.listClusterNodes(ctx, cluster)
	if err != nil {
		return "", false, err
	}
	for i := range nodes.Items {
		if img := nodes.Items[i].Spec.Image; img != "" && img != cluster.Spec.Image {
			return img, true, nil
		}
	}
	return cluster.Spec.Image, false, nil
}

// classifyEngineChange classifies the current->target engine move on its feature
// line (09 §7). Unparseable versions classify as engineChangeNone (no policy
// action — an opaque tag is treated as indeterminate). A backward feature line is
// a downgrade; an unchanged line with a different patch is patch-level; a single
// adjacent feature-line step forward (e.g. 8.0 -> 9.0) is the routine
// minor-forward; a wider forward span (a major skip like 7.2 -> 9.0, or a
// leading-line jump like 8.0 -> 9.2 crossing engineJumpWarnMinors minor lines) is
// a multi-minor jump that the caller warns on. The line ordinal packs minor into
// the low two digits so feature lines compare as a single monotonic key
// (7.2 < 8.0 < 9.0 < 9.2).
func classifyEngineChange(current, target string) engineChangeKind {
	cur := parseEngineVersion(current)
	tgt := parseEngineVersion(target)
	if !cur.known || !tgt.known {
		return engineChangeNone
	}
	curLine := cur.major*100 + cur.minor
	tgtLine := tgt.major*100 + tgt.minor
	switch {
	case tgtLine < curLine:
		return engineChangeFeatureLineDowngrade
	case tgtLine == curLine:
		return engineChangePatch
	case isMultiLineJump(cur, tgt):
		return engineChangeMultiMinorJump
	default:
		return engineChangeMinorForward
	}
}

// isMultiLineJump reports whether a forward engine move (cur < tgt by feature
// line) spans more than one adjacent feature line and so warrants the
// EngineJumpWarning (09 §7). A major skip of two or more (7.2 -> 9.x), a
// next-major target at minor engineJumpWarnMinors or higher (8.0 -> 9.2), or a
// same-major minor advance of engineJumpWarnMinors or more (9.0 -> 9.2+) all
// count as multi-line; a single adjacent step (8.0 -> 9.0, 9.0 -> 9.1) does not.
func isMultiLineJump(cur, tgt engineVersion) bool {
	switch {
	case tgt.major-cur.major >= engineJumpWarnMinors:
		return true
	case tgt.major == cur.major+1:
		return tgt.minor >= engineJumpWarnMinors
	case tgt.major == cur.major:
		return tgt.minor-cur.minor >= engineJumpWarnMinors
	default:
		return false
	}
}

// parseEngineVersion extracts the numeric major.minor.patch of a Valkey engine
// image tag (e.g. "percona/percona-valkey:9.0.1-2" -> 9.0.1). It takes the part
// after the last ':' (the tag), strips any '-<build>' suffix, and parses up to
// three dot-separated numeric components. A tag with no leading numeric
// component (e.g. "latest") yields known=false so the policy treats it as
// indeterminate (no downgrade refusal on an opaque tag — 09 §7 assumes clear
// engine numbering).
func parseEngineVersion(image string) engineVersion {
	tag := image
	if i := strings.LastIndex(tag, ":"); i >= 0 {
		tag = tag[i+1:]
	}
	if i := strings.IndexByte(tag, '-'); i >= 0 {
		tag = tag[:i]
	}
	parts := strings.SplitN(tag, ".", 3)
	ver := engineVersion{}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return ver // no leading numeric component: indeterminate.
	}
	ver.major, ver.known = maj, true
	if len(parts) > 1 {
		ver.minor = atoiOrZero(parts[1])
	}
	if len(parts) > 2 {
		ver.patch = atoiOrZero(parts[2])
	}
	return ver
}

// atoiOrZero parses a decimal int, returning 0 on any non-numeric input (an
// engine tag component like a trailing letter sorts as 0 rather than failing).
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
