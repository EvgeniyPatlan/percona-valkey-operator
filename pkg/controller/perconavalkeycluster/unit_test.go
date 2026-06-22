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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// newCluster builds a bare cluster CR for unit tests.
func newCluster() *valkeyv1alpha1.PerconaValkeyCluster {
	c := &valkeyv1alpha1.PerconaValkeyCluster{}
	c.Name, c.Namespace = "u", "default"
	c.Generation = 1
	return c
}

func TestDeriveStateTruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		set  func(c *valkeyv1alpha1.PerconaValkeyCluster)
		want valkeyv1alpha1.ClusterState
	}{
		{"degraded wins over ready", func(c *valkeyv1alpha1.PerconaValkeyCluster) {
			setCondition(c, CondReady, metav1.ConditionTrue, "", "")
			setCondition(c, CondDegraded, metav1.ConditionTrue, "", "")
		}, valkeyv1alpha1.StateDegraded},
		{"ready", func(c *valkeyv1alpha1.PerconaValkeyCluster) {
			setCondition(c, CondReady, metav1.ConditionTrue, "", "")
		}, valkeyv1alpha1.StateReady},
		{"progressing pre-form is initializing", func(c *valkeyv1alpha1.PerconaValkeyCluster) {
			setCondition(c, CondProgressing, metav1.ConditionTrue, "", "")
		}, valkeyv1alpha1.StateInitializing},
		{"progressing post-form is reconciling", func(c *valkeyv1alpha1.PerconaValkeyCluster) {
			setCondition(c, CondClusterFormed, metav1.ConditionTrue, "", "")
			setCondition(c, CondProgressing, metav1.ConditionTrue, "", "")
		}, valkeyv1alpha1.StateReconciling},
		{"no conditions is failed", func(_ *valkeyv1alpha1.PerconaValkeyCluster) {}, valkeyv1alpha1.StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster()
			tc.set(c)
			if got := deriveState(c); got != tc.want {
				t.Fatalf("deriveState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeriveStateDeletingIsReconciling(t *testing.T) {
	t.Parallel()
	c := newCluster()
	now := metav1.Now()
	c.DeletionTimestamp = &now
	setCondition(c, CondReady, metav1.ConditionTrue, "", "")
	// A deleting cluster never reports ready.
	if got := deriveState(c); got == valkeyv1alpha1.StateReady {
		t.Fatalf("deleting cluster derived ready: %q", got)
	}
}

func TestNodeConverged(t *testing.T) {
	t.Parallel()
	c := newCluster()
	c.Spec.Image = "img:1"
	node := &valkeyv1alpha1.ValkeyNode{}
	node.Generation = 2
	node.Spec.ServerConfigHash = "h1"
	node.Spec.Image = "img:1"
	node.Status.Ready = true
	node.Status.ObservedGeneration = 2

	if !nodeConverged(node, c, "h1") {
		t.Fatal("expected converged")
	}
	// Hash mismatch.
	if nodeConverged(node, c, "h2") {
		t.Fatal("hash mismatch should not be converged")
	}
	// Image mismatch.
	node2 := *node
	node2.Spec.Image = "img:2"
	if nodeConverged(&node2, c, "h1") {
		t.Fatal("image mismatch should not be converged")
	}
	// Not ready.
	node3 := *node
	node3.Status.Ready = false
	if nodeConverged(&node3, c, "h1") {
		t.Fatal("not-ready should not be converged")
	}
	// Generation lag.
	node4 := *node
	node4.Status.ObservedGeneration = 1
	if nodeConverged(&node4, c, "h1") {
		t.Fatal("generation lag should not be converged")
	}
}

func TestDesiredNodesReplicasBeforePrimary(t *testing.T) {
	t.Parallel()
	c := newCluster()
	c.Spec.Shards = 2
	c.Spec.Replicas = 1
	got := desiredNodes(c)
	// Per shard: replica (node 1) before primary (node 0).
	want := []nodeKey{{0, 1}, {0, 0}, {1, 1}, {1, 0}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCrVersionNewerThanOperator(t *testing.T) {
	t.Parallel()
	c := newCluster()
	c.Spec.CrVersion = "999.0"
	if newer, _ := crVersionNewerThanOperator(c); !newer {
		t.Fatal("999.0 should be newer than the operator")
	}
	c.Spec.CrVersion = ""
	if newer, _ := crVersionNewerThanOperator(c); newer {
		t.Fatal("empty crVersion should not be considered newer")
	}
	c.Spec.CrVersion = "0.0"
	if newer, _ := crVersionNewerThanOperator(c); newer {
		t.Fatal("0.0 should not be newer than the operator")
	}
}

func TestProgressingReason(t *testing.T) {
	t.Parallel()
	c := newCluster()
	if progressingReason(c) != ReasonInitializing {
		t.Fatal("pre-form should be Initializing")
	}
	setCondition(c, CondClusterFormed, metav1.ConditionTrue, "", "")
	if progressingReason(c) != ReasonReconciling {
		t.Fatal("post-form should be Reconciling")
	}
}

func TestHostFromAddrAndSlotsHelpers(t *testing.T) {
	t.Parallel()
	if hostFromAddr("10.0.0.1:6379") != "10.0.0.1" {
		t.Fatal("hostFromAddr strip failed")
	}
	if hostFromAddr("nocolon") != "nocolon" {
		t.Fatal("hostFromAddr passthrough failed")
	}
	if boolToStatus(true) != metav1.ConditionTrue || boolToStatus(false) != metav1.ConditionFalse {
		t.Fatal("boolToStatus")
	}
	if slotsReason(true) != ReasonClusterHealthy || slotsReason(false) != ReasonSlotsUnassigned {
		t.Fatal("slotsReason")
	}
	if slotsMessage(true) == slotsMessage(false) {
		t.Fatal("slotsMessage should differ")
	}
}

func TestClusterHost(t *testing.T) {
	t.Parallel()
	c := newCluster()
	if got := clusterHost(c); got != "valkey-u.default.svc" {
		t.Fatalf("clusterHost = %q", got)
	}
}

func TestMapBackupRestoreToCluster(t *testing.T) {
	t.Parallel()
	b := &valkeyv1alpha1.PerconaValkeyBackup{}
	b.Namespace = "ns"
	b.Spec.ClusterName = "mycluster"
	reqs := mapBackupToCluster(context.TODO(), b)
	if len(reqs) != 1 || reqs[0].Name != "mycluster" || reqs[0].Namespace != "ns" {
		t.Fatalf("mapBackupToCluster = %+v", reqs)
	}
	// Empty clusterName -> no request.
	b.Spec.ClusterName = ""
	if reqs := mapBackupToCluster(context.TODO(), b); reqs != nil {
		t.Fatalf("empty clusterName should map to nil, got %+v", reqs)
	}

	rst := &valkeyv1alpha1.PerconaValkeyRestore{}
	rst.Namespace = "ns"
	rst.Spec.ClusterName = "mycluster"
	reqs = mapRestoreToCluster(context.TODO(), rst)
	if len(reqs) != 1 || reqs[0].Name != "mycluster" {
		t.Fatalf("mapRestoreToCluster = %+v", reqs)
	}
	rst.Spec.ClusterName = ""
	if reqs := mapRestoreToCluster(context.TODO(), rst); reqs != nil {
		t.Fatalf("empty restore clusterName should map to nil, got %+v", reqs)
	}
}

func TestConfigInputFromSpec(t *testing.T) {
	t.Parallel()
	c := newCluster()
	in := configInput(c)
	if in.Persistence || in.TLS || !in.ACL {
		t.Fatalf("minimal spec configInput = %+v", in)
	}
}
