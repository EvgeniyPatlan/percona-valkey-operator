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
	// ResourcePrefix is the prefix on every operator-managed child workload /
	// PVC / ConfigMap (Charter: workloads are named valkey-<node>). See
	// docs/architecture/03-api-design.md §6.1.
	ResourcePrefix = "valkey-"

	// pdbSuffix is the suffix on the operator-managed PodDisruptionBudget
	// (valkey-<cluster>-pdb).
	pdbSuffix = "-pdb"

	// internalPrefix prefixes the operator-managed (internal) Secrets.
	internalPrefix = "internal-"

	// pvcDataSuffix is the suffix on the data PVC (valkey-<node>-data).
	pvcDataSuffix = "-data"

	// aclSecretSuffix is the suffix on the rendered ACL-file Secret
	// (internal-<cluster>-acl, type valkey.io/acl).
	aclSecretSuffix = "-acl"

	// systemPasswordsSecretSuffix is the suffix on the system-passwords Secret
	// (internal-<cluster>-system-passwords, type Opaque) holding the random
	// _operator/_exporter/_backup passwords.
	systemPasswordsSecretSuffix = "-system-passwords"

	// AnnServerConfigHash is the pod-template annotation that carries the
	// parent-stamped server-config roll hash. A change to its value rolls the
	// single-replica workload (docs/architecture/04-control-plane.md §11).
	AnnServerConfigHash = labelPrefix + "server-config-hash"
)

// NodeWorkloadName returns the StatefulSet/Deployment name for a ValkeyNode:
// valkey-<node> (Charter, 03 §6.1).
func NodeWorkloadName(node string) string {
	return ResourcePrefix + node
}

// NodePVCName returns the data PVC name for a ValkeyNode: valkey-<node>-data
// (Charter, 03 §6.1).
func NodePVCName(node string) string {
	return ResourcePrefix + node + pvcDataSuffix
}

// NodeConfigMapName returns the per-node ConfigMap name the Node controller
// renders when the parent did not supply spec.serverConfigMapName: valkey-<node>.
// (In production the parent always supplies the shared valkey-<cluster> ConfigMap;
// this name is only used for a standalone ValkeyNode.)
func NodeConfigMapName(node string) string {
	return ResourcePrefix + node
}

// ClusterConfigMapName returns the shared rendered ConfigMap name for a cluster:
// valkey-<cluster> (03 §6.1). The Cluster controller renders it (M3); the Node
// controller only consumes it via spec.serverConfigMapName.
func ClusterConfigMapName(cluster string) string {
	return ResourcePrefix + cluster
}

// HeadlessServiceName returns the headless Service name: valkey-<cluster>
// (03 §6.1). Used to build the TLS ServerName for per-node client connections.
func HeadlessServiceName(cluster string) string {
	return ResourcePrefix + cluster
}

// ACLSecretName returns the rendered ACL-file Secret name: internal-<cluster>-acl
// (type valkey.io/acl), holding users.acl (03 §6.1).
func ACLSecretName(cluster string) string {
	return internalPrefix + cluster + aclSecretSuffix
}

// SystemPasswordsSecretName returns the system-passwords Secret name:
// internal-<cluster>-system-passwords (type Opaque) holding the random
// _operator/_exporter/_backup passwords (03 §6.1). The exporter sidecar reads
// its _exporter credential from here (OQ-2.1 interim choice: derive the name by
// Charter convention from the cluster label rather than adding a spec field).
func SystemPasswordsSecretName(cluster string) string {
	return internalPrefix + cluster + systemPasswordsSecretSuffix
}

// ClusterPDBName returns the operator-managed PodDisruptionBudget name:
// valkey-<cluster>-pdb (04 §2.1 step2).
func ClusterPDBName(cluster string) string {
	return ResourcePrefix + cluster + pdbSuffix
}

// NodeName returns the ValkeyNode CR name for a (shardIndex, nodeIndex)
// position: <cluster>-<shard>-<node>. node index 0 is the INITIAL primary; the
// live role is always read from the engine, never inferred from this name
// (Charter, 03 §6 / 04 §1). The child workload is then valkey-<NodeName>.
func NodeName(cluster string, shard, node int) string {
	return cluster + "-" + strconv.Itoa(shard) + "-" + strconv.Itoa(node)
}
