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

package backup

import (
	"cmp"
	"slices"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// This file is the SINGLE SOURCE OF TRUTH for the VALKEY_BACKUP_* environment
// contract between the operator's Job builders (the perconavalkeybackup and
// perconavalkeyrestore controllers) and the cmd/valkey-backup sidecar that runs
// inside the Job pod. The sidecar reads its operation inputs (storage type +
// coordinates, cluster/backup names, the Valkey seed node + auth + TLS) from
// these env vars and takes only --download/--cleanup/--shard as flags (06 §4.1,
// §8.7). cmd/valkey-backup re-exports these names so the two sides cannot drift,
// and the controllers build the env via JobEnv so a future env-name change is a
// one-line edit here rather than three.

// Environment variable names the cmd/valkey-backup sidecar reads. Storage
// credential VALUES travel separately under the cloud-SDK names (AWS_*/AZURE_*/
// GOOGLE_*) mounted from the credentials Secret; these carry only non-secret
// operation inputs.
const (
	// EnvCluster is the source/target cluster name (used to derive object keys).
	EnvCluster = "VALKEY_BACKUP_CLUSTER"
	// EnvBackupName is the backup-set name (used to derive object keys).
	EnvBackupName = "VALKEY_BACKUP_NAME"
	// EnvMode is the cluster topology mode recorded in the manifest (default cluster).
	EnvMode = "VALKEY_BACKUP_MODE"
	// EnvCRVersion is the source cluster's crVersion recorded in the manifest.
	EnvCRVersion = "VALKEY_BACKUP_CR_VERSION"
	// EnvConsistency is strict|best-effort (06 §9.2).
	EnvConsistency = "VALKEY_BACKUP_CONSISTENCY"
	// EnvStorageType selects the backend (s3/gcs/azure/filesystem).
	EnvStorageType = "VALKEY_BACKUP_STORAGE_TYPE"
	// EnvS3Bucket / EnvS3Prefix / EnvS3Region / EnvS3Endpoint carry the S3 coordinates.
	EnvS3Bucket   = "VALKEY_BACKUP_S3_BUCKET"
	EnvS3Prefix   = "VALKEY_BACKUP_S3_PREFIX"
	EnvS3Region   = "VALKEY_BACKUP_S3_REGION"
	EnvS3Endpoint = "VALKEY_BACKUP_S3_ENDPOINT"
	// EnvGCSBucket / EnvGCSPrefix carry the GCS coordinates.
	EnvGCSBucket = "VALKEY_BACKUP_GCS_BUCKET"
	EnvGCSPrefix = "VALKEY_BACKUP_GCS_PREFIX"
	// EnvAzureContainer / EnvAzurePrefix carry the Azure coordinates.
	EnvAzureContainer = "VALKEY_BACKUP_AZURE_CONTAINER"
	EnvAzurePrefix    = "VALKEY_BACKUP_AZURE_PREFIX"
	// EnvFSRoot is the on-disk root for the test-only filesystem backend.
	EnvFSRoot = "VALKEY_BACKUP_FS_ROOT"
	// EnvSeedNode is a reachable Valkey node the backup Job scrapes CLUSTER NODES
	// from to resolve each shard's live primary (backup mode only).
	EnvSeedNode = "VALKEY_BACKUP_SEED_NODE"
	// EnvAuthUser is the ACL user the Job authenticates as (default _backup; M6
	// security refactor, 07 §10 — the SYNC-as-replica grants live on _backup).
	EnvAuthUser = "VALKEY_BACKUP_AUTH_USER"
	// EnvAuthPassword is the env-var NAME the Job reads the auth password from; its
	// VALUE is supplied by a mounted-Secret env entry, never embedded here.
	EnvAuthPassword = "VALKEY_BACKUP_AUTH_PASSWORD"
	// EnvTLSEnabled is "true" when the Job must use TLS to talk to Valkey.
	EnvTLSEnabled = "VALKEY_BACKUP_TLS"
	// EnvTLSCAFile is the mounted CA bundle path for engine TLS.
	EnvTLSCAFile = "VALKEY_BACKUP_TLS_CA_FILE"
	// EnvDownloadDst overrides the restore-seed download destination (default
	// /data/dump.rdb).
	EnvDownloadDst = "VALKEY_BACKUP_DOWNLOAD_DST"
	// EnvEngineVersion is the engine version recorded in the manifest.
	EnvEngineVersion = "VALKEY_BACKUP_ENGINE_VERSION"
)

// JobEnvParams is the resolved, non-secret input the controller has after
// storage resolution. The credential VALUES are NOT here — they are mounted into
// the Job from the credentials Secret under the cloud-SDK env names (06 §8.2).
type JobEnvParams struct {
	// Cluster and Backup name the backup-set; they derive every object key.
	Cluster string
	Backup  string
	// Spec is the resolved named storage sub-spec (type + coordinates).
	Spec valkeyv1alpha1.BackupStorageSpec
	// FilesystemRoot is set only for the test-only filesystem backend.
	FilesystemRoot string
	// Mode/CRVersion/Consistency/EngineVersion populate the manifest (backup mode).
	Mode          string
	CRVersion     string
	Consistency   string
	EngineVersion string
	// SeedNode is a reachable Valkey node for CLUSTER NODES scrape (backup mode).
	SeedNode string
	// AuthUser/AuthPasswordEnv configure the engine connection (backup mode). When
	// AuthPasswordEnv is set, the controller also adds a corresponding env entry
	// whose value comes from a Secret key (built outside JobEnv).
	AuthUser string
	// TLSEnabled / TLSCAFile configure engine TLS (backup mode).
	TLSEnabled bool
	TLSCAFile  string
	// DownloadDst overrides the restore-seed write path (download mode).
	DownloadDst string
}

// JobEnv builds the deterministic, sorted-by-key VALKEY_BACKUP_* environment for
// a backup/cleanup/restore Job from already-resolved (non-secret) parameters. It
// emits only the variables relevant to the supplied params (empty values are
// omitted) so the rendered Job env stays minimal and diffable. The credential
// values and any Secret-sourced password env are added by the caller from the
// credentials Secret, never here.
func JobEnv(p JobEnvParams) []EnvVar {
	kv := map[string]string{
		EnvCluster:    p.Cluster,
		EnvBackupName: p.Backup,
	}
	addStorageEnv(kv, p)
	addManifestEnv(kv, p)
	addConnEnv(kv, p)
	if p.DownloadDst != "" {
		kv[EnvDownloadDst] = p.DownloadDst
	}

	out := make([]EnvVar, 0, len(kv))
	for k, v := range kv {
		if v == "" {
			continue
		}
		out = append(out, EnvVar{Name: k, Value: v})
	}
	slices.SortFunc(out, func(a, b EnvVar) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// addStorageEnv sets the backend type + coordinates from the resolved spec.
func addStorageEnv(kv map[string]string, p JobEnvParams) {
	kv[EnvStorageType] = string(p.Spec.Type)
	switch p.Spec.Type {
	case valkeyv1alpha1.BackupStorageS3:
		if p.Spec.S3 != nil {
			kv[EnvS3Bucket] = p.Spec.S3.Bucket
			kv[EnvS3Prefix] = p.Spec.S3.Prefix
			kv[EnvS3Region] = p.Spec.S3.Region
			kv[EnvS3Endpoint] = p.Spec.S3.EndpointURL
		}
	case valkeyv1alpha1.BackupStorageGCS:
		if p.Spec.GCS != nil {
			kv[EnvGCSBucket] = p.Spec.GCS.Bucket
			kv[EnvGCSPrefix] = p.Spec.GCS.Prefix
		}
	case valkeyv1alpha1.BackupStorageAzure:
		if p.Spec.Azure != nil {
			kv[EnvAzureContainer] = p.Spec.Azure.Container
			kv[EnvAzurePrefix] = p.Spec.Azure.Prefix
		}
	case valkeyv1alpha1.BackupStorageFilesystem:
		kv[EnvFSRoot] = p.FilesystemRoot
	}
}

// addManifestEnv sets the manifest-populating fields (backup mode).
func addManifestEnv(kv map[string]string, p JobEnvParams) {
	kv[EnvMode] = p.Mode
	kv[EnvCRVersion] = p.CRVersion
	kv[EnvConsistency] = p.Consistency
	kv[EnvEngineVersion] = p.EngineVersion
}

// addConnEnv sets the engine-connection fields (backup mode). The auth password
// VALUE is added by the caller from a Secret; here we only name the user, the
// seed node, and TLS.
func addConnEnv(kv map[string]string, p JobEnvParams) {
	kv[EnvSeedNode] = p.SeedNode
	kv[EnvAuthUser] = p.AuthUser
	if p.TLSEnabled {
		kv[EnvTLSEnabled] = "true"
		kv[EnvTLSCAFile] = p.TLSCAFile
	}
}

// EnvVar is a minimal name/value pair so pkg/backup stays free of a hard k8s
// core/v1 dependency at this seam; the controllers map it to corev1.EnvVar.
type EnvVar struct {
	Name  string
	Value string
}
