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

package perconavalkeyrestore

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// resolvedSource is the fully-resolved restore source: the storage coordinates
// needed to build an ArtifactStore plus the cluster/backup identity used to locate
// the manifest object key (06 §7.5 step 1). It is hydrated either from a referenced
// PerconaValkeyBackup CR (spec.backupName) or from an inline spec.backupSource.
type resolvedSource struct {
	// Cluster is the SOURCE cluster name (the manifest's clusterName); it forms the
	// object-key prefix together with Backup (keys.go).
	Cluster string
	// Backup is the SOURCE backup name (the manifest's backupName / the prefix
	// component under Cluster).
	Backup string
	// StorageName is echoed for diagnostics.
	StorageName string
	// Config is the resolved StorageConfig the storeFactory consumes. The operator
	// process never populates credential VALUES (06 §8.2); a concrete backend reads
	// them from the Job env. For the FakeStore test seam the values are irrelevant.
	Config backup.StorageConfig
}

// resolveSource enforces the spec one-of (backupName XOR backupSource, also a CEL
// rule) and hydrates a resolvedSource from whichever is set. backupName resolves the
// referenced PerconaValkeyBackup in the same namespace and reads its status
// (destination/storage details); backupSource carries those inline for restoring
// from an artifact whose Backup CR is gone (06 §7.1, mirrors PXC backupSource).
func (r *Reconciler) resolveSource(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore) (resolvedSource, error) {
	hasName := rst.Spec.BackupName != ""
	hasSource := rst.Spec.BackupSource != nil
	if hasName == hasSource {
		return resolvedSource{}, fmt.Errorf("exactly one of spec.backupName or spec.backupSource must be set")
	}
	if hasName {
		return r.resolveFromBackupName(ctx, rst)
	}
	return resolveFromBackupSource(rst.Spec.BackupSource)
}

// resolveFromBackupName loads the referenced PerconaValkeyBackup and hydrates the
// source from its resolved status. The backup must have reached a coverage-complete
// terminal state worth restoring; a New/Running/Failed backup is rejected loudly.
func (r *Reconciler) resolveFromBackupName(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore) (resolvedSource, error) {
	bk := &valkeyv1alpha1.PerconaValkeyBackup{}
	key := types.NamespacedName{Name: rst.Spec.BackupName, Namespace: rst.Namespace}
	if err := r.Get(ctx, key, bk); err != nil {
		return resolvedSource{}, fmt.Errorf("get referenced backup %s: %w", key, err)
	}
	if bk.Status.Destination == "" {
		return resolvedSource{}, fmt.Errorf(
			"referenced backup %s has no resolved destination (state %q) — not yet a restorable set",
			key, bk.Status.State)
	}
	src := &valkeyv1alpha1.BackupSource{
		Destination: bk.Status.Destination,
		StorageName: bk.Status.StorageName,
		S3:          bk.Status.S3,
		GCS:         bk.Status.GCS,
		Azure:       bk.Status.Azure,
	}
	return sourceFromDestination(bk.Spec.ClusterName, bk.Name, src)
}

// resolveFromBackupSource hydrates the source from an inline spec.backupSource
// (valkeyv1alpha1.BackupSource), deriving the cluster/backup identity from the
// destination prefix.
func resolveFromBackupSource(bs *valkeyv1alpha1.BackupSource) (resolvedSource, error) {
	cluster, bkName, err := destinationIdentity(bs.Destination)
	if err != nil {
		return resolvedSource{}, err
	}
	return sourceFromDestination(cluster, bkName, bs)
}

// sourceFromDestination builds a resolvedSource from a destination string + storage
// details, deriving the StorageConfig from the destination prefix so the manifest
// object key is correctly rooted (keys.go).
func sourceFromDestination(clusterHint, backupHint string, bs *valkeyv1alpha1.BackupSource) (resolvedSource, error) {
	dest, err := backup.ParseDestination(bs.Destination)
	if err != nil {
		return resolvedSource{}, fmt.Errorf("parse backup destination: %w", err)
	}
	cfg, err := storageConfigFromSource(bs, dest)
	if err != nil {
		return resolvedSource{}, err
	}
	return resolvedSource{
		Cluster:     clusterHint,
		Backup:      backupHint,
		StorageName: bs.StorageName,
		Config:      cfg,
	}, nil
}

// destinationIdentity derives the (cluster, backup) identity from a destination
// path. The object layout is "<prefix>/<cluster>/<backup>" (keys.go SetPrefix);
// the last two path components are the cluster and backup names.
func destinationIdentity(destination string) (cluster, backupName string, err error) {
	dest, perr := backup.ParseDestination(destination)
	if perr != nil {
		return "", "", fmt.Errorf("parse backup destination: %w", perr)
	}
	cluster, backupName, ok := lastTwoPathComponents(dest.Path)
	if !ok {
		return "", "", fmt.Errorf("backup destination %q lacks a <cluster>/<backup> suffix", destination)
	}
	return cluster, backupName, nil
}

// storageConfigFromSource maps the inline/hydrated source onto a backup.StorageConfig
// the storeFactory consumes, dispatching on the destination scheme. Credential
// VALUES are intentionally absent (the operator never authenticates — 06 §8.2);
// the FakeStore test seam ignores them and a concrete backend reads them from env.
func storageConfigFromSource(src *valkeyv1alpha1.BackupSource, dest backup.Destination) (backup.StorageConfig, error) {
	switch dest.Scheme {
	case backup.SchemeS3:
		return backup.StorageConfig{Type: valkeyv1alpha1.BackupStorageS3, S3: src.S3}, nil
	case backup.SchemeGCS:
		return backup.StorageConfig{Type: valkeyv1alpha1.BackupStorageGCS, GCS: src.GCS}, nil
	case backup.SchemeAzure:
		return backup.StorageConfig{Type: valkeyv1alpha1.BackupStorageAzure, Azure: src.Azure}, nil
	case backup.SchemePVC:
		return backup.StorageConfig{Type: valkeyv1alpha1.BackupStorageFilesystem, FilesystemRoot: dest.Path}, nil
	default:
		return backup.StorageConfig{}, fmt.Errorf("unsupported backup destination scheme %q", dest.Scheme)
	}
}

// readManifest builds an ArtifactStore for the resolved source and reads the
// manifest object — the FIRST artifact a restore touches (06 §7.5). A missing
// manifest surfaces as a wrapped backup.ErrNotExist so the caller fails the restore
// cleanly (the set is incomplete / never existed).
func (r *Reconciler) readManifest(ctx context.Context, src resolvedSource, namespace string) (backup.Manifest, error) {
	cfg := src.Config
	creds, err := r.loadStorageCreds(ctx, namespace, cfg)
	if err != nil {
		return backup.Manifest{}, err
	}
	cfg.Credentials = creds
	store, err := r.storeFactory(ctx, cfg)
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("build artifact store: %w", err)
	}
	key := backup.ManifestKey(src.Cluster, src.Backup)
	man, err := backup.ReadManifest(ctx, store, key)
	if err != nil {
		if errors.Is(err, backup.ErrNotExist) {
			return backup.Manifest{}, fmt.Errorf("backup manifest %q not found — the backup-set is incomplete or was deleted: %w", key, err)
		}
		return backup.Manifest{}, fmt.Errorf("read backup manifest %q: %w", key, err)
	}
	return man, nil
}

// loadStorageCreds reads the storage credentials Secret named in the resolved
// StorageConfig into a name->value map the ArtifactStore backend authenticates
// with. Unlike the seed init container (which mounts the Secret as pod env), the
// OPERATOR reads the manifest in-process and must load the credential values itself
// — without them a cloud backend falls back to the SDK default chain (e.g. EC2 IMDS)
// and the manifest GetObject fails. Returns nil (defer to the SDK default chain)
// when no Secret is named (e.g. the test-only filesystem backend, or IRSA/role
// credentials). The values are held only for the immediate store call (06 §8.2).
func (r *Reconciler) loadStorageCreds(
	ctx context.Context, namespace string, cfg backup.StorageConfig,
) (map[string]string, error) {
	name := credentialsSecretName(cfg)
	if name == "" {
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		return nil, fmt.Errorf("get credentialsSecret %q: %w", name, err)
	}
	creds := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		creds[k] = string(v)
	}
	return creds, nil
}

// credentialsSecretName returns the credentials Secret named by the resolved
// storage sub-spec, or "" when none applies (filesystem / unset).
func credentialsSecretName(cfg backup.StorageConfig) string {
	switch cfg.Type {
	case valkeyv1alpha1.BackupStorageS3:
		if cfg.S3 != nil {
			return cfg.S3.CredentialsSecret
		}
	case valkeyv1alpha1.BackupStorageGCS:
		if cfg.GCS != nil {
			return cfg.GCS.CredentialsSecret
		}
	case valkeyv1alpha1.BackupStorageAzure:
		if cfg.Azure != nil {
			return cfg.Azure.CredentialsSecret
		}
	}
	return ""
}
