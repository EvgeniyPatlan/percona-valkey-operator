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

	// AnnTLSHash is the pod-template annotation that carries the parent-stamped
	// TLS hash (SHA-256 of the cert material identity). A real cert change bumps
	// the value and rolls the workload via the same machinery as
	// AnnServerConfigHash, replicas-before-primary with proactive failover
	// (M5 GO-5.8; docs/architecture/07-security.md §3.4). The hash changes ONLY on
	// a real cert change, never on an unrelated reconcile.
	AnnTLSHash = labelPrefix + "tls-hash"

	// tlsSecretSuffix is the suffix on the operator-provisioned cert-manager TLS
	// Secret (internal-<cluster>-tls). In secret-ref mode the user supplies the
	// Secret name directly via spec.tls.secretName instead.
	tlsSecretSuffix = "-tls"

	// TLSMountPath is the read-only mount point for the TLS cert material
	// (ca.crt/tls.crt/tls.key) inside every Valkey pod. FROZEN M5 contract: the
	// rendered tls-cert-file/tls-key-file/tls-ca-cert-file directives
	// (pkg/valkey/config.go) and the exporter --tls-ca-cert-file flag and the
	// valkey-cli probe --cacert/--cert/--key flags all reference this single
	// path (docs/architecture/07-security.md §3.1).
	TLSMountPath = "/etc/valkey/tls"

	// TLSSecretKeyCA is the CA-bundle data key in the three-key
	// kubernetes.io/tls-shaped TLS Secret (07 §3.1). FROZEN M5 contract.
	TLSSecretKeyCA = "ca.crt"
	// TLSSecretKeyCert is the server-certificate data key in the TLS Secret.
	TLSSecretKeyCert = "tls.crt"
	// TLSSecretKeyKey is the server private-key data key in the TLS Secret.
	TLSSecretKeyKey = "tls.key"
)

// Exporter sidecar credential env var names (the redis_exporter convention).
// FROZEN M5 contract: the exporter authenticates as the _exporter system user
// with its password sourced from the internal-<cluster>-system-passwords Secret
// (docs/architecture/08-observability.md §2.4).
const (
	// EnvExporterUser is the env var carrying the _exporter ACL username.
	EnvExporterUser = "REDIS_USER"
	// EnvExporterPassword is the env var carrying the _exporter password
	// (injected via a SecretKeyRef to internal-<cluster>-system-passwords, key
	// _exporter).
	EnvExporterPassword = "REDIS_PASSWORD"
)

// TLSSecretName returns the operator-provisioned TLS Secret name for cert-manager
// mode: internal-<cluster>-tls. cert-manager writes ca.crt/tls.crt/tls.key into
// it from the provisioned Certificate (M5 GO-5.6, docs/architecture/07-security.md
// §3.3). In secret-ref mode the Secret name comes from spec.tls.secretName instead.
func TLSSecretName(cluster string) string {
	return internalPrefix + cluster + tlsSecretSuffix
}

// NodeWorkloadName returns the StatefulSet/Deployment name for a ValkeyNode:
// valkey-<node> (Charter, 03 §6.1).
func NodeWorkloadName(node string) string {
	return ResourcePrefix + node
}

// NodePVCName returns the data PVC name for a ValkeyNode: valkey-<node>-data
// (Charter, 03 §6.1). This is the volumeClaimTemplate name on the StatefulSet,
// NOT the name of the PVC the StatefulSet controller actually materializes — see
// NodeStatefulSetPVCName.
func NodePVCName(node string) string {
	return ResourcePrefix + node + pvcDataSuffix
}

// NodeStatefulSetPVCName returns the name of the PVC the StatefulSet controller
// actually creates for a ValkeyNode's single (ordinal-0) pod:
// <vctName>-<stsName>-0, i.e. valkey-<node>-data-valkey-<node>-0. The STS suffixes
// each volumeClaimTemplate with "-<podName>"; a ValkeyNode workload always has
// exactly one replica (ordinal 0), so the live PVC name is fixed. The bare
// NodePVCName (the VCT name) never exists as a standalone object for an
// STS-backed node, so PVC reads/resizes must target THIS name.
func NodeStatefulSetPVCName(node string) string {
	return NodePVCName(node) + "-" + NodeWorkloadName(node) + "-0"
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
