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

package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	opmetrics "valkey.percona.com/percona-valkey-operator/pkg/metrics"
)

const (
	testNamespace = "ns1"
	testCluster   = "vk1"
)

func TestCollectorsRegisteredOnSharedRegistry(t *testing.T) {
	// init() must have registered every collector on controller-runtime's shared
	// registry; re-registering the same collectors must therefore error (already
	// registered), proving they live on the served /metrics registry.
	for _, c := range opmetrics.CollectorsForTest() {
		if err := crmetrics.Registry.Register(c); err == nil {
			t.Fatalf("collector %T was not already registered on the shared registry", c)
		}
	}
}

func TestObserveClusterSetsGauges(t *testing.T) {
	opmetrics.ResetForTest()

	opmetrics.ObserveCluster(testNamespace, testCluster, true, "Ready", 3, 3)

	if got := testutil.ToFloat64(opmetrics.ClusterReadyVec.WithLabelValues(testNamespace, testCluster)); got != 1 {
		t.Errorf("cluster_ready = %v, want 1", got)
	}
	if got := testutil.ToFloat64(opmetrics.ClusterShardsDesiredVec.WithLabelValues(testNamespace, testCluster)); got != 3 {
		t.Errorf("cluster_shards_desired = %v, want 3", got)
	}
	if got := testutil.ToFloat64(opmetrics.ClusterShardsReadyVec.WithLabelValues(testNamespace, testCluster)); got != 3 {
		t.Errorf("cluster_shards_ready = %v, want 3", got)
	}

	// One-hot: exactly the "Ready" state series is 1, the rest 0.
	for _, s := range opmetrics.ClusterStates {
		want := float64(0)
		if s == "Ready" {
			want = 1
		}
		if got := testutil.ToFloat64(opmetrics.ClusterStateVec.WithLabelValues(testNamespace, testCluster, s)); got != want {
			t.Errorf("cluster_state{state=%q} = %v, want %v", s, got, want)
		}
	}
}

func TestObserveClusterNotReadyIsZeroAndOneHotMoves(t *testing.T) {
	opmetrics.ResetForTest()

	opmetrics.ObserveCluster(testNamespace, testCluster, true, "Ready", 5, 5)
	opmetrics.ObserveCluster(testNamespace, testCluster, false, "Degraded", 5, 2)

	if got := testutil.ToFloat64(opmetrics.ClusterReadyVec.WithLabelValues(testNamespace, testCluster)); got != 0 {
		t.Errorf("cluster_ready after degrade = %v, want 0", got)
	}
	if got := testutil.ToFloat64(opmetrics.ClusterShardsReadyVec.WithLabelValues(testNamespace, testCluster)); got != 2 {
		t.Errorf("cluster_shards_ready after degrade = %v, want 2", got)
	}
	// The one-hot must have flipped: Ready=0, Degraded=1.
	if got := testutil.ToFloat64(opmetrics.ClusterStateVec.WithLabelValues(testNamespace, testCluster, "Ready")); got != 0 {
		t.Errorf("cluster_state{Ready} after degrade = %v, want 0", got)
	}
	if got := testutil.ToFloat64(opmetrics.ClusterStateVec.WithLabelValues(testNamespace, testCluster, "Degraded")); got != 1 {
		t.Errorf("cluster_state{Degraded} after degrade = %v, want 1", got)
	}
}

func TestDeleteClusterReapsGaugesNotCounters(t *testing.T) {
	opmetrics.ResetForTest()

	opmetrics.ObserveCluster(testNamespace, testCluster, true, "Ready", 3, 3)
	opmetrics.IncBackup(testNamespace, testCluster, opmetrics.ResultSucceeded)

	opmetrics.DeleteCluster(testNamespace, testCluster)

	// Gauges reaped: the vec must hold no series for the cluster anymore.
	if n := testutil.CollectAndCount(opmetrics.ClusterReadyVec); n != 0 {
		t.Errorf("cluster_ready series after delete = %d, want 0", n)
	}
	if n := testutil.CollectAndCount(opmetrics.ClusterStateVec); n != 0 {
		t.Errorf("cluster_state series after delete = %d, want 0", n)
	}
	if n := testutil.CollectAndCount(opmetrics.ClusterShardsDesiredVec); n != 0 {
		t.Errorf("cluster_shards_desired series after delete = %d, want 0", n)
	}
	if n := testutil.CollectAndCount(opmetrics.ClusterShardsReadyVec); n != 0 {
		t.Errorf("cluster_shards_ready series after delete = %d, want 0", n)
	}
	// Counter survives the cluster (monotonic event record).
	if got := testutil.ToFloat64(opmetrics.BackupTotalVec.WithLabelValues(testNamespace, testCluster, string(opmetrics.ResultSucceeded))); got != 1 {
		t.Errorf("backup_total survived delete = %v, want 1", got)
	}
}

func TestIncBackupAndRestoreByResult(t *testing.T) {
	opmetrics.ResetForTest()

	opmetrics.IncBackup(testNamespace, testCluster, opmetrics.ResultSucceeded)
	opmetrics.IncBackup(testNamespace, testCluster, opmetrics.ResultSucceeded)
	opmetrics.IncBackup(testNamespace, testCluster, opmetrics.ResultFailed)
	opmetrics.IncRestore(testNamespace, testCluster, opmetrics.ResultFailed)

	if got := testutil.ToFloat64(opmetrics.BackupTotalVec.WithLabelValues(testNamespace, testCluster, "Succeeded")); got != 2 {
		t.Errorf("backup_total{Succeeded} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(opmetrics.BackupTotalVec.WithLabelValues(testNamespace, testCluster, "Failed")); got != 1 {
		t.Errorf("backup_total{Failed} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(opmetrics.RestoreTotalVec.WithLabelValues(testNamespace, testCluster, "Failed")); got != 1 {
		t.Errorf("restore_total{Failed} = %v, want 1", got)
	}
}

func TestIncFailoverByKindAndGossipRepair(t *testing.T) {
	opmetrics.ResetForTest()

	opmetrics.IncFailover(testNamespace, testCluster, opmetrics.FailoverGraceful)
	opmetrics.IncFailover(testNamespace, testCluster, opmetrics.FailoverForce)
	opmetrics.IncFailover(testNamespace, testCluster, opmetrics.FailoverTakeover)
	opmetrics.IncFailover(testNamespace, testCluster, opmetrics.FailoverTakeover)
	opmetrics.IncGossipRepair(testNamespace, testCluster)

	cases := map[opmetrics.FailoverKind]float64{
		opmetrics.FailoverGraceful: 1,
		opmetrics.FailoverForce:    1,
		opmetrics.FailoverTakeover: 2,
	}
	for kind, want := range cases {
		if got := testutil.ToFloat64(opmetrics.FailoverTotalVec.WithLabelValues(testNamespace, testCluster, string(kind))); got != want {
			t.Errorf("failover_total{kind=%q} = %v, want %v", kind, got, want)
		}
	}
	if got := testutil.ToFloat64(opmetrics.GossipRepairTotalVec.WithLabelValues(testNamespace, testCluster)); got != 1 {
		t.Errorf("gossip_repair_total = %v, want 1", got)
	}
}

func TestCollectAndCompareExposesExpectedFormat(t *testing.T) {
	opmetrics.ResetForTest()

	opmetrics.IncBackup(testNamespace, testCluster, opmetrics.ResultSucceeded)

	expected := `
# HELP valkey_operator_backup_total Total PerconaValkeyBackups that reached a terminal state, by result.
# TYPE valkey_operator_backup_total counter
valkey_operator_backup_total{cluster="vk1",namespace="ns1",result="Succeeded"} 1
`
	if err := testutil.CollectAndCompare(opmetrics.BackupTotalVec, strings.NewReader(expected), "valkey_operator_backup_total"); err != nil {
		t.Fatalf("CollectAndCompare backup_total: %v", err)
	}
}
