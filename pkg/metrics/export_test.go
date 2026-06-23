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

package metrics

import "github.com/prometheus/client_golang/prometheus"

// Test-only accessors expose the package-private collectors and helpers so the
// white-box tests can assert series values via prometheus testutil without
// widening the public API (mirrors the repo's export_test.go convention).
var (
	ClusterReadyVec         = clusterReady
	ClusterStateVec         = clusterState
	ClusterShardsDesiredVec = clusterShardsDesired
	ClusterShardsReadyVec   = clusterShardsReady
	BackupTotalVec          = backupTotal
	RestoreTotalVec         = restoreTotal
	FailoverTotalVec        = failoverTotal
	GossipRepairTotalVec    = gossipRepairTotal
)

// ClusterStates exposes the closed state set for the one-hot assertion.
var ClusterStates = clusterStates

// ResetForTest clears every collector so each test starts from a clean slate
// regardless of ordering. The collectors stay registered on the shared registry.
func ResetForTest() {
	for _, c := range []interface{ Reset() }{
		clusterReady, clusterState, clusterShardsDesired, clusterShardsReady,
		backupTotal, restoreTotal, failoverTotal, gossipRepairTotal,
	} {
		c.Reset()
	}
}

// CollectorsForTest returns the registered collectors for registration assertions.
func CollectorsForTest() []prometheus.Collector { return collectors() }
