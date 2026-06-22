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
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// newTestFSStore builds an fsStore rooted at a t.TempDir via the registered
// constructor (also exercising NewStore dispatch for the filesystem type).
func newTestFSStore(t *testing.T) ArtifactStore {
	t.Helper()
	store, err := NewStore(context.Background(), StorageConfig{
		Type:           valkeyv1alpha1.BackupStorageFilesystem,
		FilesystemRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore(filesystem): %v", err)
	}
	return store
}

func TestFilesystemStorePutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newTestFSStore(t)

	key := ShardRDBKey("c", "b", 0)
	want := []byte("dump-rdb-payload")
	if err := store.Put(ctx, key, bytes.NewReader(want), int64(len(want))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	if cErr := rc.Close(); cErr != nil {
		t.Fatalf("Close: %v", cErr)
	}
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}
}

func TestFilesystemStoreExistsAndDelete(t *testing.T) {
	ctx := context.Background()
	store := newTestFSStore(t)
	key := "c/b/shard-0/dump.rdb"

	ok, err := store.Exists(ctx, key)
	if err != nil || ok {
		t.Fatalf("Exists(absent) = %v,%v want false,nil", ok, err)
	}

	if err = store.Put(ctx, key, strings.NewReader("x"), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err = store.Exists(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Exists(present) = %v,%v want true,nil", ok, err)
	}

	if err = store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Idempotent: deleting again is a no-op success.
	if err = store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete(absent): %v", err)
	}
	ok, _ = store.Exists(ctx, key)
	if ok {
		t.Fatalf("Exists after delete = true, want false")
	}
}

func TestFilesystemStoreGetMissingIsErrNotExist(t *testing.T) {
	store := newTestFSStore(t)
	_, err := store.Get(context.Background(), "c/b/missing")
	if !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get(missing) err = %v, want ErrNotExist", err)
	}
}

func TestFilesystemStoreList(t *testing.T) {
	ctx := context.Background()
	store := newTestFSStore(t)
	keys := []string{
		ShardRDBKey("c", "b", 0),
		ShardRDBKey("c", "b", 1),
		ShardRDBKey("c", "b", 10),
		ManifestKey("c", "b"),
		ShardRDBKey("c", "other", 0),
	}
	for _, k := range keys {
		if err := store.Put(ctx, k, strings.NewReader("y"), 1); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}

	// Prefix boundary: "shard-1" must NOT match "shard-10".
	got, err := store.List(ctx, ShardPrefix("c", "b", 1))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0] != ShardRDBKey("c", "b", 1) {
		t.Fatalf("List(shard-1) = %v, want exactly [%s]", got, ShardRDBKey("c", "b", 1))
	}

	// Set-prefix lists every object of backup "b" but not "other".
	got, err = store.List(ctx, SetPrefix("c", "b"))
	if err != nil {
		t.Fatalf("List set: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("List(c/b) = %v, want 4 keys", got)
	}
	for _, k := range got {
		if strings.Contains(k, "/other/") {
			t.Fatalf("List(c/b) leaked %q from backup 'other'", k)
		}
	}

	// Empty prefix lists everything.
	all, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	if len(all) != len(keys) {
		t.Fatalf("List(\"\") = %d keys, want %d", len(all), len(keys))
	}
}

// TestFilesystemStoreManifestWriteLastDeleteFirst proves the crash-safety
// ordering invariant end-to-end on a real backend: every shard RDB is uploaded
// BEFORE the manifest (write-last), and on teardown the manifest is removed
// FIRST (delete-first), per 06 §4.5/§6.1.
func TestFilesystemStoreManifestWriteLastDeleteFirst(t *testing.T) {
	ctx := context.Background()
	store := newTestFSStore(t)
	const cluster, backup = "prod", "prod-bk"

	// Upload shards, then the manifest LAST.
	for i := range 3 {
		if err := store.Put(ctx, ShardRDBKey(cluster, backup, i), strings.NewReader("rdb"), 3); err != nil {
			t.Fatalf("Put shard %d: %v", i, err)
		}
	}
	man := Manifest{Cluster: cluster, BackupName: backup, Mode: "cluster", SlotCoverage: "complete"}
	if err := WriteManifest(ctx, store, ManifestKey(cluster, backup), man); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	// The manifest is readable and round-trips.
	readBack, err := ReadManifest(ctx, store, ManifestKey(cluster, backup))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if readBack.Cluster != cluster || readBack.SlotCoverage != "complete" {
		t.Fatalf("manifest round-trip mismatch: %+v", readBack)
	}

	// Teardown: manifest FIRST, then shards, then verify the set is gone.
	if err = store.Delete(ctx, ManifestKey(cluster, backup)); err != nil {
		t.Fatalf("Delete manifest: %v", err)
	}
	// After manifest deletion the set is recognizably incomplete.
	if _, mErr := ReadManifest(ctx, store, ManifestKey(cluster, backup)); !errors.Is(mErr, ErrNotExist) {
		t.Fatalf("manifest still present after delete-first: %v", mErr)
	}
	for i := range 3 {
		if err = store.Delete(ctx, ShardRDBKey(cluster, backup, i)); err != nil {
			t.Fatalf("Delete shard %d: %v", i, err)
		}
	}
	remaining, err := store.List(ctx, SetPrefix(cluster, backup))
	if err != nil {
		t.Fatalf("List after teardown: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("set not fully removed: %v", remaining)
	}
}

// TestFilesystemStorePathTraversalConfined proves a "../" key can never escape
// root: pathFor cleans the key against a leading "/", collapsing any traversal
// to a root-confined path. The object is written/read inside root, never above
// it (the path-traversal guard motivated by 06 §8.1 key construction).
func TestFilesystemStorePathTraversalConfined(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := newFilesystemStore(ctx, StorageConfig{FilesystemRoot: root})
	if err != nil {
		t.Fatalf("newFilesystemStore: %v", err)
	}
	fs, ok := store.(*fsStore)
	if !ok {
		t.Fatalf("expected *fsStore")
	}
	for _, key := range []string{"../escape", "a/../../escape", "../../etc/passwd"} {
		abs, perr := fs.pathFor(key)
		if perr != nil {
			continue // rejection is also acceptable
		}
		rootClean := filepath.Clean(root)
		if abs != rootClean && !strings.HasPrefix(abs, rootClean+string(filepath.Separator)) {
			t.Fatalf("traversal key %q resolved to %q, escaping root %q", key, abs, root)
		}
		if putErr := store.Put(ctx, key, strings.NewReader("x"), 1); putErr != nil {
			continue
		}
	}
	// No file may exist outside root after writing traversal keys.
	parent := filepath.Dir(filepath.Clean(root))
	matches, _ := filepath.Glob(filepath.Join(parent, "escape"))
	if len(matches) != 0 {
		t.Fatalf("traversal write escaped root: %v", matches)
	}
}

func TestNewFilesystemStoreEmptyRoot(t *testing.T) {
	_, err := NewStore(context.Background(), StorageConfig{Type: valkeyv1alpha1.BackupStorageFilesystem})
	if err == nil {
		t.Fatalf("NewStore(filesystem, empty root) = nil error, want failure")
	}
}

func TestPrefixMatch(t *testing.T) {
	cases := []struct {
		key, prefix string
		want        bool
	}{
		{"a/b/c", "", true},
		{"a/b/c", "a", true},
		{"a/b/c", "a/b", true},
		{"a/b/c", "a/b/c", true},
		{"shard-10/x", "shard-1", false},
		{"a/b/c", "a/b/", true},
		{"x/y", "a", false},
	}
	for _, tc := range cases {
		if got := prefixMatch(tc.key, tc.prefix); got != tc.want {
			t.Errorf("prefixMatch(%q,%q) = %v want %v", tc.key, tc.prefix, got, tc.want)
		}
	}
}

func TestFilesystemStorePutCreatesNestedDirs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := newFilesystemStore(ctx, StorageConfig{FilesystemRoot: dir})
	if err != nil {
		t.Fatalf("newFilesystemStore: %v", err)
	}
	key := "deep/nested/path/object"
	if err = store.Put(ctx, key, strings.NewReader("z"), 1); err != nil {
		t.Fatalf("Put nested: %v", err)
	}
	if _, statErr := filepath.Glob(filepath.Join(dir, "deep", "nested", "path", "object")); statErr != nil {
		t.Fatalf("glob: %v", statErr)
	}
	ok, _ := store.Exists(ctx, key)
	if !ok {
		t.Fatalf("nested object not found after Put")
	}
}
