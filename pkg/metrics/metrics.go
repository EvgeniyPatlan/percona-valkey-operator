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

// Package metrics defines the operator's BUSINESS-level Prometheus collectors and
// the small typed API the reconcilers call to keep them current. The collectors
// complement (never duplicate) controller-runtime's built-in reconcile/workqueue
// metrics (controller_runtime_reconcile_total/_errors/_time_seconds): those measure
// the reconcile machinery, while these surface the OBSERVED state of the managed
// Valkey clusters (readiness, shard counts) and the operator's terminal actions
// (backups, restores, failovers, gossip repairs).
//
// All series are deliberately LOW-cardinality: cluster gauges carry only
// (namespace, cluster); the action counters add a single bounded-enum dimension
// (result Succeeded|Failed, or failover kind graceful|force|takeover). Nothing is
// ever labelled by pod, node, UID or any unbounded value, so the time-series count
// stays bounded by the number of managed clusters.
//
// The collectors register on controller-runtime's shared registry
// (sigs.k8s.io/controller-runtime/pkg/metrics) via this package's init(), so they
// are served on the manager's existing /metrics endpoint with no extra wiring; the
// reconcilers only need to call the exported setter/incrementer functions.
// Per-cluster gauge series are reaped on cluster teardown via DeleteCluster so a
// removed cluster never leaves a stale gauge behind.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// metricsNamespace is the Prometheus namespace (name prefix) for every operator
// business metric, yielding names such as valkey_operator_cluster_ready.
const metricsNamespace = "valkey_operator"

// Label-name constants keep the (low-cardinality) label set single-sourced across
// the collectors and the setters that drive them.
const (
	labelNamespace = "namespace"
	labelCluster   = "cluster"
	labelState     = "state"
	labelResult    = "result"
	labelKind      = "kind"
)

// Result is the bounded outcome enum for the backup/restore counters. It is a
// closed set (Succeeded|Failed) so the result label can never explode cardinality.
type Result string

const (
	// ResultSucceeded marks a backup/restore that reached terminal success.
	ResultSucceeded Result = "Succeeded"
	// ResultFailed marks a backup/restore that reached a terminal failure.
	ResultFailed Result = "Failed"
)

// FailoverKind is the bounded enum for the failover counter, mirroring the three
// CLUSTER FAILOVER variants the operator issues (05 §6-§7).
type FailoverKind string

const (
	// FailoverGraceful is a graceful (proactive, pre-roll) failover.
	FailoverGraceful FailoverKind = "graceful"
	// FailoverForce is a forced failover (CLUSTER FAILOVER FORCE).
	FailoverForce FailoverKind = "force"
	// FailoverTakeover is a last-resort takeover (CLUSTER FAILOVER TAKEOVER).
	FailoverTakeover FailoverKind = "takeover"
)

// clusterStates is the closed set of states the cluster_state gauge reports one
// series per. Keeping it fixed bounds the (namespace, cluster) × state cardinality
// and lets DeleteCluster reap every state series for a removed cluster.
var clusterStates = []string{"Initializing", "Reconciling", "Ready", "Degraded", "Failed"}

var (
	// clusterReady is 1 when the cluster's Ready condition holds, else 0. The single
	// at-a-glance health signal per managed cluster.
	clusterReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "cluster_ready",
		Help:      "1 if the PerconaValkeyCluster is Ready, 0 otherwise.",
	}, []string{labelNamespace, labelCluster})

	// clusterState is a one-hot gauge: exactly one state series is 1 and the rest 0
	// for each cluster, so dashboards can chart the cluster's lifecycle state.
	clusterState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "cluster_state",
		Help:      "One-hot gauge of the PerconaValkeyCluster state (1 for the current state, 0 for the others).",
	}, []string{labelNamespace, labelCluster, labelState})

	// clusterShardsDesired is the spec.shards target for the cluster.
	clusterShardsDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "cluster_shards_desired",
		Help:      "Desired number of shards (spec.shards) for the PerconaValkeyCluster.",
	}, []string{labelNamespace, labelCluster})

	// clusterShardsReady is the count of fully-healthy shards (status.readyShards).
	clusterShardsReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "cluster_shards_ready",
		Help:      "Number of fully-healthy shards (status.readyShards) for the PerconaValkeyCluster.",
	}, []string{labelNamespace, labelCluster})

	// backupTotal counts terminal backups by result (Succeeded|Failed).
	backupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "backup_total",
		Help:      "Total PerconaValkeyBackups that reached a terminal state, by result.",
	}, []string{labelNamespace, labelCluster, labelResult})

	// restoreTotal counts terminal restores by result (Succeeded|Failed).
	restoreTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "restore_total",
		Help:      "Total PerconaValkeyRestores that reached a terminal state, by result.",
	}, []string{labelNamespace, labelCluster, labelResult})

	// failoverTotal counts CLUSTER FAILOVER actions the operator issued, by kind.
	failoverTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "failover_total",
		Help:      "Total CLUSTER FAILOVER actions the operator issued, by kind (graceful|force|takeover).",
	}, []string{labelNamespace, labelCluster, labelKind})

	// gossipRepairTotal counts stale-gossip repair passes (bug #1) that re-MEET at
	// least one node at its current address.
	gossipRepairTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "gossip_repair_total",
		Help:      "Total stale-gossip repair passes that re-MEET at least one node at its current address (bug #1).",
	}, []string{labelNamespace, labelCluster})
)

// collectors is the full set registered on the shared registry, kept in one slice
// so registration (init) and test re-registration share a single source of truth.
func collectors() []prometheus.Collector {
	return []prometheus.Collector{
		clusterReady,
		clusterState,
		clusterShardsDesired,
		clusterShardsReady,
		backupTotal,
		restoreTotal,
		failoverTotal,
		gossipRepairTotal,
	}
}

func init() {
	crmetrics.Registry.MustRegister(collectors()...)
}

// ObserveCluster records the observed health of one cluster: the ready gauge, the
// one-hot state gauge, and the desired/ready shard counts. ready is the cluster's
// Ready-condition verdict; state is the derived status.state; desiredShards is
// spec.shards and readyShards is status.readyShards. It is called from the cluster
// reconciler's status-write path so the gauges track every reconcile that finalizes
// status, across all states (Ready, Degraded, Failed, ...).
func ObserveCluster(namespace, cluster string, ready bool, state string, desiredShards, readyShards int) {
	clusterReady.WithLabelValues(namespace, cluster).Set(boolToFloat(ready))
	for _, s := range clusterStates {
		clusterState.WithLabelValues(namespace, cluster, s).Set(boolToFloat(s == state))
	}
	clusterShardsDesired.WithLabelValues(namespace, cluster).Set(float64(desiredShards))
	clusterShardsReady.WithLabelValues(namespace, cluster).Set(float64(readyShards))
}

// DeleteCluster reaps every per-cluster gauge series for the named cluster so a
// removed cluster does not leave stale gauges behind. It is wired into the cluster
// teardown finalizer. The action counters (backup/restore/failover/gossip) are
// intentionally NOT deleted: a counter is a monotonic record of events that
// happened and is expected to survive the resource it describes.
func DeleteCluster(namespace, cluster string) {
	clusterReady.DeleteLabelValues(namespace, cluster)
	clusterShardsDesired.DeleteLabelValues(namespace, cluster)
	clusterShardsReady.DeleteLabelValues(namespace, cluster)
	for _, s := range clusterStates {
		clusterState.DeleteLabelValues(namespace, cluster, s)
	}
}

// IncBackup increments the terminal-backup counter for the cluster by result.
func IncBackup(namespace, cluster string, result Result) {
	backupTotal.WithLabelValues(namespace, cluster, string(result)).Inc()
}

// IncRestore increments the terminal-restore counter for the cluster by result.
func IncRestore(namespace, cluster string, result Result) {
	restoreTotal.WithLabelValues(namespace, cluster, string(result)).Inc()
}

// IncFailover increments the failover-action counter for the cluster by kind.
func IncFailover(namespace, cluster string, kind FailoverKind) {
	failoverTotal.WithLabelValues(namespace, cluster, string(kind)).Inc()
}

// IncGossipRepair increments the stale-gossip-repair counter for the cluster (bug
// #1): one increment per repair pass that re-MEETs at least one node.
func IncGossipRepair(namespace, cluster string) {
	gossipRepairTotal.WithLabelValues(namespace, cluster).Inc()
}

// boolToFloat maps a bool to the gauge value 1 (true) or 0 (false).
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
