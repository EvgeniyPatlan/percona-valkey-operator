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
	"slices"
	"strings"
	"testing"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// envMap collapses the ordered JobEnv result into a lookup map for assertions.
func envMap(t *testing.T, kvs []EnvVar) map[string]string {
	t.Helper()
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if _, dup := m[kv.Name]; dup {
			t.Fatalf("duplicate env key %q in %+v", kv.Name, kvs)
		}
		m[kv.Name] = kv.Value
	}
	return m
}

func TestJobEnvS3Backup(t *testing.T) {
	t.Parallel()
	kvs := JobEnv(JobEnvParams{
		Cluster: "prod",
		Backup:  "prod-20260622",
		Spec: valkeyv1alpha1.BackupStorageSpec{
			Type: valkeyv1alpha1.BackupStorageS3,
			S3: &valkeyv1alpha1.BackupStorageS3Spec{
				Bucket:      "bkt",
				Prefix:      "p/",
				Region:      "eu-central-1",
				EndpointURL: "https://minio:9000",
			},
		},
		Mode:          "cluster",
		CRVersion:     "1.0",
		Consistency:   "strict",
		EngineVersion: "9.0.0",
		SeedNode:      "valkey-prod:6379",
		AuthUser:      "_operator",
		TLSEnabled:    true,
		TLSCAFile:     "/etc/valkey/tls/ca.crt",
	})

	// JobEnv must be sorted by key (deterministic, diffable Job specs).
	if !sortedByName(kvs) {
		t.Fatalf("JobEnv result is not sorted by name: %+v", kvs)
	}

	env := envMap(t, kvs)
	want := map[string]string{
		EnvCluster:       "prod",
		EnvBackupName:    "prod-20260622",
		EnvStorageType:   "s3",
		EnvS3Bucket:      "bkt",
		EnvS3Prefix:      "p/",
		EnvS3Region:      "eu-central-1",
		EnvS3Endpoint:    "https://minio:9000",
		EnvMode:          "cluster",
		EnvCRVersion:     "1.0",
		EnvConsistency:   "strict",
		EnvEngineVersion: "9.0.0",
		EnvSeedNode:      "valkey-prod:6379",
		EnvAuthUser:      "_operator",
		EnvTLSEnabled:    "true",
		EnvTLSCAFile:     "/etc/valkey/tls/ca.crt",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
	// No stray GCS/Azure/FS keys leaked for an S3 backend.
	for _, k := range []string{EnvGCSBucket, EnvAzureContainer, EnvFSRoot, EnvDownloadDst} {
		if _, ok := env[k]; ok {
			t.Errorf("unexpected env key %s for s3 backend", k)
		}
	}
}

func TestJobEnvOmitsEmpty(t *testing.T) {
	t.Parallel()
	// Minimal params: no TLS, no manifest fields, no seed/auth. Only the keys with
	// non-empty values are emitted (TLS off => no TLS keys at all).
	kvs := JobEnv(JobEnvParams{
		Cluster: "c",
		Backup:  "b",
		Spec: valkeyv1alpha1.BackupStorageSpec{
			Type: valkeyv1alpha1.BackupStorageS3,
			S3:   &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "only-bucket"},
		},
	})
	env := envMap(t, kvs)
	if env[EnvCluster] != "c" || env[EnvBackupName] != "b" || env[EnvS3Bucket] != "only-bucket" {
		t.Fatalf("core keys missing: %v", env)
	}
	// TLS off => neither TLS key present.
	if _, ok := env[EnvTLSEnabled]; ok {
		t.Errorf("TLS-off must not emit %s", EnvTLSEnabled)
	}
	if _, ok := env[EnvTLSCAFile]; ok {
		t.Errorf("TLS-off must not emit %s", EnvTLSCAFile)
	}
	// Empty manifest/conn fields are omitted, not emitted blank.
	for _, k := range []string{EnvMode, EnvCRVersion, EnvConsistency, EnvEngineVersion, EnvSeedNode, EnvAuthUser, EnvS3Prefix, EnvS3Region, EnvS3Endpoint} {
		if _, ok := env[k]; ok {
			t.Errorf("empty field must be omitted, but %s present = %q", k, env[k])
		}
	}
}

func TestJobEnvGCS(t *testing.T) {
	t.Parallel()
	kvs := JobEnv(JobEnvParams{
		Cluster: "c", Backup: "b",
		Spec: valkeyv1alpha1.BackupStorageSpec{
			Type: valkeyv1alpha1.BackupStorageGCS,
			GCS:  &valkeyv1alpha1.BackupStorageGCSSpec{Bucket: "gbkt", Prefix: "g/"},
		},
	})
	env := envMap(t, kvs)
	if env[EnvStorageType] != "gcs" || env[EnvGCSBucket] != "gbkt" || env[EnvGCSPrefix] != "g/" {
		t.Fatalf("gcs env = %v", env)
	}
	if _, ok := env[EnvS3Bucket]; ok {
		t.Errorf("s3 key leaked for gcs backend")
	}
}

func TestJobEnvAzure(t *testing.T) {
	t.Parallel()
	kvs := JobEnv(JobEnvParams{
		Cluster: "c", Backup: "b",
		Spec: valkeyv1alpha1.BackupStorageSpec{
			Type:  valkeyv1alpha1.BackupStorageAzure,
			Azure: &valkeyv1alpha1.BackupStorageAzureSpec{Container: "ctr", Prefix: "a/"},
		},
	})
	env := envMap(t, kvs)
	if env[EnvStorageType] != "azure" || env[EnvAzureContainer] != "ctr" || env[EnvAzurePrefix] != "a/" {
		t.Fatalf("azure env = %v", env)
	}
}

func TestJobEnvFilesystemAndDownloadDst(t *testing.T) {
	t.Parallel()
	kvs := JobEnv(JobEnvParams{
		Cluster: "c", Backup: "b",
		Spec:           valkeyv1alpha1.BackupStorageSpec{Type: valkeyv1alpha1.BackupStorageFilesystem},
		FilesystemRoot: "/tmp/vk",
		DownloadDst:    "/data/dump.rdb",
	})
	env := envMap(t, kvs)
	if env[EnvStorageType] != "filesystem" || env[EnvFSRoot] != "/tmp/vk" {
		t.Fatalf("fs env = %v", env)
	}
	if env[EnvDownloadDst] != "/data/dump.rdb" {
		t.Fatalf("download dst not set: %v", env)
	}
}

func TestJobEnvNilSubSpecNoPanic(t *testing.T) {
	t.Parallel()
	// A declared type with a nil sub-spec must not panic; it emits only the type.
	kvs := JobEnv(JobEnvParams{
		Cluster: "c", Backup: "b",
		Spec: valkeyv1alpha1.BackupStorageSpec{Type: valkeyv1alpha1.BackupStorageS3}, // S3 nil
	})
	env := envMap(t, kvs)
	if env[EnvStorageType] != "s3" {
		t.Fatalf("type must still be emitted with a nil sub-spec: %v", env)
	}
	if _, ok := env[EnvS3Bucket]; ok {
		t.Errorf("nil S3 sub-spec must not emit a bucket key")
	}
}

// sortedByName reports whether the env slice is ascending by Name.
func sortedByName(kvs []EnvVar) bool {
	return slices.IsSortedFunc(kvs, func(a, b EnvVar) int { return strings.Compare(a.Name, b.Name) })
}
