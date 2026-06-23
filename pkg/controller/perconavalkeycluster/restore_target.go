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
	"strings"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// This file is the restore-target SEAM (CR-8 / 06 §7.4). The PerconaValkeyRestore
// controller stamps restore markers onto its target cluster (the restored-from
// annotation + the appendonly-no seed override, see perconavalkeyrestore/provision.go);
// when those markers are present the cluster controller must propagate a per-node
// spec.restoreFrom onto each ValkeyNode so the node controller injects the
// restore-seed init container (valkeynode_resources.go buildRestoreSeedContainer)
// that downloads the shard RDB before the engine boots.
//
// The FOUNDATION provides the marker predicate + the per-node accessor
// (restoreSourceForNode) that buildValkeyNodeSpec calls, plus the marker-annotation
// key bridged from the restore leg. The restore LEG fills the body that resolves the
// concrete RestoreSource (named storage + backup name + this node's shard index)
// from the restore intent, without touching the FOUNDATION's files.

// annRestoreMarker is the restored-from marker the PerconaValkeyRestore controller
// stamps onto the target cluster (mirrors perconavalkeyrestore/status.go
// annRestoreMarker). Its presence tells the cluster controller this cluster is a
// restore target so each node seeds its shard RDB before booting.
const annRestoreMarker = "valkey.percona.com/restored-from"

// annRestoreStorage is the resolved named storage backend the restore controller
// stamps onto the target cluster (mirrors perconavalkeyrestore/status.go
// annRestoreStorage). It names which spec.backup.storages key the shard RDBs are
// downloaded from; the seam reads it into the per-node RestoreSource.Storage so the
// seed init container resolves the backend. Absent when the restore resolved from an
// inline backupSource with no named storage — the node then falls back to the
// cluster's own resolved backup storage.
const annRestoreStorage = "valkey.percona.com/restore-storage"

// annSourceCluster / annSourceBackup carry the SOURCE cluster and backup-set names
// (the manifest's clusterName/backupName) the shard RDB object keys are derived from.
// The restored-from provenance marker does not preserve them (it is "backupSource"
// for an inline source), so the seam reads these to render the per-node
// RestoreSource.ClusterName / .BackupName the seed init container needs to build the
// VALKEY_BACKUP_CLUSTER/VALKEY_BACKUP_NAME object-key env.
//
// MUST stay byte-identical to the writer in perconavalkeyrestore/status.go
// (annSourceCluster / annSourceBackup): the two packages have no import relationship,
// so a drift here silently yields an empty ClusterName on the seed (download cannot
// resolve the object key). Mirrors the established annRestoreMarker/annRestoreStorage
// cross-package marker convention.
const (
	annSourceCluster = "valkey.percona.com/restore-source-cluster"
	annSourceBackup  = "valkey.percona.com/restore-source-backup"
)

// isRestoreTarget reports whether this cluster carries the restored-from marker
// (so the node controller must seed each shard's RDB before the engine boots).
func isRestoreTarget(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	if cluster == nil || cluster.Annotations == nil {
		return false
	}
	_, ok := cluster.Annotations[annRestoreMarker]
	return ok
}

// restoreSourceForNode returns the per-node restore-seed source to stamp onto a
// node's spec.restoreFrom for a (shard, node) position, or nil when this cluster is
// not a restore target. Only a primary (node index 0) seeds its shard's RDB — a
// replica syncs from its primary after the topology re-forms (06 §7.5), so a
// non-primary node never carries a restoreFrom. The per-node marker is derived from
// the restore markers the restore controller stamps: the backup-set name (parsed from
// the restored-from value), this node's ShardIndex (key.shard), and the named storage
// backend (from the restore-storage marker). The Storage falls back to the cluster's
// own resolved backup storage when the restore stamped no named storage (an inline
// backupSource), via storageForRestore. Returns nil (no seed container) for a
// non-restore cluster, keeping the seam inert by default.
func restoreSourceForNode(cluster *valkeyv1alpha1.PerconaValkeyCluster, key nodeKey) *valkeyv1alpha1.RestoreSource {
	if !isRestoreTarget(cluster) {
		return nil
	}
	if key.node != 0 {
		// Replicas re-sync from the seeded primary; only the primary seeds the RDB.
		return nil
	}
	storageName := storageForRestore(cluster)
	// Prefer the explicit source-backup marker (set for both backupName and inline
	// backupSource restores); fall back to parsing the provenance marker for older
	// targets stamped before the source markers existed.
	backupName := cluster.Annotations[annSourceBackup]
	if backupName == "" {
		backupName = backupNameFromMarker(cluster.Annotations[annRestoreMarker])
	}
	rs := &valkeyv1alpha1.RestoreSource{
		Storage:     storageName,
		BackupName:  backupName,
		ShardIndex:  int32(key.shard), //#nosec G115 -- bounded shard index, cannot overflow int32
		ClusterName: cluster.Annotations[annSourceCluster],
	}
	// Resolve the named storage's full coordinates + credentials Secret from the
	// cluster's own backup block so the node controller can render the VALKEY_BACKUP_*
	// storage env + EnvFrom creds on the seed init container (it has no access to the
	// cluster's backup storages otherwise — CR-8 / 06 §7.4, §8.2).
	if storageName != "" {
		if spec, ok := cluster.Spec.Backup.Storages[storageName]; ok {
			specCopy := spec
			rs.StorageSpec = &specCopy
			rs.CredentialsSecret = credentialsSecretForStorage(spec)
		}
	}
	return rs
}

// credentialsSecretForStorage returns the object-store credentials Secret named by a
// resolved storage sub-spec (the cloud-SDK-env Secret the seed init container mounts
// via EnvFrom). The test-only filesystem backend needs none.
func credentialsSecretForStorage(spec valkeyv1alpha1.BackupStorageSpec) string {
	switch spec.Type {
	case valkeyv1alpha1.BackupStorageS3:
		if spec.S3 != nil {
			return spec.S3.CredentialsSecret
		}
	case valkeyv1alpha1.BackupStorageGCS:
		if spec.GCS != nil {
			return spec.GCS.CredentialsSecret
		}
	case valkeyv1alpha1.BackupStorageAzure:
		if spec.Azure != nil {
			return spec.Azure.CredentialsSecret
		}
	}
	return ""
}

// storageForRestore resolves the named backup-storage backend the restore-seed init
// container downloads each shard's RDB from. It prefers the restore-storage marker
// the restore controller stamped (the named spec.backup.storages key the source
// backup used); when that marker is absent (an inline backupSource carried no
// storageName) it falls back to the cluster's own backup block — the sole configured
// storage when there is exactly one, so the unambiguous case still resolves a backend
// without operator input. An empty result means the seed env builder must resolve the
// storage from the cluster's resolved backup block at pod-build time.
func storageForRestore(cluster *valkeyv1alpha1.PerconaValkeyCluster) string {
	if name := cluster.Annotations[annRestoreStorage]; name != "" {
		return name
	}
	if storages := cluster.Spec.Backup.Storages; len(storages) == 1 {
		for name := range storages {
			return name
		}
	}
	return ""
}

// backupNameFromMarker extracts the backup-set name from the restored-from marker
// value, whose shape is "<restoreName>/<backupName>" (or "<restoreName>/backupSource"
// for an inline source — in which case "" is returned and the LEG resolves the
// backup-set from the inline source). It is the FOUNDATION's read of the provenance
// string the restore controller stamps (perconavalkeyrestore restoreMarkerValue).
func backupNameFromMarker(marker string) string {
	idx := strings.LastIndex(marker, "/")
	if idx < 0 || idx+1 >= len(marker) {
		return ""
	}
	name := marker[idx+1:]
	if name == "backupSource" {
		return ""
	}
	return name
}
