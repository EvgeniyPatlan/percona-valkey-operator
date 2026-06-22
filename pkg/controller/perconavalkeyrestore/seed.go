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
	"fmt"

	corev1 "k8s.io/api/core/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// seedDataDir is the data directory the engine loads dump.rdb from on boot; the
// restore init container writes the downloaded RDB here BEFORE the Valkey process
// starts (06 §7.4 step 1).
const seedDataDir = "/data"

// seedDataVolume is the name of the data volume shared between the seed init
// container and the engine container.
const seedDataVolume = "data"

// seedContainerName is the name of the per-pod restore init container.
const seedContainerName = "restore-seed"

// restoreInitContainer builds the per-shard restore init container spec (06 §7.4,
// §8.5): it runs cmd/valkey-backup --download --shard=<i> to fetch this shard's
// dump.rdb from object storage into the engine's data dir BEFORE the Valkey process
// starts, verifying the sha256 against the manifest and failing pod start on
// mismatch (06 §9.3). The sidecar reads the storage type/coordinates and the
// cluster/backup names that derive object keys from the VALKEY_BACKUP_* env
// (op carries them — see pkg/backup.JobEnv); only --download/--shard are flags
// (06 §4.1, §8.7). Credentials are mounted as env into the POD via the creds
// Secret (the operator never authenticates — 06 §8.2). This is a pure builder the
// node/cluster controller injects when it observes the restore markers; exposing
// it here keeps the seed mechanism testable in this leg.
func restoreInitContainer(shardIndex int, image, credentialsSecret string, op backup.JobEnvParams) corev1.Container {
	c := corev1.Container{
		Name:  seedContainerName,
		Image: image,
		Command: []string{
			"/valkey-backup",
			"--download",
			fmt.Sprintf("--shard=%d", shardIndex),
		},
		Env: seedEnv(op),
		VolumeMounts: []corev1.VolumeMount{
			{Name: seedDataVolume, MountPath: seedDataDir},
		},
	}
	if credentialsSecret != "" {
		// Credentials live in the POD env (the Job/pod SA), never in the operator
		// process (06 §8.2). GCS differs (file mount); the env path covers S3/Azure.
		c.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: credentialsSecret},
			},
		}}
	}
	return c
}

// seedEnv maps the backend-agnostic backup.JobEnv key/value list to corev1.EnvVar
// (pkg/backup has no core/v1 dependency at the seam). The download seed needs the
// storage coordinates plus the cluster/backup names that derive object keys; it
// does NOT connect to the engine, so no seed-node/auth/TLS env is set.
func seedEnv(op backup.JobEnvParams) []corev1.EnvVar {
	kvs := backup.JobEnv(op)
	out := make([]corev1.EnvVar, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, corev1.EnvVar{Name: kv.Name, Value: kv.Value})
	}
	return out
}

// seedOverrideApplied reports whether the target cluster carries the appendonly-no
// seed override marker — the single place restore overrides persistence so the
// engine loads dump.rdb instead of an empty AOF on the seed boot (06 §7.4, R3). The
// cluster/node controller reads this marker to render appendonly no for the seed
// boot and to re-enable AOF (CONFIG SET appendonly yes) once the keyspace is loaded.
func seedOverrideApplied(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	if cluster == nil || cluster.Annotations == nil {
		return false
	}
	return cluster.Annotations[annSeedAppendonly] == seedAppendonlyNo
}

// restoreMarkerApplied reports whether the target cluster carries the restored-from
// marker (so the cluster controller gates engine rolls and the node controller
// injects the seed init container — 06 §7.4).
func restoreMarkerApplied(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	if cluster == nil || cluster.Annotations == nil {
		return false
	}
	_, ok := cluster.Annotations[annRestoreMarker]
	return ok
}
