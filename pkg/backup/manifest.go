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
	"encoding/json"
	"fmt"
)

// ManifestSchemaVersion is the on-disk manifest schema version. Bump it when the
// Manifest shape changes in a backwards-incompatible way so restore can
// reject manifests it does not understand. See
// docs/architecture/06-backup-restore.md §4.5.
const ManifestSchemaVersion = 1

// ManifestAPIVersion is the apiVersion stamped into every manifest. It echoes the
// CRD group/version so a manifest is self-describing (06 §4.5).
const ManifestAPIVersion = "valkey.percona.com/v1alpha1"

// ManifestFilename is the object name (relative to the backup-set destination
// root) of the authoritative backup-set descriptor. It is written LAST on create
// and deleted FIRST on teardown (06 §4.5, §6.1).
const ManifestFilename = "manifest.json"

// Manifest is the authoritative backup-set descriptor written to object
// storage as manifest.json at the destination root. Its presence is the durable
// "backup-set is complete" marker: the backup Job uploads every shard RDB FIRST
// and writes the manifest LAST (06 §4.5), and the cleanup Job deletes the
// manifest FIRST (06 §6.1). A backup-set without a manifest is by definition
// incomplete and is itself a GC candidate.
//
// The controller copies the coverage/version fields into
// PerconaValkeyBackup.status; restore reads the manifest first and reproduces the
// exact slot map recorded here (06 §7.5).
type Manifest struct {
	// SchemaVersion is the manifest schema version (ManifestSchemaVersion).
	SchemaVersion int `json:"schemaVersion"`
	// APIVersion echoes the CRD group/version (ManifestAPIVersion).
	APIVersion string `json:"apiVersion"`
	// Cluster is the source PerconaValkeyCluster name.
	Cluster string `json:"clusterName"`
	// BackupName is the source PerconaValkeyBackup name.
	BackupName string `json:"backupName"`
	// Mode is the source cluster topology mode (cluster/replication).
	Mode string `json:"mode"`
	// CRVersion is the source cluster spec.crVersion, recorded for restore-time
	// compatibility gating.
	CRVersion string `json:"crVersion,omitempty"`
	// EngineVersion is the Valkey engine version captured at backup time
	// (INFO server: valkey_version), used by restore for compatibility checks.
	EngineVersion string `json:"engineVersion"`
	// CreatedAt is the RFC3339 UTC manifest-write timestamp.
	CreatedAt string `json:"createdAt"`
	// Consistency is the snapshot-coordination mode (strict | best-effort).
	Consistency string `json:"consistency"`
	// SlotCoverage is the union-coverage verdict (complete | partial). A backup is
	// Succeeded only at complete coverage (06 §4.4).
	SlotCoverage string `json:"slotCoverage"`
	// Shards are the per-shard fragments, ascending by ShardIndex (deterministic,
	// 06 §4.4).
	Shards []ShardManifest `json:"shards"`
}

// ShardManifest is one shard's fragment of a Manifest: where its RDB lives,
// its integrity metadata, the slots it owned at snapshot time, and the
// forward-compat PITR anchor. See docs/architecture/06-backup-restore.md §4.5.
type ShardManifest struct {
	// Index is the zero-based shard index. Restore maps this index to the target
	// shard's primary; the SourcePrimaryNodeID below is meaningless in a fresh
	// target cluster (06 §7.4 step 2).
	Index int `json:"index"`
	// PrimaryNodeID is the SOURCE cluster's live-primary node ID at snapshot time
	// (recorded for provenance/diagnostics; NOT used to address the target — see
	// 06 §7.4 step 2).
	PrimaryNodeID string `json:"primaryNodeID"`
	// SlotRanges is the shard's assigned slot ranges in CLUSTER NODES form
	// (e.g. "0-5460" or "0-100,500-600"). Restore reproduces this exact map via
	// CLUSTER ADDSLOTSRANGE (06 §7.5.2).
	SlotRanges string `json:"slotRanges"`
	// RDBKey is the object key of this shard's RDB relative to the destination
	// root (e.g. "shard-0/dump.rdb").
	RDBKey string `json:"rdbKey"`
	// SizeBytes is the uploaded RDB object size in bytes.
	SizeBytes int64 `json:"sizeBytes"`
	// SHA256 is the hex-encoded SHA-256 of the RDB stream, verified on restore
	// download (06 §9.3).
	SHA256 string `json:"sha256"`
	// MasterReplOffset is master_repl_offset at BGSAVE issuance. It is a
	// forward-compat anchor for the DEFERRED PITR design (06 §11) and is unused by
	// the v1alpha1 snapshot/restore path.
	MasterReplOffset int64 `json:"masterReplOffset,omitempty"`
}

// MarshalManifest serialises a Manifest to indented JSON for upload. It
// stamps SchemaVersion and APIVersion if unset so callers cannot forget them.
func MarshalManifest(m Manifest) ([]byte, error) {
	if m.SchemaVersion == 0 {
		m.SchemaVersion = ManifestSchemaVersion
	}
	if m.APIVersion == "" {
		m.APIVersion = ManifestAPIVersion
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal backup manifest: %w", err)
	}
	return data, nil
}

// UnmarshalManifest parses manifest.json bytes into a Manifest and validates
// the schema version is one this build understands. Restore calls this first and
// fails loudly on an unknown/newer schema rather than silently mis-reading it.
func UnmarshalManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("unmarshal backup manifest: %w", err)
	}
	if m.SchemaVersion > ManifestSchemaVersion {
		return Manifest{}, fmt.Errorf(
			"unsupported manifest schemaVersion %d (this build understands up to %d)",
			m.SchemaVersion, ManifestSchemaVersion)
	}
	return m, nil
}
