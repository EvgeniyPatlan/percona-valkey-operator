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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// seedFilesystemSet writes a backup-set into a filesystem-backed store rooted at
// root, exercising the same ArtifactStore the env runners build via buildStore.
func seedFilesystemSet(t *testing.T, root, cluster, name string, payloads map[int][]byte) {
	t.Helper()
	ctx := context.Background()
	store, err := backup.NewStore(ctx, backup.StorageConfig{
		Type:           "filesystem",
		FilesystemRoot: root,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	man := backup.Manifest{Cluster: cluster, BackupName: name, Mode: "cluster", SlotCoverage: "complete"}
	for idx, data := range payloads {
		if putErr := store.Put(ctx, backup.ShardRDBKey(cluster, name, idx), strings.NewReader(string(data)), int64(len(data))); putErr != nil {
			t.Fatalf("seed shard %d: %v", idx, putErr)
		}
		man.Shards = append(man.Shards, backup.ShardManifest{Index: idx, RDBKey: backup.ShardRDBRelKey(idx)})
	}
	if wErr := backup.WriteManifest(ctx, store, backup.ManifestKey(cluster, name), man); wErr != nil {
		t.Fatalf("seed manifest: %v", wErr)
	}
}

// setFilesystemEnv points the sidecar at a filesystem-backed store for a given
// cluster/backup, hermetically (no cloud, no Valkey).
func setFilesystemEnv(t *testing.T, root, cluster, name string) {
	t.Helper()
	t.Setenv(envStorageType, "filesystem")
	t.Setenv(envFSRoot, root)
	t.Setenv(envCluster, cluster)
	t.Setenv(envBackupName, name)
}

func TestRunCleanupFromEnv(t *testing.T) {
	root := t.TempDir()
	seedFilesystemSet(t, root, "c", "b", map[int][]byte{0: []byte("a"), 1: []byte("b")})
	setFilesystemEnv(t, root, "c", "b")

	if err := run([]string{"--cleanup"}); err != nil {
		t.Fatalf("run(--cleanup): %v", err)
	}
	// Every object under the set prefix must be reclaimed (object-store view; the
	// filesystem backend may leave empty dirs, which carry no objects).
	store, err := backup.NewStore(context.Background(), backup.StorageConfig{Type: "filesystem", FilesystemRoot: root})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	keys, err := store.List(context.Background(), backup.SetPrefix("c", "b"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("cleanup left objects: %v", keys)
	}
}

func TestRunDownloadFromEnv(t *testing.T) {
	root := t.TempDir()
	payload := []byte("real-rdb-bytes")
	// Seed with a correct manifest sha256 so download verification passes.
	seedFilesystemSetWithSHA(t, root, "c", "b", map[int][]byte{0: payload})

	dst := filepath.Join(t.TempDir(), "dump.rdb")
	setFilesystemEnv(t, root, "c", "b")
	t.Setenv(envDownloadDst, dst)

	if err := run([]string{"--download", "--shard=0"}); err != nil {
		t.Fatalf("run(--download): %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read seeded dump: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("seeded %q, want %q", got, payload)
	}
}

func TestRunBackupFromEnvNoSeedNodeFails(t *testing.T) {
	root := t.TempDir()
	setFilesystemEnv(t, root, "c", "b")
	t.Setenv(envSeedNode, "")
	// Backup mode with no seed node must fail at shard resolution, not panic.
	if err := run(nil); err == nil {
		t.Fatalf("run(backup, no seed) = nil error, want failure")
	}
}

func TestBuildStoreFilesystem(t *testing.T) {
	root := t.TempDir()
	t.Setenv(envStorageType, "filesystem")
	t.Setenv(envFSRoot, root)
	store, err := buildStore(context.Background())
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	if store == nil {
		t.Fatalf("buildStore returned nil store")
	}
}

func TestConnSecurityFromEnvTLSWithCA(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, testCAPEM(t), 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	t.Setenv(envTLSEnabled, "true")
	t.Setenv(envTLSCAFile, caFile)
	t.Setenv(envAuthUser, "_operator")
	t.Setenv(envAuthPassword, "pw")

	auth, tlsConfig, err := connSecurityFromEnv()
	if err != nil {
		t.Fatalf("connSecurityFromEnv: %v", err)
	}
	if tlsConfig == nil || tlsConfig.RootCAs == nil {
		t.Fatalf("expected TLS config with RootCAs")
	}
	if auth.username != "_operator" {
		t.Fatalf("auth.username = %q", auth.username)
	}
}

func TestConnSecurityFromEnvTLSBadCA(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	t.Setenv(envTLSEnabled, "true")
	t.Setenv(envTLSCAFile, caFile)
	if _, _, err := connSecurityFromEnv(); err == nil {
		t.Fatalf("connSecurityFromEnv(bad CA) = nil error, want failure")
	}
}
