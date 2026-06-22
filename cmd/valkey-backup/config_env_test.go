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
	"testing"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

func TestStorageConfigFromEnvS3(t *testing.T) {
	t.Setenv(envStorageType, "s3")
	t.Setenv(envS3Bucket, "bkt")
	t.Setenv(envS3Prefix, "prod/")
	t.Setenv(envS3Region, "eu-central-1")
	t.Setenv(envS3Endpoint, "https://minio:9000")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	cfg, err := storageConfigFromEnv()
	if err != nil {
		t.Fatalf("storageConfigFromEnv: %v", err)
	}
	if cfg.Type != valkeyv1alpha1.BackupStorageS3 {
		t.Fatalf("type = %q", cfg.Type)
	}
	if cfg.S3 == nil || cfg.S3.Bucket != "bkt" || cfg.S3.EndpointURL != "https://minio:9000" {
		t.Fatalf("S3 spec = %+v", cfg.S3)
	}
	if cfg.Credentials["AWS_ACCESS_KEY_ID"] != "AKIA" || cfg.Credentials["AWS_SECRET_ACCESS_KEY"] != "secret" {
		t.Fatalf("creds = %v", cfg.Credentials)
	}
}

func TestStorageConfigFromEnvGCS(t *testing.T) {
	t.Setenv(envStorageType, "gcs")
	t.Setenv(envGCSBucket, "gbkt")
	t.Setenv(envGCSPrefix, "p/")
	cfg, err := storageConfigFromEnv()
	if err != nil {
		t.Fatalf("storageConfigFromEnv: %v", err)
	}
	if cfg.Type != valkeyv1alpha1.BackupStorageGCS || cfg.GCS == nil || cfg.GCS.Bucket != "gbkt" {
		t.Fatalf("GCS cfg = %+v", cfg)
	}
}

func TestStorageConfigFromEnvAzure(t *testing.T) {
	t.Setenv(envStorageType, "azure")
	t.Setenv(envAzureCtr, "ctr")
	t.Setenv(envAzurePrefix, "p/")
	cfg, err := storageConfigFromEnv()
	if err != nil {
		t.Fatalf("storageConfigFromEnv: %v", err)
	}
	if cfg.Type != valkeyv1alpha1.BackupStorageAzure || cfg.Azure == nil || cfg.Azure.Container != "ctr" {
		t.Fatalf("Azure cfg = %+v", cfg)
	}
}

func TestStorageConfigFromEnvFilesystem(t *testing.T) {
	t.Setenv(envStorageType, "filesystem")
	t.Setenv(envFSRoot, "/tmp/vk")
	cfg, err := storageConfigFromEnv()
	if err != nil {
		t.Fatalf("storageConfigFromEnv: %v", err)
	}
	if cfg.Type != valkeyv1alpha1.BackupStorageFilesystem || cfg.FilesystemRoot != "/tmp/vk" {
		t.Fatalf("fs cfg = %+v", cfg)
	}
}

func TestStorageConfigFromEnvUnknown(t *testing.T) {
	t.Setenv(envStorageType, "")
	if _, err := storageConfigFromEnv(); err == nil {
		t.Fatalf("storageConfigFromEnv(empty) = nil error, want failure")
	}
	t.Setenv(envStorageType, "ftp")
	if _, err := storageConfigFromEnv(); err == nil {
		t.Fatalf("storageConfigFromEnv(ftp) = nil error, want failure")
	}
}

func TestCredsFromEnvOnlyPresent(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	creds := credsFromEnv()
	if creds["AWS_ACCESS_KEY_ID"] != "id" {
		t.Fatalf("creds = %v", creds)
	}
	if _, ok := creds["AZURE_STORAGE_KEY"]; ok {
		t.Fatalf("absent key leaked into creds map: %v", creds)
	}
}

func TestShardPrimariesFromClusterNodes(t *testing.T) {
	// 3 primaries + 3 replicas, complete cover. Addresses deliberately out of
	// order so we can assert ascending-by-address ordering.
	raw := "" +
		"p2 10.0.0.5:6379@16379 master - 0 0 3 connected 10923-16383\n" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460\n" +
		"r0 10.0.0.2:6379@16379 slave p0 0 100 1 connected\n" +
		"p1 10.0.0.3:6379@16379 master - 0 0 2 connected 5461-10922\n" +
		"r1 10.0.0.4:6379@16379 slave p1 0 100 2 connected\n" +
		"r2 10.0.0.6:6379@16379 slave p2 0 100 3 connected\n"

	got, err := shardPrimariesFromClusterNodes(raw)
	if err != nil {
		t.Fatalf("shardPrimariesFromClusterNodes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d primaries, want 3: %+v", len(got), got)
	}
	// Ascending by address: p0 (10.0.0.1) -> p1 (10.0.0.3) -> p2 (10.0.0.5).
	wantAddrs := []string{"10.0.0.1:6379", "10.0.0.3:6379", "10.0.0.5:6379"}
	wantIDs := []string{"p0", "p1", "p2"}
	for i, sp := range got {
		if sp.index != i {
			t.Errorf("primary[%d].index = %d, want %d", i, sp.index, i)
		}
		if sp.addr != wantAddrs[i] {
			t.Errorf("primary[%d].addr = %q, want %q", i, sp.addr, wantAddrs[i])
		}
		if sp.nodeID != wantIDs[i] {
			t.Errorf("primary[%d].nodeID = %q, want %q", i, sp.nodeID, wantIDs[i])
		}
		if len(sp.slots) == 0 {
			t.Errorf("primary[%d] has no slots", i)
		}
	}
	// The derived primaries must form a complete 0..16383 cover.
	var union []valkeySlotRangeAlias
	for _, sp := range got {
		for _, r := range sp.slots {
			union = append(union, valkeySlotRangeAlias{r.Start, r.End})
		}
	}
	if len(union) != 3 {
		t.Fatalf("expected 3 slot ranges across primaries, got %d", len(union))
	}
}

func TestShardPrimariesSkipsFailedAndSlotless(t *testing.T) {
	raw := "" +
		"p0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460\n" +
		"dead 10.0.0.9:6379@16379 master,fail - 0 0 2 disconnected 5461-10922\n" +
		"empty 10.0.0.8:6379@16379 master - 0 0 3 connected\n" + // no slots
		"p2 10.0.0.5:6379@16379 master - 0 0 4 connected 10923-16383\n"

	got, err := shardPrimariesFromClusterNodes(raw)
	if err != nil {
		t.Fatalf("shardPrimariesFromClusterNodes: %v", err)
	}
	// Only p0 and p2 qualify (dead is failed, empty owns no slots).
	if len(got) != 2 {
		t.Fatalf("got %d primaries, want 2: %+v", len(got), got)
	}
	for _, sp := range got {
		if sp.nodeID == "dead" || sp.nodeID == "empty" {
			t.Fatalf("disqualified node %q included", sp.nodeID)
		}
	}
}

func TestShardPrimariesNoPrimaries(t *testing.T) {
	raw := "r0 10.0.0.2:6379@16379 myself,slave p0 0 100 1 connected\n"
	if _, err := shardPrimariesFromClusterNodes(raw); err == nil {
		t.Fatalf("shardPrimariesFromClusterNodes(no primaries) = nil error, want failure")
	}
}

// valkeySlotRangeAlias is a tiny local struct used only to materialise the
// captured slot-range union in the cover assertion above.
type valkeySlotRangeAlias struct{ start, end int }
