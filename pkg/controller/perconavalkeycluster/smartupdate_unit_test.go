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
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeybackup"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// fullCoverageState builds a single-shard ClusterState that owns all 16384 slots
// with one primary + one synced replica (link up), the all-healthy baseline the
// gate unit tests perturb one field at a time.
func fullCoverageState() *valkey.ClusterState {
	primary := &valkey.NodeState{
		ID: "p0", ShardID: "s0", Addr: "10.0.0.1:6379", Role: valkey.RolePrimary,
		Slots: []valkey.SlotRange{{Start: 0, End: valkey.TotalSlots - 1}},
	}
	replica := &valkey.NodeState{
		ID: "r0", ShardID: "s0", Addr: "10.0.0.2:6379", Role: valkey.RoleReplica,
		PrimaryID: "p0", LinkUp: true, Offset: 100,
	}
	return valkey.NewClusterState([]*valkey.NodeState{primary, replica})
}

// readyCluster returns a cluster CR whose conditions report Ready=True.
func readyCluster(image string) *valkeyv1alpha1.PerconaValkeyCluster {
	c := &valkeyv1alpha1.PerconaValkeyCluster{}
	c.Name, c.Namespace, c.Generation = "u", "default", 1
	c.Spec.Image = image
	setCondition(c, CondReady, metav1.ConditionTrue, ReasonClusterHealthy, "healthy")
	return c
}

// newGateReconciler wires a Reconciler with a fake client (seeded with objs) and a
// buffered recorder for the gate/downgrade unit tests.
func newGateReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	s := aclTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Reconciler{Client: c, scheme: s, recorder: events.NewFakeRecorder(200)}
}

func TestParseEngineVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		image               string
		major, minor, patch int
		known               bool
	}{
		{"percona/percona-valkey:9.0.1-2", 9, 0, 1, true},
		{"percona/percona-valkey:9.0", 9, 0, 0, true},
		{"percona/percona-valkey:8.0.3", 8, 0, 3, true},
		{"percona/percona-valkey:7.2", 7, 2, 0, true},
		{"valkey:9", 9, 0, 0, true},
		{"percona/percona-valkey:latest", 0, 0, 0, false},
		{"percona/percona-valkey", 0, 0, 0, false},            // repo only -> tag "percona-valkey", not numeric
		{"9.0.2", 9, 0, 2, true},                              // bare tag, no repo
		{"registry:5000/percona-valkey:9.2.0", 9, 2, 0, true}, // registry port must not confuse the tag split
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			got := parseEngineVersion(tc.image)
			if got.known != tc.known {
				t.Fatalf("known = %v, want %v (got %+v)", got.known, tc.known, got)
			}
			if !tc.known {
				return
			}
			if got.major != tc.major || got.minor != tc.minor || got.patch != tc.patch {
				t.Fatalf("parseEngineVersion(%q) = %d.%d.%d, want %d.%d.%d",
					tc.image, got.major, got.minor, got.patch, tc.major, tc.minor, tc.patch)
			}
		})
	}
}

func TestClassifyEngineChange(t *testing.T) {
	t.Parallel()
	const repo = "percona/percona-valkey:"
	cases := []struct {
		name          string
		current, next string
		want          engineChangeKind
	}{
		{"feature-line downgrade 9.0->8.0", repo + "9.0.2", repo + "8.0.1", engineChangeFeatureLineDowngrade},
		{"major downgrade 9.0->7.2", repo + "9.0.0", repo + "7.2.4", engineChangeFeatureLineDowngrade},
		{"patch within line 9.0.1->9.0.2", repo + "9.0.1", repo + "9.0.2", engineChangePatch},
		{"patch downgrade within line 9.0.2->9.0.1 is same-line", repo + "9.0.2", repo + "9.0.1", engineChangePatch},
		{"one-step minor forward 8.0->9.0 is routine", repo + "8.0.3", repo + "9.0.0", engineChangeMinorForward},
		{"adjacent same-major step 9.0->9.1 is routine", repo + "9.0.0", repo + "9.1.0", engineChangeMinorForward},
		{"same-major span 9.0->9.2 is a jump", repo + "9.0.0", repo + "9.2.0", engineChangeMultiMinorJump},
		{"multi-minor jump 8.0->9.2 (next major, minor 2)", repo + "8.0.0", repo + "9.2.0", engineChangeMultiMinorJump},
		{"major skip 7.2->9.0 is a jump", repo + "7.2.0", repo + "9.0.0", engineChangeMultiMinorJump},
		{"unparseable current -> none", repo + "latest", repo + "9.0.0", engineChangeNone},
		{"unparseable next -> none", repo + "9.0.0", repo + "latest", engineChangeNone},
		{"identical -> patch (same line)", repo + "9.0.1", repo + "9.0.1", engineChangePatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEngineChange(tc.current, tc.next); got != tc.want {
				t.Fatalf("classifyEngineChange(%q,%q) = %d, want %d", tc.current, tc.next, got, tc.want)
			}
		})
	}
}

func TestSmartUpdateAllowedHealthGates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		mutate     func(c *valkeyv1alpha1.PerconaValkeyCluster, s *valkey.ClusterState) *valkey.ClusterState
		wantOK     bool
		wantReason string
	}{
		{
			name:   "all healthy -> allowed",
			mutate: func(_ *valkeyv1alpha1.PerconaValkeyCluster, s *valkey.ClusterState) *valkey.ClusterState { return s },
			wantOK: true,
		},
		{
			name: "degraded -> gated (genuine impairment backstop)",
			mutate: func(c *valkeyv1alpha1.PerconaValkeyCluster, s *valkey.ClusterState) *valkey.ClusterState {
				setCondition(c, CondDegraded, metav1.ConditionTrue, ReasonQuorumLost, "quorum lost")
				return s
			},
			wantOK: false, wantReason: ReasonUpgradeGatedNotReady,
		},
		{
			name: "transient Progressing alone does NOT gate (no self-deadlock)",
			mutate: func(c *valkeyv1alpha1.PerconaValkeyCluster, s *valkey.ClusterState) *valkey.ClusterState {
				// A one-at-a-time roll transiently flips Progressing/Ready=False; with
				// the live state healthy the engine roll must still be allowed.
				setCondition(c, CondReady, metav1.ConditionFalse, ReasonReconciling, "rolling")
				setCondition(c, CondProgressing, metav1.ConditionTrue, ReasonReconciling, "rolling")
				return s
			},
			wantOK: true,
		},
		{
			name: "nil state -> gated not-ready",
			mutate: func(_ *valkeyv1alpha1.PerconaValkeyCluster, _ *valkey.ClusterState) *valkey.ClusterState {
				return nil
			},
			wantOK: false, wantReason: ReasonUpgradeGatedNotReady,
		},
		{
			name: "slots incomplete -> gated",
			mutate: func(_ *valkeyv1alpha1.PerconaValkeyCluster, _ *valkey.ClusterState) *valkey.ClusterState {
				// One primary owning only half the slots: coverage incomplete.
				p := &valkey.NodeState{ID: "p0", ShardID: "s0", Addr: "10.0.0.1:6379", Role: valkey.RolePrimary,
					Slots: []valkey.SlotRange{{Start: 0, End: 8191}}}
				return valkey.NewClusterState([]*valkey.NodeState{p})
			},
			wantOK: false, wantReason: ReasonUpgradeGatedSlotsIncomplete,
		},
		{
			name: "replica link down -> gated",
			mutate: func(_ *valkeyv1alpha1.PerconaValkeyCluster, _ *valkey.ClusterState) *valkey.ClusterState {
				p := &valkey.NodeState{ID: "p0", ShardID: "s0", Addr: "10.0.0.1:6379", Role: valkey.RolePrimary,
					Slots: []valkey.SlotRange{{Start: 0, End: valkey.TotalSlots - 1}}}
				rep := &valkey.NodeState{ID: "r0", ShardID: "s0", Addr: "10.0.0.2:6379", Role: valkey.RoleReplica,
					PrimaryID: "p0", LinkUp: false}
				return valkey.NewClusterState([]*valkey.NodeState{p, rep})
			},
			wantOK: false, wantReason: ReasonUpgradeGatedReplicasUnsynced,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No Lease seeded -> backupRunning is false (fail-open NotFound).
			r := newGateReconciler(t)
			c := readyCluster("percona/percona-valkey:9.0")
			state := tc.mutate(c, fullCoverageState())
			ok, reason := r.smartUpdateAllowed(context.Background(), c, state)
			if ok != tc.wantOK || reason != tc.wantReason {
				t.Fatalf("smartUpdateAllowed = (%v,%q), want (%v,%q)", ok, reason, tc.wantOK, tc.wantReason)
			}
		})
	}
}

func TestSmartUpdateGatedWhileBackupRunning(t *testing.T) {
	t.Parallel()
	c := readyCluster("percona/percona-valkey:9.0")
	// Seed a fresh Lease held by a backup so IsBackupRunning reports busy.
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: perconavalkeybackup.LeaseName(c.Name), Namespace: c.Namespace},
	}
	now := metav1.NewMicroTime(time.Now())
	holder := "backup/" + c.Namespace + "/bk"
	dur := int32(30)
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = &dur
	lease.Spec.RenewTime = &now
	r := newGateReconciler(t, lease)

	if r.backupRunning(context.Background(), c) != true {
		t.Fatal("expected backupRunning=true while a fresh Lease is held")
	}
	ok, reason := r.smartUpdateAllowed(context.Background(), c, fullCoverageState())
	if ok || reason != ReasonUpgradeGatedBackupRunning {
		t.Fatalf("smartUpdateAllowed under backup = (%v,%q), want (false,%q)", ok, reason, ReasonUpgradeGatedBackupRunning)
	}
}

func TestBackupRunningFailsOpenOnMissingLease(t *testing.T) {
	t.Parallel()
	c := readyCluster("percona/percona-valkey:9.0")
	r := newGateReconciler(t) // no Lease seeded.
	if r.backupRunning(context.Background(), c) {
		t.Fatal("missing Lease must fail open to no-backup-running")
	}
}

func TestApplyEngineDowngradePolicyRefusesFeatureLineDowngrade(t *testing.T) {
	t.Parallel()
	c := readyCluster("percona/percona-valkey:8.0.1") // desired = older line.
	r := newGateReconciler(t)

	blocked, reason := r.applyEngineDowngradePolicy(c, "percona/percona-valkey:9.0.2")
	if !blocked || reason != ReasonUnsupportedDowngrade {
		t.Fatalf("applyEngineDowngradePolicy = (%v,%q), want (true,%q)", blocked, reason, ReasonUnsupportedDowngrade)
	}
	if !conditionTrue(c, CondDegraded) {
		t.Fatal("a refused downgrade must set Degraded=True")
	}
}

func TestApplyEngineDowngradePolicyAllowsForward(t *testing.T) {
	t.Parallel()
	// Forward jump 8.0 -> 9.0: allowed (not blocked), no Degraded.
	c := readyCluster("percona/percona-valkey:9.0.0")
	r := newGateReconciler(t)
	if blocked, _ := r.applyEngineDowngradePolicy(c, "percona/percona-valkey:8.0.1"); blocked {
		t.Fatal("a forward engine jump must not block")
	}
	if conditionTrue(c, CondDegraded) {
		t.Fatal("a forward jump must not set Degraded")
	}
}

func TestPendingEngineChangeDetectsConfigOnlyVsEngineRoll(t *testing.T) {
	t.Parallel()
	// Config-only roll: every node already on spec.image -> changing=false.
	c := readyCluster("percona/percona-valkey:9.0.0")
	cfgNode := &valkeyv1alpha1.ValkeyNode{}
	cfgNode.Name, cfgNode.Namespace = "u-0-0", c.Namespace
	cfgNode.Labels = map[string]string{"valkey.percona.com/cluster": c.Name}
	cfgNode.Spec.Image = "percona/percona-valkey:9.0.0"
	r := newGateReconciler(t, cfgNode)
	_, changing, err := r.pendingEngineChange(context.Background(), c)
	if err != nil || changing {
		t.Fatalf("config-only roll: pendingEngineChange changing=%v err=%v, want (false,nil)", changing, err)
	}

	// Engine roll: a node still on the old image -> changing=true with that image.
	c2 := readyCluster("percona/percona-valkey:9.0.1")
	engNode := &valkeyv1alpha1.ValkeyNode{}
	engNode.Name, engNode.Namespace = "v-0-0", c2.Namespace
	engNode.Labels = map[string]string{"valkey.percona.com/cluster": c2.Name}
	engNode.Spec.Image = "percona/percona-valkey:9.0.0"
	r2 := newGateReconciler(t, engNode)
	current, changing, err := r2.pendingEngineChange(context.Background(), c2)
	if err != nil || !changing || current != "percona/percona-valkey:9.0.0" {
		t.Fatalf("engine roll: pendingEngineChange = (%q,%v,%v), want (old-image,true,nil)", current, changing, err)
	}
}

func TestReconcileSmartUpdatePermitsHealthyForwardRoll(t *testing.T) {
	t.Parallel()
	c := readyCluster("percona/percona-valkey:9.0.0")
	node := &valkeyv1alpha1.ValkeyNode{}
	node.Name, node.Namespace = "u-0-0", c.Namespace
	node.Labels = map[string]string{"valkey.percona.com/cluster": c.Name}
	node.Spec.Image = "percona/percona-valkey:8.0.1" // pending forward roll.
	r := newGateReconciler(t, node)

	allowed, reason := r.reconcileSmartUpdate(context.Background(), c, fullCoverageState())
	if !allowed || reason != "" {
		t.Fatalf("reconcileSmartUpdate (healthy forward roll) = (%v,%q), want (true,\"\")", allowed, reason)
	}
}

func TestReconcileSmartUpdateDowngradePrecedesGate(t *testing.T) {
	t.Parallel()
	// Cluster is NOT ready AND the change is a downgrade: the downgrade refusal
	// must win (it is evaluated before the health gate) so the surfaced reason is
	// UnsupportedDowngrade, not a gate reason.
	c := readyCluster("percona/percona-valkey:8.0.1")
	setCondition(c, CondReady, metav1.ConditionFalse, ReasonReconciling, "progressing")
	node := &valkeyv1alpha1.ValkeyNode{}
	node.Name, node.Namespace = "u-0-0", c.Namespace
	node.Labels = map[string]string{"valkey.percona.com/cluster": c.Name}
	node.Spec.Image = "percona/percona-valkey:9.0.2"
	r := newGateReconciler(t, node)

	allowed, reason := r.reconcileSmartUpdate(context.Background(), c, fullCoverageState())
	if allowed || reason != ReasonUnsupportedDowngrade {
		t.Fatalf("reconcileSmartUpdate = (%v,%q), want (false,%q)", allowed, reason, ReasonUnsupportedDowngrade)
	}
}

func TestReconcileSmartUpdateConfigOnlyRollIsUngated(t *testing.T) {
	t.Parallel()
	// Every node already on spec.image -> a config-only roll. The smart-update
	// gate must NOT engage even while a backup Lease is held: only an engine-image
	// roll is gated (two-axis separation). Seed a held Lease to prove it.
	c := readyCluster("percona/percona-valkey:9.0.0")
	node := &valkeyv1alpha1.ValkeyNode{}
	node.Name, node.Namespace = "u-0-0", c.Namespace
	node.Labels = map[string]string{"valkey.percona.com/cluster": c.Name}
	node.Spec.Image = "percona/percona-valkey:9.0.0"
	now := metav1.NewMicroTime(time.Now())
	holder := "backup/" + c.Namespace + "/bk"
	dur := int32(30)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: perconavalkeybackup.LeaseName(c.Name), Namespace: c.Namespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &dur, RenewTime: &now},
	}
	r := newGateReconciler(t, node, lease)

	allowed, reason := r.reconcileSmartUpdate(context.Background(), c, fullCoverageState())
	if !allowed || reason != "" {
		t.Fatalf("config-only roll must be ungated even under a backup, got (%v,%q)", allowed, reason)
	}
}

func TestApplyEngineDowngradePolicyEmitsJumpAndFloorEvents(t *testing.T) {
	t.Parallel()
	// 8.0 -> 9.2 is a multi-minor jump (warned, not blocked).
	c := readyCluster("percona/percona-valkey:9.2.0")
	rec := events.NewFakeRecorder(20)
	r := &Reconciler{Client: newGateReconciler(t).Client, scheme: aclTestScheme(t), recorder: rec}
	if blocked, _ := r.applyEngineDowngradePolicy(c, "percona/percona-valkey:8.0.0"); blocked {
		t.Fatal("a multi-minor jump must not block")
	}
	if !drainContains(rec, ReasonEngineJumpWarning) {
		t.Fatal("expected an EngineJumpWarning event for the multi-minor jump")
	}

	// Resolving a sub-9.0 engine emits the MigrateSlotsUnsupported advisory.
	c2 := readyCluster("percona/percona-valkey:8.0.1")
	rec2 := events.NewFakeRecorder(20)
	r2 := &Reconciler{Client: newGateReconciler(t).Client, scheme: aclTestScheme(t), recorder: rec2}
	if blocked, _ := r2.applyEngineDowngradePolicy(c2, "percona/percona-valkey:8.0.0"); blocked {
		t.Fatal("a same-line sub-9.0 patch move must not block")
	}
	if !drainContains(rec2, ReasonMigrateSlotsUnsupported) {
		t.Fatal("expected a MigrateSlotsUnsupported advisory for a sub-9.0 engine")
	}
}

// drainContains reports whether any buffered event on the FakeRecorder contains
// the given substring (non-blocking drain).
func drainContains(rec *events.FakeRecorder, substr string) bool {
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, substr) {
				return true
			}
		default:
			return false
		}
	}
}
