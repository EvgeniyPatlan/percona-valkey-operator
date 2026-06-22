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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file holds the sub-structs shared by the cluster, node, backup and
// restore kinds so controller-gen emits one deepcopy set for them. Field shapes
// follow docs/architecture/03-api-design.md §§2.5-2.12, §7, §8. Optional
// components are pointers (or maps) so the CEL has()-guard discipline (03 §4.2)
// is meaningful — an absent block short-circuits its immutability rule to true.

// ----------------------------------------------------------------------------
// Storage / persistence (03 §2.5)
// ----------------------------------------------------------------------------

// ReclaimPolicy controls whether a managed PVC is kept or garbage-collected when
// the owning ValkeyNode is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type ReclaimPolicy string

const (
	// ReclaimRetain keeps the PVC after node deletion (default; data-safe).
	ReclaimRetain ReclaimPolicy = "Retain"
	// ReclaimDelete garbage-collects the PVC (via finalizer) on node deletion.
	ReclaimDelete ReclaimPolicy = "Delete"
)

// PersistenceSpec declares durable storage for each Valkey pod. Its presence is
// meaningful (cache mode omits it), and once present its fields are governed by
// the immutability contract (03 §4): the block cannot be added/removed, size is
// expand-only and storageClassName is immutable.
type PersistenceSpec struct {
	// size is the requested PVC size. Required when the block is present; may
	// only grow (CEL compareTo >= 0).
	Size resource.Quantity `json:"size"`
	// storageClassName is the StorageClass for the PVC. Defaults to the cluster
	// default StorageClass. Immutable once set.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// reclaimPolicy controls whether the managed PVC is kept or GC'd when the
	// ValkeyNode is deleted.
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}

// ----------------------------------------------------------------------------
// TLS (03 §2.8) — discriminated union, LOCKED shape:
//   spec.tls.secretName (bring-your-own) XOR spec.tls.certManager.issuerRef.
// Neither set => TLS off. There is intentionally NO tls.certificate nesting.
// ----------------------------------------------------------------------------

// IssuerKind is the cert-manager issuer scope.
// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
type IssuerKind string

const (
	// IssuerKindIssuer references a namespaced cert-manager Issuer.
	IssuerKindIssuer IssuerKind = "Issuer"
	// IssuerKindClusterIssuer references a cluster-scoped cert-manager ClusterIssuer.
	IssuerKindClusterIssuer IssuerKind = "ClusterIssuer"
)

// IssuerRef references a cert-manager Issuer or ClusterIssuer.
type IssuerRef struct {
	// name is the name of the cert-manager Issuer/ClusterIssuer.
	Name string `json:"name"`
	// kind is the issuer scope.
	// +kubebuilder:default=Issuer
	// +optional
	Kind IssuerKind `json:"kind,omitempty"`
}

// CertManagerSpec is the cert-manager provisioning mode: the operator provisions
// a cert-manager.io/v1 Certificate (auto-rotated) from issuerRef.
type CertManagerSpec struct {
	// issuerRef names the cert-manager Issuer/ClusterIssuer used to provision the
	// Certificate.
	IssuerRef IssuerRef `json:"issuerRef"`
}

// TLSAuthClients controls the client-certificate policy (mTLS) the server
// enforces (07 §3.2). It maps to the Valkey tls-auth-clients directive:
// optional => tls-auth-clients optional (default; encryption + server auth, no
// client cert required), require => tls-auth-clients yes (mutual TLS), off =>
// tls-auth-clients no (no client-cert validation). Surfaced as a single enum so
// the policy cannot be partially configured.
// +kubebuilder:validation:Enum=off;optional;require
type TLSAuthClients string

const (
	// TLSAuthClientsOff disables client-certificate validation (tls-auth-clients no).
	TLSAuthClientsOff TLSAuthClients = "off"
	// TLSAuthClientsOptional validates a client cert when presented but does not
	// require one (tls-auth-clients optional; the upstream/operator default).
	TLSAuthClientsOptional TLSAuthClients = "optional"
	// TLSAuthClientsRequire enforces mutual TLS (tls-auth-clients yes).
	TLSAuthClientsRequire TLSAuthClients = "require"
)

// TLSConfig is the TLS-in-transit configuration for the client port and cluster
// bus. Exactly one of secretName / certManager may be set; neither => TLS off
// (the parent *TLSConfig pointer being nil). When present the operator renders
// tls-port=6379, port=0, tls-cluster=yes, tls-replication=yes.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.secretName) && has(self.certManager))",message="set at most one of tls.secretName or tls.certManager"
type TLSConfig struct {
	// secretName is the Secret-reference mode (alternative): the name of a
	// pre-existing Secret containing ca.crt, tls.crt and tls.key. The operator
	// validates the three keys exist and fails closed if any is missing.
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// certManager is the cert-manager mode (recommended): when set the operator
	// provisions a Certificate with DNS SANs for the headless Service + per-pod
	// names; cert-manager populates the TLS Secret (auto-rotation).
	// +optional
	CertManager *CertManagerSpec `json:"certManager,omitempty"`

	// authClients is the client-certificate (mTLS) policy. Defaults to optional
	// (encryption + server auth, ACL password auth over the channel). Set to
	// require for mutual TLS in zero-trust environments, or off to skip client
	// cert validation entirely (07 §3.2). Renders tls-auth-clients.
	// +kubebuilder:default=optional
	// +optional
	AuthClients TLSAuthClients `json:"authClients,omitempty"`
	// ciphers restricts the TLSv1.2-and-below cipher list (OpenSSL cipher-string
	// syntax). Renders tls-ciphers. Empty => server default. FIPS/compliance knob.
	// +optional
	Ciphers string `json:"ciphers,omitempty"`
	// cipherSuites restricts the TLSv1.3 cipher suites (OpenSSL ciphersuites
	// syntax). Renders tls-ciphersuites. Empty => server default.
	// +optional
	CipherSuites string `json:"cipherSuites,omitempty"`
	// dhParamsSecret references a Secret holding Diffie-Hellman parameters
	// (dh-params.pem) mounted and wired to tls-dh-params-file. Empty => server
	// default (no explicit DH params). Secret-ref only — never inline (ADR-008).
	// +optional
	DHParamsSecret *SecretRef `json:"dhParamsSecret,omitempty"`
}

// SecretRef references a single key within a Secret. Used for non-password TLS
// material (e.g. dhParamsSecret) where the UserPasswordSecret multi-key rotation
// shape does not apply. Secret-ref only (ADR-008): the operator reads the keyed
// value; the material is never inlined into the CR.
type SecretRef struct {
	// name is the Secret holding the referenced material.
	Name string `json:"name"`
	// key is the Secret key to read. Defaults to dh-params.pem when empty for a
	// dhParamsSecret reference (resolved by the consuming controller).
	// +optional
	Key string `json:"key,omitempty"`
}

// ----------------------------------------------------------------------------
// Exporter (03 §2.9)
// ----------------------------------------------------------------------------

// ExporterSpec configures the Prometheus exporter sidecar. When enabled, the
// _exporter system user is provisioned.
type ExporterSpec struct {
	// enabled toggles the exporter sidecar.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// image overrides the exporter image. Defaults to the operator exporter image.
	// +optional
	Image string `json:"image,omitempty"`
	// resources are the exporter container resource requests/limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// port is the exporter scrape port (08 §3). The named "metrics" container
	// port and the PodMonitor/ServiceMonitor target. Defaults to 9121 (Charter).
	// +kubebuilder:default=9121
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`
	// scrapeInterval is the recommended scrape cadence templated into the
	// PodMonitor/ServiceMonitor (08 §2.4). Defaults to 20s; avoid sub-10s on large
	// keyspaces (INFO competes with client traffic on the single thread).
	// +kubebuilder:default="20s"
	// +optional
	ScrapeInterval string `json:"scrapeInterval,omitempty"`
	// tls enables HTTPS scraping: the exporter serves metrics over TLS using the
	// cluster's cert family and the PodMonitor switches to scheme https (08 §3.3).
	// +optional
	TLS *ExporterTLSSpec `json:"tls,omitempty"`
}

// ExporterTLSSpec toggles metrics-over-TLS for the exporter (08 §3.3). When
// enabled the exporter serves /metrics over HTTPS (reusing the cluster TLS cert
// family) and the generated PodMonitor/ServiceMonitor scrapes with scheme https
// and the matching tlsConfig.
type ExporterTLSSpec struct {
	// enabled serves the exporter metrics endpoint over TLS.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// ----------------------------------------------------------------------------
// Users / ACL (03 §2.7)
// ----------------------------------------------------------------------------

// UserPasswordSecret references the Secret holding a user's password(s).
type UserPasswordSecret struct {
	// name is the Secret holding the password(s). Defaults to <cluster>-users
	// (derived in CheckNSetDefaults).
	// +optional
	Name string `json:"name,omitempty"`
	// keys are the Secret keys to read. Defaults to [name]. Multiple keys enable
	// Valkey multi-password rotation.
	// +optional
	Keys []string `json:"keys,omitempty"`
}

// UserCommands declares allowed/denied ACL command categories and commands. Bare
// tokens only — the renderer prepends +/-. The item pattern allows categories
// (@read), commands (get) and container|subcommand pairs (config|get).
type UserCommands struct {
	// allow lists categories/commands/container|subcommand pairs to allow.
	// +kubebuilder:validation:items:Pattern=`^@?[a-z][a-z0-9-]*(\|[a-z][a-z0-9-]*)?$`
	// +optional
	Allow []string `json:"allow,omitempty"`
	// deny lists categories/commands/container|subcommand pairs to deny.
	// +kubebuilder:validation:items:Pattern=`^@?[a-z][a-z0-9-]*(\|[a-z][a-z0-9-]*)?$`
	// +optional
	Deny []string `json:"deny,omitempty"`
}

// UserKeys declares key-pattern access for an ACL user. Patterns render to
// ~pattern (read-write), %R~pattern (read-only) and %W~pattern (write-only).
type UserKeys struct {
	// readWrite key patterns render to ~pattern.
	// +optional
	ReadWrite []string `json:"readWrite,omitempty"`
	// readOnly key patterns render to %R~pattern.
	// +optional
	ReadOnly []string `json:"readOnly,omitempty"`
	// writeOnly key patterns render to %W~pattern.
	// +optional
	WriteOnly []string `json:"writeOnly,omitempty"`
}

// UserChannels declares Pub/Sub channel access for an ACL user. Patterns render
// to &pattern.
type UserChannels struct {
	// patterns are Pub/Sub channel patterns rendered to &pattern.
	// +optional
	Patterns []string `json:"patterns,omitempty"`
}

// UserACLSpec is one ACL user. The users list is keyed by name (listType=map).
// The renderer emits all users (user-defined plus the two system users) into a
// single users.acl file. System users are reserved: names starting with "_" are
// rejected by CEL.
type UserACLSpec struct {
	// name is the username. Names starting with "_" are reserved for system
	// users (_operator/_exporter/_backup) and are rejected.
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('_')",message="usernames starting with _ are reserved for system users"
	Name string `json:"name"`
	// enabled toggles the ACL user.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// passwordSecret references the Secret holding the user's password(s).
	// +optional
	PasswordSecret UserPasswordSecret `json:"passwordSecret,omitempty"`
	// nopass makes the user passwordless.
	// +kubebuilder:default=false
	// +optional
	Nopass bool `json:"nopass,omitempty"`
	// resetpass applies the resetpass ACL flag.
	// +kubebuilder:default=false
	// +optional
	Resetpass bool `json:"resetpass,omitempty"`
	// commands declares allowed/denied command categories and commands.
	// +optional
	Commands *UserCommands `json:"commands,omitempty"`
	// keys declares key-pattern access.
	// +optional
	Keys *UserKeys `json:"keys,omitempty"`
	// channels declares Pub/Sub channel access.
	// +optional
	Channels *UserChannels `json:"channels,omitempty"`
	// permissions is raw ACL appended verbatim after the generated rules.
	// +optional
	Permissions string `json:"permissions,omitempty"`
}

// ----------------------------------------------------------------------------
// Default-user authentication (07 §3 / gap §2.3) — SECURITY
// ----------------------------------------------------------------------------

// AuthSpec configures the Valkey default user's password (requirepass). This is
// distinct from the ACL users[] list: those model named, non-default ACL users
// (plus the reserved _operator/_exporter/_backup system users), whereas this
// block governs ONLY the built-in "default" user / requirepass — the chart's
// primary auth knob. All password material is Secret-ref only (never inline,
// ADR-008).
type AuthSpec struct {
	// enabled toggles default-user password auth (requirepass). When true the
	// default user requires the password from passwordSecret; when false the
	// default user is left passwordless (nopass). Defaults to true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// passwordSecret references the Secret holding the default user's password(s).
	// Multiple keys enable Valkey multi-password rotation (live ACL SETUSER, no
	// pod roll). Defaults to the <cluster>-users Secret (derived in
	// CheckNSetDefaults) when enabled and a name is not given.
	// +optional
	PasswordSecret UserPasswordSecret `json:"passwordSecret,omitempty"`
}

// ----------------------------------------------------------------------------
// Service exposure / external access (gap §2.12) — per-pod for cluster mode
// ----------------------------------------------------------------------------

// ExposeSpec controls how the cluster is reachable from outside the operator's
// headless Service. When type is NodePort/LoadBalancer the operator provisions
// the external Service(s); perPod creates one external Service per ValkeyNode
// plus the cluster-announce-ip wiring required for cluster-mode clients to
// follow MOVED/ASK redirects to per-pod external addresses.
type ExposeSpec struct {
	// type is the client Service type. ClusterIP (default) keeps access in-cluster;
	// NodePort/LoadBalancer expose the cluster externally.
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`
	// loadBalancerSourceRanges restricts client CIDRs when type is LoadBalancer.
	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`
	// annotations are added to the generated external Service(s) (e.g. cloud LB
	// controller hints).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// perPod creates an external Service per ValkeyNode and sets cluster-announce-ip
	// so cluster-mode clients can reach individual shards directly (the operator
	// performs the announce-IP discovery the chart's init container did).
	// +optional
	PerPod bool `json:"perPod,omitempty"`
}

// ----------------------------------------------------------------------------
// NetworkPolicy generation (07 §7 / gap §2.14) — SECURITY
// ----------------------------------------------------------------------------

// NetworkPolicySpec toggles and customizes the operator-managed default-deny
// perimeter (07 §7). It replaces the M5 interim annotation gate
// (valkey.percona.com/network-policy + ...-monitoring-namespace). enabled is a
// pointer so absence is distinct from explicit false (the operator's default is
// off unless explicitly enabled, matching the opt-in interim behaviour).
type NetworkPolicySpec struct {
	// enabled turns on the operator-managed default-deny NetworkPolicy plus the
	// required data-plane/metrics flows. nil/false => no policy is created
	// (opt-in; recommended true in production, 07 §7).
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// clientNamespaceSelectors selects namespaces whose pods may reach the client
	// port (6379). Empty => same-namespace pods only (the interim default).
	// +optional
	ClientNamespaceSelectors []metav1.LabelSelector `json:"clientNamespaceSelectors,omitempty"`
	// clientPodSelectors selects pods (in allowed namespaces) that may reach the
	// client port (6379). Empty => any pod in the allowed namespaces.
	// +optional
	ClientPodSelectors []metav1.LabelSelector `json:"clientPodSelectors,omitempty"`
	// monitoringNamespace is the namespace whose Prometheus pods may scrape the
	// exporter (9121). Empty => the cluster's own namespace (08 §3.4).
	// +optional
	MonitoringNamespace string `json:"monitoringNamespace,omitempty"`
}

// ----------------------------------------------------------------------------
// Upgrade options (03 §2.12)
// ----------------------------------------------------------------------------

// UpgradeOptions configures Percona smart updates via the version service.
type UpgradeOptions struct {
	// apply is the smart-update policy. Disabled = pinned image; Recommended/
	// Latest = poll the version service; <version> = pin a specific engine
	// version. The enum is intentionally open (a free-form version string is
	// allowed), so it is documented rather than enum-constrained.
	// +optional
	Apply string `json:"apply,omitempty"`
	// schedule is the cron expression governing when to poll/apply.
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// versionServiceEndpoint is the Percona-style version service endpoint.
	// +optional
	VersionServiceEndpoint string `json:"versionServiceEndpoint,omitempty"`
}

const (
	// UpgradeApplyDisabled pins the engine image (no version-service polling).
	UpgradeApplyDisabled = "Disabled"
	// UpgradeApplyRecommended applies the version service's recommended engine.
	UpgradeApplyRecommended = "Recommended"
	// UpgradeApplyLatest applies the version service's latest engine.
	UpgradeApplyLatest = "Latest"
)

// ----------------------------------------------------------------------------
// Backup spec sub-structs (03 §2.11, §7)
// ----------------------------------------------------------------------------

// BackupStorageType is the backend type for a named backup storage.
// +kubebuilder:validation:Enum=s3;gcs;azure;filesystem
type BackupStorageType string

const (
	// BackupStorageS3 is an S3-compatible object store.
	BackupStorageS3 BackupStorageType = "s3"
	// BackupStorageGCS is Google Cloud Storage.
	BackupStorageGCS BackupStorageType = "gcs"
	// BackupStorageAzure is Azure Blob storage.
	BackupStorageAzure BackupStorageType = "azure"
	// BackupStorageFilesystem is a filesystem/PVC backend (test-only).
	BackupStorageFilesystem BackupStorageType = "filesystem"
)

// BackupStorageS3Spec configures an S3-compatible backup backend. Destinations
// are prefixed s3://.
type BackupStorageS3Spec struct {
	// bucket is the S3 bucket name.
	Bucket string `json:"bucket"`
	// prefix is the key prefix within the bucket.
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// region is the S3 region.
	// +optional
	Region string `json:"region,omitempty"`
	// endpointUrl overrides the S3 endpoint (for S3-compatible stores).
	// +optional
	EndpointURL string `json:"endpointUrl,omitempty"`
	// credentialsSecret holds the access credentials.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// BackupStorageGCSSpec configures a Google Cloud Storage backup backend.
// Destinations are prefixed gs://.
type BackupStorageGCSSpec struct {
	// bucket is the GCS bucket name.
	Bucket string `json:"bucket"`
	// prefix is the object prefix within the bucket.
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// credentialsSecret holds the service-account credentials.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// BackupStorageAzureSpec configures an Azure Blob backup backend. Destinations
// are prefixed azure://.
type BackupStorageAzureSpec struct {
	// container is the Azure Blob container name.
	Container string `json:"container"`
	// prefix is the blob prefix within the container.
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// credentialsSecret holds the storage-account credentials.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// BackupStorageSpec is a named storage backend referenced by backups/schedules.
type BackupStorageSpec struct {
	// type is the backend type. filesystem/pvc is test-only.
	Type BackupStorageType `json:"type"`
	// s3 holds the S3 configuration when type is s3.
	// +optional
	S3 *BackupStorageS3Spec `json:"s3,omitempty"`
	// gcs holds the GCS configuration when type is gcs.
	// +optional
	GCS *BackupStorageGCSSpec `json:"gcs,omitempty"`
	// azure holds the Azure configuration when type is azure.
	// +optional
	Azure *BackupStorageAzureSpec `json:"azure,omitempty"`
}

// BackupScheduleType is the snapshot type for a scheduled backup. Only full RDB
// snapshots are supported in v1alpha1 (PITR/incremental deferred — ADR-012).
// +kubebuilder:validation:Enum=full
type BackupScheduleType string

const (
	// BackupTypeFull is an RDB full snapshot.
	BackupTypeFull BackupScheduleType = "full"
)

// BackupScheduleSpec is one cron-driven scheduled backup.
type BackupScheduleSpec struct {
	// name is the schedule identifier.
	Name string `json:"name"`
	// schedule is the cron expression (robfig/cron).
	Schedule string `json:"schedule"`
	// storageName must be a key in spec.backup.storages (validated in
	// CheckNSetDefaults).
	StorageName string `json:"storageName"`
	// keep is the retention count (0 = unlimited). GC is finalizer-driven.
	// +kubebuilder:default=0
	// +optional
	Keep int `json:"keep,omitempty"`
	// type is the snapshot type.
	// +kubebuilder:default=full
	// +optional
	Type BackupScheduleType `json:"type,omitempty"`
}

// BackupSpec is the cluster's backup block: storage definitions and schedules.
// Individual on-demand backups are separate PerconaValkeyBackup CRs referencing
// a storage by name (Percona minimalism).
type BackupSpec struct {
	// image is the backup-tool image (RDB ship-out). Defaults to
	// percona/valkey-backup:<tag> in CheckNSetDefaults.
	// +optional
	Image string `json:"image,omitempty"`
	// serviceAccountName is the SA for backup Jobs.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// storages are named storage backends keyed by storageName.
	// +optional
	Storages map[string]BackupStorageSpec `json:"storages,omitempty"`
	// schedule lists cron-driven scheduled backups.
	// +optional
	Schedule []BackupScheduleSpec `json:"schedule,omitempty"`
}

// ----------------------------------------------------------------------------
// Backup CR sub-structs (03 §7)
// ----------------------------------------------------------------------------

// BackupConsistency is the snapshot-coordination mode across shards.
// +kubebuilder:validation:Enum=strict;best-effort
type BackupConsistency string

const (
	// ConsistencyStrict fails the backup if any shard snapshot is not freshly
	// produced and verified (the "no silent data loss" invariant).
	ConsistencyStrict BackupConsistency = "strict"
	// ConsistencyBestEffort records partial coverage and marks the result Degraded.
	ConsistencyBestEffort BackupConsistency = "best-effort"
)

// BackupRetentionSpec is the count/age GC policy for a backup-set. Deletion is
// driven by the percona.com/delete-backup finalizer.
type BackupRetentionSpec struct {
	// keep is the count of backups to retain (0 = unlimited).
	// +optional
	Keep int `json:"keep,omitempty"`
	// keepAge is a duration string; backups older than this are GC'd.
	// +optional
	KeepAge string `json:"keepAge,omitempty"`
}

// BackupContainerOptions are extra args/env and tuning knobs for the
// cmd/valkey-backup tool.
type BackupContainerOptions struct {
	// args are extra CLI args for the backup tool.
	// +optional
	Args []string `json:"args,omitempty"`
	// env are extra environment variables for the backup container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// compressionLevel tunes the RDB compression level.
	// +optional
	CompressionLevel *int `json:"compressionLevel,omitempty"`
	// parallelShards caps how many shards are snapshotted in parallel.
	// +optional
	ParallelShards *int `json:"parallelShards,omitempty"`
	// preferReplica snapshots from a replica when available.
	// +optional
	PreferReplica *bool `json:"preferReplica,omitempty"`
}

// ----------------------------------------------------------------------------
// Backup status sub-structs (03 §7.2)
// ----------------------------------------------------------------------------

// ShardBackupStatus is per-shard slot-coverage metadata so restore can verify
// all 16384 slots are represented.
type ShardBackupStatus struct {
	// shardIndex is the zero-based shard index.
	ShardIndex int32 `json:"shardIndex"`
	// slotRange is the shard's slot range (e.g. "0-5460").
	// +optional
	SlotRange string `json:"slotRange,omitempty"`
	// rdbObject is the backend object key for this shard's RDB.
	// +optional
	RDBObject string `json:"rdbObject,omitempty"`
	// sizeBytes is the RDB object size.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// checksum is the RDB object checksum.
	// +optional
	Checksum string `json:"checksum,omitempty"`
}

// ----------------------------------------------------------------------------
// Restore sub-structs (03 §8.1)
// ----------------------------------------------------------------------------

// BackupSource is an inline restore source (for restoring from an artifact whose
// PerconaValkeyBackup CR no longer exists), hydrated from a
// PerconaValkeyBackup.status.
type BackupSource struct {
	// destination is the backend-prefixed root (s3://bucket/prefix/<backup>, ...).
	// +optional
	Destination string `json:"destination,omitempty"`
	// storageName is echoed from the original backup.
	// +optional
	StorageName string `json:"storageName,omitempty"`
	// s3 holds resolved S3 storage details copied from the backup.
	// +optional
	S3 *BackupStorageS3Spec `json:"s3,omitempty"`
	// gcs holds resolved GCS storage details copied from the backup.
	// +optional
	GCS *BackupStorageGCSSpec `json:"gcs,omitempty"`
	// azure holds resolved Azure storage details copied from the backup.
	// +optional
	Azure *BackupStorageAzureSpec `json:"azure,omitempty"`
}

// conditionsMapMerge documents that condition slices in this API are listType=map
// keyed by type. Kept here as a reference for reviewers; metav1.Condition is the
// element type used by every *Status.conditions field.
var _ = metav1.Condition{}
