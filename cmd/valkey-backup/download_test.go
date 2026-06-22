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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// testSetCluster / testSetBackup are the fixed cluster/backup names the download
// and cleanup test fixtures seed under.
const (
	testSetCluster = "c"
	testSetBackup  = "b"
)

// seedSet uploads a backup-set (shards + manifest) into a FakeStore under the
// fixed test cluster/backup names, for the download and cleanup tests.
func seedSet(t *testing.T, store backup.ArtifactStore, payloads map[int][]byte) {
	t.Helper()
	ctx := context.Background()
	man := backup.Manifest{Cluster: testSetCluster, BackupName: testSetBackup, Mode: "cluster", SlotCoverage: "complete"}
	for idx, data := range payloads {
		if err := store.Put(ctx, backup.ShardRDBKey(testSetCluster, testSetBackup, idx), bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("seed shard %d: %v", idx, err)
		}
		sum := sha256.Sum256(data)
		man.Shards = append(man.Shards, backup.ShardManifest{
			Index:  idx,
			RDBKey: backup.ShardRDBRelKey(idx),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	if err := backup.WriteManifest(ctx, store, backup.ManifestKey(testSetCluster, testSetBackup), man); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
}

func TestRunDownloadVerifiesAndWrites(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	payloads := map[int][]byte{0: []byte("shard-0-rdb"), 1: []byte("shard-1-rdb")}
	seedSet(t, store, payloads)

	var dst bytes.Buffer
	if err := runDownload(ctx, downloadOptions{
		cluster: "c", backupName: "b", shardIndex: 1, store: store, dst: &dst,
	}); err != nil {
		t.Fatalf("runDownload: %v", err)
	}
	if !bytes.Equal(dst.Bytes(), payloads[1]) {
		t.Fatalf("downloaded %q, want %q", dst.Bytes(), payloads[1])
	}
}

func TestRunDownloadSha256Mismatch(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	// Manifest claims a wrong sha256 for shard 0; download must fail.
	man := backup.Manifest{
		Cluster: "c", BackupName: "b", SlotCoverage: "complete",
		Shards: []backup.ShardManifest{{Index: 0, RDBKey: backup.ShardRDBRelKey(0), SHA256: "deadbeef"}},
	}
	if err := store.Put(ctx, backup.ShardRDBKey("c", "b", 0), strings.NewReader("real-bytes"), 10); err != nil {
		t.Fatalf("put shard: %v", err)
	}
	if err := backup.WriteManifest(ctx, store, backup.ManifestKey("c", "b"), man); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var dst bytes.Buffer
	err := runDownload(ctx, downloadOptions{cluster: "c", backupName: "b", shardIndex: 0, store: store, dst: &dst})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("runDownload(mismatch) err = %v, want sha256 mismatch", err)
	}
}

func TestRunDownloadMissingManifest(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	var dst bytes.Buffer
	err := runDownload(ctx, downloadOptions{cluster: "c", backupName: "b", shardIndex: 0, store: store, dst: &dst})
	if err == nil {
		t.Fatalf("runDownload(no manifest) = nil error, want failure")
	}
}

func TestRunDownloadUnknownShard(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	seedSet(t, store, map[int][]byte{0: []byte("x")})
	var dst bytes.Buffer
	err := runDownload(ctx, downloadOptions{cluster: "c", backupName: "b", shardIndex: 9, store: store, dst: &dst})
	if err == nil || !strings.Contains(err.Error(), "no shard 9") {
		t.Fatalf("runDownload(unknown shard) err = %v, want 'no shard 9'", err)
	}
}

func TestRunDownloadNilGuards(t *testing.T) {
	ctx := context.Background()
	if err := runDownload(ctx, downloadOptions{}); err == nil {
		t.Fatalf("runDownload(nil store) = nil error, want failure")
	}
	if err := runDownload(ctx, downloadOptions{store: backup.NewFakeStore()}); err == nil {
		t.Fatalf("runDownload(nil dst) = nil error, want failure")
	}
}

func TestFindShard(t *testing.T) {
	man := backup.Manifest{Shards: []backup.ShardManifest{{Index: 0}, {Index: 2}}}
	if _, ok := findShard(man, 2); !ok {
		t.Fatalf("findShard(2) not found")
	}
	if _, ok := findShard(man, 1); ok {
		t.Fatalf("findShard(1) unexpectedly found")
	}
}
