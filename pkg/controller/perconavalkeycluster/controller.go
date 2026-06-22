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

// Package perconavalkeycluster implements the PerconaValkeyCluster (pvk)
// controller: the user-facing topology controller. It renders the cluster's
// Service/PDB/ACL Secret/ConfigMap, creates one ValkeyNode per (shard,node)
// one-at-a-time, scrapes live CLUSTER state, and drives the strict bootstrap
// join MEET -> ADDSLOTSRANGE -> REPLICATE until a healthy sharded cluster
// forms. It NEVER touches a StatefulSet/PVC/pod directly — only ValkeyNode
// specs + CLUSTER commands (docs/architecture/04-control-plane.md §1).
//
// Wave 2a implements the pipeline THROUGH bootstrap-to-formed (phases
// 0-7,10-12,15). The scale-out (14), scale-in (13), rolling-update roll path
// (in 6) and proactive/orphan failover (8,9) phases are deferred to Wave 2b;
// each leaves a clean seam/TODO referencing its GO-3.x task id.
package perconavalkeycluster

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
	"valkey.percona.com/percona-valkey-operator/pkg/version"
)

// Requeue intervals (04 §9 requeue taxonomy).
const (
	requeueFast   = 2 * time.Second  // made progress / waiting on convergence
	requeueSteady = 30 * time.Second // healthy cluster periodic re-verify
)

// Reconciler owns the PerconaValkeyCluster topology. It holds an INJECTABLE
// ClusterClientFactory so envtest drives bootstrap against a scripted fake
// (there is no real Valkey under envtest, CR-18).
type Reconciler struct {
	client.Client
	scheme        *runtime.Scheme
	recorder      events.EventRecorder
	clientFactory valkey.ClusterClientFactory
	platform      valkeyv1alpha1.Platform
	// skipNameValidation lets parallel envtest specs register more than one
	// manager-backed controller of this kind in a single process.
	skipNameValidation bool
	// rolePoll, when set, overrides the proactive-failover role poll so envtest
	// flips a target replica's role deterministically without a real engine or a
	// wall-clock wait (defaults to defaultRolePoll). Injected only in tests.
	rolePoll func(ctx context.Context, target *valkey.NodeState) valkey.Role
}

// roll context flows the live ClusterState into the phase-6 node stepping so the
// roll path can order by LIVE role and proactively fail over a live primary
// before rolling it (05 §6). It is scraped in phase 7 and threaded back into
// phase 6 on the NEXT reconcile via a fresh scrape inside reconcileValkeyNodes
// when a roll is actually pending (so a steady, no-roll pass never scrapes twice).

// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=valkey.percona.com,resources=valkeynodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.percona.com,resources=valkeynodes/status,verbs=get
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeybackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=valkey.percona.com,resources=perconavalkeyrestores,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile runs the cluster pipeline phases 0-15 (04 §2.1). Wave 2a stops at
// "a healthy sharded cluster forms"; the deferred phases are no-op seams.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("cluster", req.String())

	// Phase 0: fetch + defaults + crVersion gate + deletion branch.
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		// NotFound: the object is gone, GC reaps children via owner refs.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := cluster.CheckNSetDefaults(ctx, r.platform); err != nil {
		return ctrl.Result{}, r.fail(ctx, cluster, ReasonConfigMapError, err)
	}
	// crVersion gate (09 §2, §7, §8): validate spec.crVersion against the operator
	// version exactly once, here in phase 0, before any image/topology reconcile.
	// halt=true means the gate set a terminal condition (status already written by
	// the gate) and reconcile must stop without proceeding to the pipeline.
	if halt, err := r.reconcileVersionGate(ctx, cluster); halt || err != nil {
		return ctrl.Result{}, err
	}

	if !cluster.DeletionTimestamp.IsZero() {
		// Phase 0 deletion branch: ordered teardown (replicas-before-primaries,
		// shard by shard) then finalizer removal; owner-ref GC reaps the rest
		// (04 §6.1, GO-3.19).
		return r.handleDeletion(ctx, cluster)
	}

	// Register the teardown finalizers up front so a delete that arrives mid-life
	// always finds them present (04 §6). Persisted now but the pipeline continues
	// in the same pass (the in-memory object already carries them).
	if err := r.ensureFinalizers(ctx, cluster); err != nil {
		return ctrl.Result{}, r.fail(ctx, cluster, ReasonReconciling, err)
	}

	return r.reconcileCluster(ctx, log, cluster)
}

// reconcileCluster runs phases 1-15 once defaults/gate/deletion are resolved.
// Each phase is idempotent and returns early with a short requeue on progress
// (one effect per reconcile, 04 §2). Phases 1-6 (infra + node stepping) are in
// reconcileInfra; the bootstrap-join phases 10-12 are in bootstrapJoin; this
// keeps each function small while preserving the strict phase ordering.
func (r *Reconciler) reconcileCluster(
	ctx context.Context, log logr.Logger, cluster *valkeyv1alpha1.PerconaValkeyCluster,
) (ctrl.Result, error) {
	log.V(1).Info("reconciling cluster", "shards", cluster.Spec.Shards, "replicas", cluster.Spec.Replicas)
	cluster.Status.Host = clusterHost(cluster)

	// Version check (09 §3): resolve spec.upgradeOptions and let the version service
	// mutate spec.image (only-if-differs) BEFORE the node-stepping/smart-update step,
	// so a resolved engine pin is in place by the time phase 4 renders the pod
	// template and phase 6 rolls. Disabled (default) is a no-op. SEAM filled by the
	// version-check leg (GO-6.4/6.5/6.6/6.7); no-op stub until then.
	if err := r.reconcileVersionCheck(ctx, cluster); err != nil {
		return ctrl.Result{}, r.fail(ctx, cluster, ReasonVersionCheckFailed, err)
	}

	// Phases 1-6: infrastructure + one-at-a-time ValkeyNode stepping.
	nodes, done, res, err := r.reconcileInfra(ctx, cluster)
	if err != nil || done {
		return res, err
	}

	// Phase 7: scrape live ClusterState (needs ready nodes' podIPs).
	state := r.getValkeyClusterState(ctx, nodes)
	if state == nil {
		// No ready nodes yet — wait for podIPs.
		setCondition(cluster, CondProgressing, metav1.ConditionTrue, ReasonInitializing, "waiting for node podIPs")
		setCondition(cluster, CondReady, metav1.ConditionFalse, ReasonInitializing, "no ready nodes to scrape yet")
		if werr := r.writeStatus(ctx, cluster); werr != nil {
			return ctrl.Result{}, werr
		}
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	defer state.CloseClients()

	// Phases 8-9: recovery — promote orphaned replicas (TAKEOVER on quorum-loss +
	// persistence-off, BEFORE forget) then FORGET stale in-gossip-only nodes
	// (GO-3.17). TAKEOVER-before-FORGET keeps slots continuously owned (CR-5).
	if done, res, err := r.recover(ctx, cluster, state, nodes); err != nil || done {
		return res, err
	}

	// Phases 10-12: bootstrap join (MEET -> ADDSLOTSRANGE -> REPLICATE).
	done, res, err = r.bootstrapJoin(ctx, cluster, state, nodes)
	if err != nil || done {
		return res, err
	}

	// Phases 13-14: scale-in drain+delete then scale-out rebalance, one effect per
	// reconcile (GO-3.15 / GO-3.14). Both run only once the cluster is otherwise
	// formed (bootstrap join above returned no progress).
	if done, res, err := r.scale(ctx, cluster, state, nodes); err != nil || done {
		return res, err
	}

	// Phase 15: verify & mark Ready (or specific False + requeue fast).
	ready, err := r.verifyAndMarkReady(ctx, cluster, state)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ready {
		return ctrl.Result{RequeueAfter: requeueSteady}, nil
	}
	return ctrl.Result{RequeueAfter: requeueFast}, nil
}

// recover runs phases 8-9 (promoteOrphanedReplicas then forgetStaleNodes). Each
// returns acted=true to short-circuit the pipeline with a fast requeue so the
// next pass observes fresh state. The strict order (takeover BEFORE forget) keeps
// slots continuously owned during a quorum-loss recovery (04 §2.1 steps 8-9).
func (r *Reconciler) recover(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, ctrl.Result, error) {
	if acted, err := r.promoteOrphanedReplicas(ctx, cluster, state); err != nil {
		return true, ctrl.Result{}, r.degrade(ctx, cluster, ReasonQuorumLost, err)
	} else if acted {
		return r.progressRequeue(ctx, cluster, "promoting orphaned replicas")
	}
	if acted, err := r.forgetStaleNodes(ctx, cluster, state, nodes); err != nil {
		return true, ctrl.Result{}, r.fail(ctx, cluster, ReasonNodeForgetFailed, err)
	} else if acted {
		return r.progressRequeue(ctx, cluster, "forgetting stale nodes")
	}
	return false, ctrl.Result{}, nil
}

// scale runs phases 13-14 (scale-in then scale-out rebalance), one effect per
// reconcile. handleScaleIn drains+deletes excess shards; rebalanceSlots issues a
// single MIGRATESLOTS move toward balance (04 §2.1 steps 13-14).
func (r *Reconciler) scale(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, ctrl.Result, error) {
	if acted, err := r.handleScaleIn(ctx, cluster, state, nodes); err != nil {
		return true, ctrl.Result{}, r.fail(ctx, cluster, ReasonDrainFailed, err)
	} else if acted {
		return r.progressRequeue(ctx, cluster, "scaling in: draining/deleting excess shards")
	}
	if acted, err := r.rebalanceSlots(ctx, cluster, state); err != nil {
		return true, ctrl.Result{}, r.fail(ctx, cluster, ReasonRebalanceFailed, err)
	} else if acted {
		setCondition(cluster, CondProgressing, metav1.ConditionTrue, ReasonRebalancingSlots, "rebalancing slots across shards")
		if werr := r.writeStatus(ctx, cluster); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return true, ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	return false, ctrl.Result{}, nil
}

// degrade records a Warning event, sets Degraded=True + Ready=False with the
// reason, writes status and returns the error so controller-runtime backs off.
// Used for genuine impairment (quorum loss) distinct from a transient infra
// error (fail()).
func (r *Reconciler) degrade(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, reason string, err error) error {
	r.recorder.Eventf(cluster, nil, eventWarning, reason, reason, "%s", err.Error())
	setCondition(cluster, CondDegraded, metav1.ConditionTrue, reason, err.Error())
	setCondition(cluster, CondReady, metav1.ConditionFalse, reason, err.Error())
	if werr := r.writeStatus(ctx, cluster); werr != nil {
		logf.FromContext(ctx).Error(werr, "status writeback failed after cluster degrade")
	}
	return err
}

// gateAnnotation, when present and truthy on the cluster, blocks the step-6
// ValkeyNode roll. It is the interim M3 hook for the M4 backup-running roll gate
// / M6 restore pause gate (04 §4.2): until those controllers exist, an operator
// (or a test) can set this annotation to hold rolls; M4/M6 will replace the
// annotation read with a Watch-driven backup/restore-Running read. Recorded as
// an OPEN QUESTION (OQ-3.E pause/gate mechanics).
const gateAnnotation = "valkey.percona.com/gate-engine-roll"

// shouldGateEngineRoll reports whether the step-6 ValkeyNode roll must be blocked
// this pass — when a backup Job is Running (M4) or a restore requests pause (M6),
// pod churn could corrupt the snapshot stream so the roll is held (04 §4.2). M3
// has no backup/restore controllers, so it consults the interim gateAnnotation
// hook; the result is genuinely data-dependent so the seam is real, not a stub.
func (r *Reconciler) shouldGateEngineRoll(_ context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	// TODO(M4/M6): replace the annotation read with a Watch-driven check of
	// PerconaValkeyBackup Running / PerconaValkeyRestore pause-requested.
	v, ok := cluster.Annotations[gateAnnotation]
	return ok && (v == annEnabledValue || v == "1")
}

// reconcileInfra runs phases 1-6: headless Service, PDB, ACL Secret, ConfigMap
// (+ roll hash), list nodes, and the one-at-a-time ValkeyNode stepping. It
// returns the listed nodes plus done=true (with a result) when the caller should
// return early (an error or a node not yet converged).
func (r *Reconciler) reconcileInfra(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
) (*valkeyv1alpha1.ValkeyNodeList, bool, ctrl.Result, error) {
	// Phase 1: headless Service.
	if err := r.upsertService(ctx, cluster); err != nil {
		return nil, true, ctrl.Result{}, r.fail(ctx, cluster, ReasonServiceError, err)
	}
	// Phase 1b: external client access (spec.expose). Rendered right after the
	// headless Service: it provisions/prunes the operator-managed NodePort/
	// LoadBalancer Service(s) the cluster is reachable through, or reverts to
	// in-cluster-only access when expose is nil/ClusterIP (03 §2.12). Per-pod
	// selectors derive from the desired topology (not the live node list), so it
	// is safe to run before listing nodes.
	if err := r.reconcileExpose(ctx, cluster); err != nil {
		return nil, true, ctrl.Result{}, r.fail(ctx, cluster, ReasonExposeError, err)
	}
	// Phase 2: PodDisruptionBudget.
	if err := r.reconcilePodDisruptionBudget(ctx, cluster); err != nil {
		return nil, true, ctrl.Result{}, r.fail(ctx, cluster, ReasonPodDisruptionBudgetError, err)
	}
	// Phase 2b: NetworkPolicy — rendered alongside the Service/PDB infra (the
	// data-plane perimeter is part of bringing the cluster's infra up, 07 §7).
	// SEAM filled by the NetworkPolicy leg (M5 GO-5.9); no-op stub until then.
	if err := r.reconcileNetworkPolicy(ctx, cluster); err != nil {
		return nil, true, ctrl.Result{}, r.fail(ctx, cluster, ReasonNetworkPolicyError, err)
	}
	// Phase 3: ACL / system users.
	if err := r.reconcileUsersACL(ctx, cluster); err != nil {
		return nil, true, ctrl.Result{}, r.fail(ctx, cluster, ReasonUsersACLError, err)
	}
	// Phase 3b: TLS — provision/validate the cert Secret and propagate the tlsHash
	// BEFORE the ConfigMap+nodes so the cert exists before any pod mounts it (the
	// rendered tls-* directives reference naming.TLSMountPath, 07 §3.3). A
	// malformed/missing cert fails CLOSED with Degraded/TLSError. SEAM filled by
	// the TLS leg (M5 GO-5.6/5.8); no-op stub until then.
	if err := r.reconcileTLS(ctx, cluster); err != nil {
		return nil, true, ctrl.Result{}, r.degrade(ctx, cluster, ReasonTLSError, err)
	}
	// Phase 4: ConfigMap + roll hash.
	configHash, err := r.upsertConfigMap(ctx, cluster)
	if err != nil {
		return nil, true, ctrl.Result{}, r.fail(ctx, cluster, ReasonConfigMapError, err)
	}
	// Phase 5: list nodes.
	nodes, err := r.listClusterNodes(ctx, cluster)
	if err != nil {
		return nil, true, ctrl.Result{}, r.fail(ctx, cluster, ReasonValkeyNodeListError, err)
	}
	// Phase 6: create/update ValkeyNodes one-at-a-time, replicas-before-primary.
	// (Wave 2a owns the create + stepping scaffold; the roll/proactive-failover
	// integration is GO-3.16, Wave 2b.)
	requeue, err := r.reconcileValkeyNodes(ctx, cluster, nodes, configHash)
	if err != nil {
		return nil, true, ctrl.Result{}, r.fail(ctx, cluster, ReasonUpdatingNodes, err)
	}
	if requeue {
		setCondition(cluster, CondProgressing, metav1.ConditionTrue, progressingReason(cluster),
			"creating/updating ValkeyNodes one-at-a-time")
		setCondition(cluster, CondReady, metav1.ConditionFalse, ReasonUpdatingNodes, "ValkeyNodes not yet converged")
		if werr := r.writeStatus(ctx, cluster); werr != nil {
			return nil, true, ctrl.Result{}, werr
		}
		return nil, true, ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	return nodes, false, ctrl.Result{}, nil
}

// bootstrapJoin runs the strict bootstrap-join phases 10-12 (MEET ->
// ADDSLOTSRANGE -> REPLICATE), each returning early with a fast requeue on
// progress so the next pass observes fresh gossip/slot state. It returns
// done=true (with a result) when the caller should return early.
func (r *Reconciler) bootstrapJoin(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	state *valkey.ClusterState, nodes *valkeyv1alpha1.ValkeyNodeList,
) (bool, ctrl.Result, error) {
	// Phase 10: MEET isolated pending nodes (bidirectional, epoch-bumped).
	met, err := r.meetIsolatedNodes(ctx, cluster, state)
	if err != nil {
		return true, ctrl.Result{}, r.fail(ctx, cluster, ReasonClusterMeet, err)
	}
	if met > 0 {
		return r.progressRequeue(ctx, cluster, "introducing nodes via CLUSTER MEET")
	}
	// Phase 11: assign slots to pending primaries (ADDSLOTSRANGE, even split).
	assigned, err := r.assignSlotsToPendingPrimaries(ctx, cluster, state, nodes)
	if err != nil {
		return true, ctrl.Result{}, r.fail(ctx, cluster, ReasonSlotsUnassigned, err)
	}
	if assigned > 0 {
		return r.progressRequeue(ctx, cluster, "assigning slots to primaries")
	}
	// Phase 12: replicate pending replicas (CLUSTER REPLICATE).
	replicated, err := r.replicatePendingReplicas(ctx, cluster, state, nodes)
	if err != nil {
		return true, ctrl.Result{}, r.fail(ctx, cluster, ReasonMissingReplicas, err)
	}
	if replicated > 0 {
		return r.progressRequeue(ctx, cluster, "attaching replicas")
	}
	return false, ctrl.Result{}, nil
}

// progressRequeue marks Progressing=True with the message, writes status, and
// returns done=true + a fast requeue — the common "made progress, observe fresh
// state next pass" tail of the bootstrap phases.
func (r *Reconciler) progressRequeue(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, msg string,
) (bool, ctrl.Result, error) {
	setCondition(cluster, CondProgressing, metav1.ConditionTrue, ReasonAddingNodes, msg)
	if err := r.writeStatus(ctx, cluster); err != nil {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{RequeueAfter: requeueFast}, nil
}

// clusterHost is the client connection endpoint: the headless Service DNS.
func clusterHost(cluster *valkeyv1alpha1.PerconaValkeyCluster) string {
	return naming.HeadlessServiceName(cluster.Name) + "." + cluster.Namespace + ".svc"
}

// progressingReason picks Initializing before the cluster has ever formed, else
// Reconciling (04 §7 initializing-vs-reconciling distinction).
func progressingReason(cluster *valkeyv1alpha1.PerconaValkeyCluster) string {
	if conditionTrue(cluster, CondClusterFormed) {
		return ReasonReconciling
	}
	return ReasonInitializing
}

// reconcileVersionGate is the SINGLE phase-0 crVersion gate (09 §2, §7, §8). It
// validates spec.crVersion against the running operator's major.minor and decides
// whether the reconcile may proceed. It is wired ONCE in Reconcile (the contended
// dispatch the M6 FOUNDATION owns) so the version-acceptance policy has exactly
// one entry point.
//
// The gate applies four checks in order, all routed through this one entry point
// so the version-acceptance policy stays single-sourced:
//
//  1. NEWER than the operator's major.minor — an older operator must not reconcile
//     a CR authored for a newer API (04 §2.1 step0): Ready=False/UnsupportedCRVersion.
//  2. Runtime crVersion DOWNGRADE (GO-6.3) — a decrease vs status.lastObservedCrVersion
//     is refused; the prior contract is kept in force (spec.crVersion reverted in
//     memory) and Degraded/CrVersionDowngradeRejected is set (09 §7). crVersion is
//     deliberately not hard-immutable (03 §4.3), so monotonicity is enforced here.
//  3. Below-FLOOR crVersion (GO-6.2) — accepted = own minor + the immediately-
//     preceding released minor, computed via version.AcceptedCrVersions (09 §8). A
//     value more than one minor behind is halted with Degraded/CrVersionTooOld.
//  4. Otherwise the contract is accepted and the pipeline proceeds.
//
// An empty crVersion never reaches here (CheckNSetDefaults stamps it first). The
// successful tail (verifyAndMarkReady) mirrors the accepted crVersion onto
// status.lastObservedCrVersion, the anchor check 2 compares against next pass.
// version.CompareMajorMinor / CompareVersion are the comparators used here.
func (r *Reconciler) reconcileVersionGate(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
) (halt bool, err error) {
	// Newer-than-operator: an older operator must not reconcile a CR authored for a
	// newer API (04 §2.1 step0). Ready=False/UnsupportedCRVersion, halt.
	if newer, msg := crVersionNewerThanOperator(cluster); newer {
		setCondition(cluster, CondReady, metav1.ConditionFalse, ReasonUnsupportedCRVersion, msg)
		return true, r.writeStatus(ctx, cluster)
	}

	// GO-6.3 runtime monotonicity: reject a crVersion DECREASE vs the last accepted
	// value (mirrored on status). crVersion is deliberately not hard-immutable
	// (03 §4.3) so the forward opt-in bump is allowed; a decrease is refused HERE at
	// runtime — the prior contract is kept in force (spec.crVersion reverted in
	// memory so the rest of the pass reconciles the OLD contract), a Warning is
	// emitted, and Degraded/CrVersionDowngradeRejected is set (09 §7).
	if last := cluster.Status.LastObservedCrVersion; last != "" &&
		version.CompareMajorMinor(cluster.Spec.CrVersion, last) < 0 {
		rejected := cluster.Spec.CrVersion
		cluster.Spec.CrVersion = last // keep the prior contract in force
		msg := "refusing crVersion downgrade " + rejected + " -> " + last + " (crVersion is monotonic)"
		r.recorder.Eventf(cluster, nil, eventWarning, ReasonCrVersionDowngradeRejected,
			ReasonCrVersionDowngradeRejected, "%s", msg)
		setCondition(cluster, CondDegraded, metav1.ConditionTrue, ReasonCrVersionDowngradeRejected, msg)
		setCondition(cluster, CondReady, metav1.ConditionFalse, ReasonCrVersionDowngradeRejected, msg)
		return true, r.writeStatus(ctx, cluster)
	}

	// GO-6.2 floor: accept only the operator's own minor + the immediately-preceding
	// released minor (09 §8, computed not hardcoded). A crVersion below that floor
	// (more than one minor behind) is halted with Degraded/CrVersionTooOld so the
	// user steps up one minor at a time rather than jumping (09 §7). Newer values
	// were already handled above, so a non-accepted value here is necessarily too old.
	accepted := version.AcceptedCrVersions(version.MajorMinor())
	if !slices.Contains(accepted, cluster.Spec.CrVersion) {
		msg := "spec.crVersion " + cluster.Spec.CrVersion + " is more than one minor behind operator " +
			version.MajorMinor() + " (accepted: " + strings.Join(accepted, ", ") +
			"); step crVersion up one minor at a time"
		r.recorder.Eventf(cluster, nil, eventWarning, ReasonCrVersionTooOld,
			ReasonCrVersionTooOld, "%s", msg)
		setCondition(cluster, CondDegraded, metav1.ConditionTrue, ReasonCrVersionTooOld, msg)
		setCondition(cluster, CondReady, metav1.ConditionFalse, ReasonCrVersionTooOld, msg)
		return true, r.writeStatus(ctx, cluster)
	}

	return false, nil
}

// crVersionNewerThanOperator reports whether spec.crVersion is newer than the
// running operator's major.minor (an older operator must not reconcile a CR
// authored for a newer API, 04 §2.1 step0). An empty/equal/older crVersion is
// fine. version.CompareVersion returns sign(operator - crVersion); a negative
// value means the operator is older than the CR's crVersion. Kept as a pure
// predicate (no receiver) so reconcileVersionGate and unit tests share one source
// of truth for the newer-than-operator rule.
func crVersionNewerThanOperator(cluster *valkeyv1alpha1.PerconaValkeyCluster) (bool, string) {
	if cluster.Spec.CrVersion == "" {
		return false, ""
	}
	if version.CompareVersion(cluster.Spec.CrVersion) < 0 {
		return true, "spec.crVersion " + cluster.Spec.CrVersion + " is newer than operator " + version.MajorMinor()
	}
	return false, ""
}

// fail records a Warning event, sets Ready=False with the reason, writes status
// and returns the error so controller-runtime backs off.
func (r *Reconciler) fail(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, reason string, err error) error {
	r.recorder.Eventf(cluster, nil, eventWarning, reason, reason, "%s", err.Error())
	setCondition(cluster, CondReady, metav1.ConditionFalse, reason, err.Error())
	if werr := r.writeStatus(ctx, cluster); werr != nil {
		logf.FromContext(ctx).Error(werr, "status writeback failed after cluster error")
	}
	return err
}
