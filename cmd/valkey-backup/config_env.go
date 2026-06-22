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

package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// cloudCredEnvKeys are the cloud-SDK env-var names the backends consume. They are
// copied verbatim from the Job's environment into StorageConfig.Credentials so a
// backend finds its mounted-Secret values; the operator never reads these.
var cloudCredEnvKeys = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AZURE_STORAGE_ACCOUNT",
	"AZURE_STORAGE_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_APPLICATION_CREDENTIALS_JSON",
}

// credsFromEnv collects the cloud credential VALUES present in the Job's
// environment into the map the backends expect.
func credsFromEnv() map[string]string {
	creds := make(map[string]string, len(cloudCredEnvKeys))
	for _, k := range cloudCredEnvKeys {
		if v := os.Getenv(k); v != "" {
			creds[k] = v
		}
	}
	return creds
}

// s3SpecFromEnv reads the S3 storage coordinates from VALKEY_BACKUP_S3_* env.
func s3SpecFromEnv() *valkeyv1alpha1.BackupStorageS3Spec {
	return &valkeyv1alpha1.BackupStorageS3Spec{
		Bucket:      os.Getenv(envS3Bucket),
		Prefix:      os.Getenv(envS3Prefix),
		Region:      os.Getenv(envS3Region),
		EndpointURL: os.Getenv(envS3Endpoint),
	}
}

// gcsSpecFromEnv reads the GCS storage coordinates from VALKEY_BACKUP_GCS_* env.
func gcsSpecFromEnv() *valkeyv1alpha1.BackupStorageGCSSpec {
	return &valkeyv1alpha1.BackupStorageGCSSpec{
		Bucket: os.Getenv(envGCSBucket),
		Prefix: os.Getenv(envGCSPrefix),
	}
}

// azureSpecFromEnv reads the Azure storage coordinates from VALKEY_BACKUP_AZURE_*
// env.
func azureSpecFromEnv() *valkeyv1alpha1.BackupStorageAzureSpec {
	return &valkeyv1alpha1.BackupStorageAzureSpec{
		Container: os.Getenv(envAzureCtr),
		Prefix:    os.Getenv(envAzurePrefix),
	}
}

// shardPrimariesFromClusterNodes derives the live primary-per-shard list from
// raw CLUSTER NODES output (06 §4.3 step 1). It selects nodes flagged master
// that own at least one slot — never trusting labels (06 §1.3) — skips
// fail/noaddr nodes, and orders them ascending by dial address so the assigned
// shardIndex is deterministic and diffable (06 §4.4, matching compareByAddr).
func shardPrimariesFromClusterNodes(raw string) ([]shardPrimary, error) {
	nodes, err := valkey.ParseClusterNodes(raw)
	if err != nil {
		return nil, fmt.Errorf("parse CLUSTER NODES: %w", err)
	}

	primaries := make([]*valkeyNodeView, 0, len(nodes))
	for _, n := range nodes {
		if !n.IsPrimary() || n.IsFailed() {
			continue
		}
		if n.HasFlag("noaddr") || n.Addr == "" {
			continue
		}
		if len(n.Slots) == 0 {
			continue // a primary owning no slots is not part of the snapshot set
		}
		primaries = append(primaries, &valkeyNodeView{
			id:    n.ID,
			addr:  n.Addr,
			slots: n.Slots,
		})
	}
	if len(primaries) == 0 {
		return nil, fmt.Errorf("no slot-owning primaries found in CLUSTER NODES")
	}

	slices.SortFunc(primaries, func(a, b *valkeyNodeView) int { return strings.Compare(a.addr, b.addr) })

	out := make([]shardPrimary, 0, len(primaries))
	for i, p := range primaries {
		out = append(out, shardPrimary{
			index:  i,
			nodeID: p.id,
			addr:   p.addr,
			slots:  p.slots,
		})
	}
	return out, nil
}

// valkeyNodeView is the minimal projection of a parsed CLUSTER NODES line used
// to build a shardPrimary.
type valkeyNodeView struct {
	id    string
	addr  string
	slots []valkey.SlotRange
}
