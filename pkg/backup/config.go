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
	"context"
	"fmt"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// StorageConfig is the fully-resolved input to a backend constructor: the named
// storage spec from cluster.spec.backup.storages[name] plus the credential
// VALUES read from the referenced Secret. The operator process resolves the
// storage by name and only checks key-presence on the Secret (06 §8.2); the
// credential values themselves are consumed inside the backup/cleanup/restore Job
// (which mounts the Secret as env), and that Job builds the StorageConfig and
// calls NewStore. Credentials are NEVER copied into CR status or logs.
type StorageConfig struct {
	// Type is the backend type (s3/gcs/azure/filesystem). It selects the backend
	// constructor in newBackend.
	Type valkeyv1alpha1.BackupStorageType
	// S3 is the resolved S3 storage spec when Type == s3.
	S3 *valkeyv1alpha1.BackupStorageS3Spec
	// GCS is the resolved GCS storage spec when Type == gcs.
	GCS *valkeyv1alpha1.BackupStorageGCSSpec
	// Azure is the resolved Azure storage spec when Type == azure.
	Azure *valkeyv1alpha1.BackupStorageAzureSpec
	// Credentials is the credential VALUES read from the credentialsSecret, keyed
	// by env-var name (AWS_ACCESS_KEY_ID, AZURE_STORAGE_KEY, ...) — or, for GCS,
	// the service-account JSON under GOOGLE_APPLICATION_CREDENTIALS_JSON. Populated
	// only inside the Job; empty in the operator process (presence-checked, never
	// used to authenticate).
	Credentials map[string]string
	// FilesystemRoot is the on-disk base directory for the test-only filesystem
	// backend (pvc/). Ignored for cloud backends.
	FilesystemRoot string
}

// StorageConfigFromSpec builds a StorageConfig from a named BackupStorageSpec and
// already-resolved credential values. It is the single place that flattens the
// discriminated-union spec into the constructor input, so every caller (backup
// Job, cleanup Job, restore download) resolves storage identically.
func StorageConfigFromSpec(spec valkeyv1alpha1.BackupStorageSpec, creds map[string]string) StorageConfig {
	return StorageConfig{
		Type:        spec.Type,
		S3:          spec.S3,
		GCS:         spec.GCS,
		Azure:       spec.Azure,
		Credentials: creds,
	}
}

// backendConstructor builds a concrete ArtifactStore from a StorageConfig. The
// cloud-backend leg (GO-4.2/GO-4.3) registers one per BackupStorageType via
// RegisterBackend; this foundation registers only the test-only filesystem
// backend stub below so NewStore is callable end-to-end before the cloud backends
// land.
type backendConstructor func(ctx context.Context, cfg StorageConfig) (ArtifactStore, error)

// backendRegistry maps a storage type to its constructor. It is the constructor
// SEAM the cloud-backend leg fills: each backend calls RegisterBackend in an
// init() (or the leg wires a switch here) so NewStore dispatches without
// pkg/backup importing the cloud SDKs at the seam level.
var backendRegistry = map[valkeyv1alpha1.BackupStorageType]backendConstructor{}

// RegisterBackend registers a constructor for a storage type. It is called by the
// concrete backend files (s3.go/gcs.go/azure.go) — typically from an init() — so
// NewStore can dispatch on cfg.Type. Re-registering a type overwrites the prior
// constructor (last registration wins); the cloud-backend leg owns these calls.
func RegisterBackend(t valkeyv1alpha1.BackupStorageType, ctor backendConstructor) {
	backendRegistry[t] = ctor
}

// NewStore is the single entry point that maps a resolved StorageConfig to a
// concrete ArtifactStore backend (the repository-pattern factory, 06 §8.1). It
// dispatches on cfg.Type through the backendRegistry the cloud-backend leg fills.
// It returns a clear error for an unregistered/unknown type so a backend that has
// not yet been wired (S3/GCS/Azure land in GO-4.2/GO-4.3, filesystem fsStore in
// GO-4.3) fails loudly rather than nil-deref'ing.
func NewStore(ctx context.Context, cfg StorageConfig) (ArtifactStore, error) {
	ctor, ok := backendRegistry[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("backup: no storage backend registered for type %q (S3/GCS/Azure/filesystem land in GO-4.2/4.3)", cfg.Type)
	}
	store, err := ctor(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("backup: construct %q store: %w", cfg.Type, err)
	}
	return store, nil
}

// BackendRegistered reports whether a constructor is registered for type t. It
// lets the controller fail-fast at CheckNSetDefaults time if a backend has not
// been wired, rather than at Job runtime.
func BackendRegistered(t valkeyv1alpha1.BackupStorageType) bool {
	_, ok := backendRegistry[t]
	return ok
}
