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

package perconavalkeybackup

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// resolvedStorage carries the named storage spec plus the credentials Secret
// name extracted from it, so the Job builder and the destination formatter share
// one resolution. credValues is never populated by the operator process — the
// operator only presence-checks the Secret keys (06 §8.2); the credential values
// are mounted into the Job from the Secret.
type resolvedStorage struct {
	name        string
	spec        valkeyv1alpha1.BackupStorageSpec
	credsSecret string
	destination string
}

// requiredCredKeys returns the Secret keys a backend's credentials Secret must
// carry (06 §3.4, §8.2). The operator presence-checks exactly these and never
// reads their values.
func requiredCredKeys(t valkeyv1alpha1.BackupStorageType) []string {
	switch t {
	case valkeyv1alpha1.BackupStorageS3:
		return []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
	case valkeyv1alpha1.BackupStorageGCS:
		// The service-account JSON key, mounted as a file in the Job.
		return []string{"GOOGLE_APPLICATION_CREDENTIALS_JSON"}
	case valkeyv1alpha1.BackupStorageAzure:
		return []string{"AZURE_STORAGE_ACCOUNT", "AZURE_STORAGE_KEY"}
	default:
		// filesystem (test-only) needs no credentials.
		return nil
	}
}

// checkNSetDefaults resolves the backup's storageName into the source cluster's
// spec.backup.storages, hydrates status (storageName, destination, the resolved
// storage sub-spec), and validates the named credentials Secret exists with the
// expected keys. It fails loudly (no fallback) on a typo or missing Secret — the
// classic Percona "storage name mismatch silently fails at execution" trap
// (06 §8.2). It is the ONLY place the operator touches the credentials Secret,
// and it never reads the values.
func (r *Reconciler) checkNSetDefaults(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) (*resolvedStorage, error) {
	if bk.Spec.ClusterName == "" {
		return nil, fmt.Errorf("spec.clusterName is required")
	}
	if bk.Spec.StorageName == "" {
		return nil, fmt.Errorf("spec.storageName is required")
	}

	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	key := types.NamespacedName{Name: bk.Spec.ClusterName, Namespace: bk.Namespace}
	if err := r.Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("cluster %q not found", bk.Spec.ClusterName)
		}
		return nil, fmt.Errorf("get cluster %q: %w", bk.Spec.ClusterName, err)
	}

	rs, err := resolveStorage(cluster, bk)
	if err != nil {
		return nil, err
	}

	if !backup.BackendRegistered(rs.spec.Type) {
		return nil, fmt.Errorf("no storage backend wired for type %q (S3/GCS/Azure land in GO-4.2/4.3)", rs.spec.Type)
	}

	if err := r.validateCredsSecret(ctx, bk.Namespace, rs); err != nil {
		return nil, err
	}

	// Hydrate status so restore is self-contained and the printer columns populate.
	bk.Status.StorageName = rs.name
	bk.Status.Destination = rs.destination
	bk.Status.S3 = rs.spec.S3
	bk.Status.GCS = rs.spec.GCS
	bk.Status.Azure = rs.spec.Azure
	return rs, nil
}

// resolveStorage looks up the named storage in cluster.spec.backup.storages,
// extracts its credentials-Secret name, and computes the backend-prefixed
// destination root for this backup-set. No fallback: a missing key is an error.
func resolveStorage(cluster *valkeyv1alpha1.PerconaValkeyCluster, bk *valkeyv1alpha1.PerconaValkeyBackup) (*resolvedStorage, error) {
	storages := cluster.Spec.Backup.Storages
	spec, ok := storages[bk.Spec.StorageName]
	if !ok {
		return nil, fmt.Errorf("storageName %q not found in cluster %q spec.backup.storages", bk.Spec.StorageName, cluster.Name)
	}
	rs := &resolvedStorage{name: bk.Spec.StorageName, spec: spec}

	scheme, bucketOrContainer, prefix, credsSecret, err := storageCoordinates(spec)
	if err != nil {
		return nil, err
	}
	rs.credsSecret = credsSecret
	rs.destination = backup.FormatDestination(scheme, bucketOrContainer, prefix, cluster.Name, bk.Name)
	return rs, nil
}

// storageCoordinates flattens the discriminated-union storage spec into the
// scheme/bucket/prefix/credsSecret tuple the destination formatter and Job
// builder need. It validates the sub-object matching the declared type is set.
func storageCoordinates(spec valkeyv1alpha1.BackupStorageSpec) (scheme, bucketOrContainer, prefix, credsSecret string, err error) {
	switch spec.Type {
	case valkeyv1alpha1.BackupStorageS3:
		if spec.S3 == nil {
			return "", "", "", "", fmt.Errorf("storage type s3 requires the s3 sub-object")
		}
		return backup.SchemeS3, spec.S3.Bucket, spec.S3.Prefix, spec.S3.CredentialsSecret, nil
	case valkeyv1alpha1.BackupStorageGCS:
		if spec.GCS == nil {
			return "", "", "", "", fmt.Errorf("storage type gcs requires the gcs sub-object")
		}
		return backup.SchemeGCS, spec.GCS.Bucket, spec.GCS.Prefix, spec.GCS.CredentialsSecret, nil
	case valkeyv1alpha1.BackupStorageAzure:
		if spec.Azure == nil {
			return "", "", "", "", fmt.Errorf("storage type azure requires the azure sub-object")
		}
		return backup.SchemeAzure, spec.Azure.Container, spec.Azure.Prefix, spec.Azure.CredentialsSecret, nil
	case valkeyv1alpha1.BackupStorageFilesystem:
		// test-only: no bucket, no credentials.
		return backup.SchemePVC, "", "", "", nil
	default:
		return "", "", "", "", fmt.Errorf("unknown storage type %q", spec.Type)
	}
}

// validateCredsSecret confirms the credentials Secret named by the resolved
// storage exists and carries the keys the backend expects. It is a presence
// check only — the operator never reads the credential VALUES (06 §8.2). The
// test-only filesystem backend needs no Secret.
func (r *Reconciler) validateCredsSecret(ctx context.Context, namespace string, rs *resolvedStorage) error {
	required := requiredCredKeys(rs.spec.Type)
	if len(required) == 0 {
		return nil
	}
	if rs.credsSecret == "" {
		return fmt.Errorf("storage %q (type %s) requires a credentialsSecret", rs.name, rs.spec.Type)
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: rs.credsSecret, Namespace: namespace}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("credentialsSecret %q for storage %q not found", rs.credsSecret, rs.name)
		}
		return fmt.Errorf("get credentialsSecret %q: %w", rs.credsSecret, err)
	}
	var missing []string
	for _, k := range required {
		if _, ok := secret.Data[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("credentialsSecret %q for storage %q is missing key(s) %v", rs.credsSecret, rs.name, missing)
	}
	return nil
}
