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

package naming

import "strconv"

const (
	// ComponentValkey is the component value stamped on Valkey server pods and
	// their child workloads (app.kubernetes.io/component + valkey.percona.com/component).
	ComponentValkey = "valkey"

	// SystemUserOperator is the cluster-orchestration ACL user (03 §2.7, 07 §4.3).
	SystemUserOperator = "_operator"
	// SystemUserExporter is the read-only metrics-scraper ACL user the exporter
	// sidecar authenticates as (08 §2.4).
	SystemUserExporter = "_exporter"
	// SystemUserBackup is the server-side snapshot ACL user (06).
	SystemUserBackup = "_backup"
)

// NodeLabels returns the full label set for the child resources of a ValkeyNode.
// It is the base recommended-label set (component=valkey) plus the operator
// topology labels (cluster / shard-index / node-index) copied verbatim from the
// node's own labels, which the parent Cluster controller stamps. The returned
// map is a fresh copy; callers may mutate it freely.
func NodeLabels(nodeName string, nodeLabels map[string]string) map[string]string {
	cluster := nodeName
	if v, ok := nodeLabels[LabelCluster]; ok && v != "" {
		cluster = v
	}
	l := Labels(cluster, ComponentValkey)
	l[LabelAppInstance] = nodeName
	for _, key := range []string{LabelCluster, LabelShardIndex, LabelNodeIndex} {
		if v, ok := nodeLabels[key]; ok && v != "" {
			l[key] = v
		}
	}
	return l
}

// NodeCluster returns the cluster name a ValkeyNode belongs to, read from its
// valkey.percona.com/cluster label. Falls back to the node name when the label
// is absent (a standalone ValkeyNode created without a parent). This is the
// OQ-2.1 interim mechanism for resolving cluster-scoped Secret names.
func NodeCluster(nodeName string, nodeLabels map[string]string) string {
	if v, ok := nodeLabels[LabelCluster]; ok && v != "" {
		return v
	}
	return nodeName
}

// ClusterTopologyLabels returns the operator topology labels the Cluster
// controller stamps on each ValkeyNode: cluster + shard-index + node-index. The
// Node controller copies these verbatim onto its child workload/pods so the
// parent can select per-cluster/per-shard (04 §1 naming reminder). The returned
// map is fresh; callers may add to it freely.
func ClusterTopologyLabels(cluster string, shard, node int) map[string]string {
	return map[string]string{
		LabelCluster:    cluster,
		LabelShardIndex: strconv.Itoa(shard),
		LabelNodeIndex:  strconv.Itoa(node),
	}
}

// ClusterSelector returns the label selector matching every resource of a
// cluster (valkey.percona.com/cluster=<cluster>). Used to List the cluster's
// ValkeyNodes (04 §2.1 step5) and as the headless Service pod selector.
func ClusterSelector(cluster string) map[string]string {
	return map[string]string{LabelCluster: cluster}
}

// ShardSelector returns the label selector matching every node of one shard
// (cluster + shard-index). Used for shard-scoped PDBs and replica placement.
func ShardSelector(cluster string, shard int) map[string]string {
	return map[string]string{
		LabelCluster:    cluster,
		LabelShardIndex: strconv.Itoa(shard),
	}
}
