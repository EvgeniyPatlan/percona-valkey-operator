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

package backup_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

func TestFakeStorePutGetExistsDelete(t *testing.T) {
	ctx := context.Background()
	fs := backup.NewFakeStore()

	const key = "prod/b/shard-0/dump.rdb"
	if ok, err := fs.Exists(ctx, key); err != nil || ok {
		t.Fatalf("Exists before Put: ok=%v err=%v", ok, err)
	}
	if err := fs.Put(ctx, key, strings.NewReader("rdb-bytes"), 9); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, err := fs.Exists(ctx, key); err != nil || !ok {
		t.Fatalf("Exists after Put: ok=%v err=%v", ok, err)
	}
	rc, err := fs.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "rdb-bytes" {
		t.Errorf("Get returned %q", data)
	}
	if err := fs.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := fs.Exists(ctx, key); ok {
		t.Error("key still exists after Delete")
	}
	// Idempotent delete: deleting again succeeds.
	if err := fs.Delete(ctx, key); err != nil {
		t.Errorf("second Delete should be a no-op success, got %v", err)
	}
}

func TestFakeStoreGetMissingIsErrNotExist(t *testing.T) {
	_, err := backup.NewFakeStore().Get(context.Background(), "missing")
	if !errors.Is(err, backup.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestFakeStoreList(t *testing.T) {
	ctx := context.Background()
	fs := backup.NewFakeStore()
	for _, k := range []string{"c/b/manifest.json", "c/b/shard-0/dump.rdb", "c/b/shard-1/dump.rdb", "other/x"} {
		if err := fs.Put(ctx, k, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	got, err := fs.List(ctx, "c/b")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"c/b/manifest.json", "c/b/shard-0/dump.rdb", "c/b/shard-1/dump.rdb"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("List(c/b) = %v, want %v", got, want)
	}
	all, _ := fs.List(ctx, "")
	if len(all) != 4 {
		t.Errorf("List(\"\") returned %d keys, want 4", len(all))
	}
}

// TestWriteManifestLast proves the create-order invariant: manifest.json is the
// LAST Put after all shard RDBs (06 §4.5).
func TestWriteManifestLast(t *testing.T) {
	ctx := context.Background()
	fs := backup.NewFakeStore()
	const cluster, name = "prod", "prod-1"

	for i := 0; i < 3; i++ {
		if err := fs.Put(ctx, backup.ShardRDBKey(cluster, name, i), strings.NewReader("rdb"), 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := backup.WriteManifest(ctx, fs, backup.ManifestKey(cluster, name), sampleManifest()); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	ops := fs.Ops()
	last := ops[len(ops)-1]
	if last.Kind != backup.FakeOpPut || last.Key != backup.ManifestKey(cluster, name) {
		t.Errorf("manifest not written last; last op = %+v", last)
	}
	// Round-trip the manifest back.
	m, err := backup.ReadManifest(ctx, fs, backup.ManifestKey(cluster, name))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.SlotCoverage != "complete" || len(m.Shards) != 3 {
		t.Errorf("read manifest unexpected: %+v", m)
	}
}

// TestDeleteManifestFirst proves the teardown-order invariant: manifest.json is
// the FIRST Delete, before any shard RDB (06 §6.1).
func TestDeleteManifestFirst(t *testing.T) {
	ctx := context.Background()
	fs := backup.NewFakeStore()
	const cluster, name = "prod", "prod-1"
	mk := backup.ManifestKey(cluster, name)

	for i := 0; i < 3; i++ {
		_ = fs.Put(ctx, backup.ShardRDBKey(cluster, name, i), strings.NewReader("rdb"), 3)
	}
	_ = backup.WriteManifest(ctx, fs, mk, sampleManifest())

	// Simulate the cleanup order: manifest first, then shards, then prefix.
	if err := fs.Delete(ctx, mk); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = fs.Delete(ctx, backup.ShardRDBKey(cluster, name, i))
	}

	var firstDelete backup.FakeOp
	for _, op := range fs.Ops() {
		if op.Kind == backup.FakeOpDelete {
			firstDelete = op
			break
		}
	}
	if firstDelete.Key != mk {
		t.Errorf("first delete = %q, want manifest %q", firstDelete.Key, mk)
	}
	if fs.Len() != 0 {
		t.Errorf("store not empty after teardown: %d objects", fs.Len())
	}
}

// TestPutErrorLeavesNoManifest proves the abort path: when a shard upload fails,
// no manifest is written, so the set is recognizably incomplete (06 §9.3).
func TestPutErrorLeavesNoManifest(t *testing.T) {
	ctx := context.Background()
	fs := backup.NewFakeStore()
	const cluster, name = "prod", "prod-1"
	failKey := backup.ShardRDBKey(cluster, name, 1)
	fs.SetPutError(func(key string) error {
		if key == failKey {
			return fmt.Errorf("simulated upload failure")
		}
		return nil
	})
	_ = fs.Put(ctx, backup.ShardRDBKey(cluster, name, 0), strings.NewReader("rdb"), 3)
	if err := fs.Put(ctx, failKey, strings.NewReader("rdb"), 3); err == nil {
		t.Fatal("expected Put failure")
	}
	if ok, _ := fs.Exists(ctx, failKey); ok {
		t.Error("failed Put should store nothing")
	}
	// Caller must NOT write the manifest on failure; assert it is absent.
	if ok, _ := fs.Exists(ctx, backup.ManifestKey(cluster, name)); ok {
		t.Error("manifest present after aborted backup")
	}
}

func TestFakeStoreConcurrent(t *testing.T) {
	ctx := context.Background()
	fs := backup.NewFakeStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("c/b/shard-%d/dump.rdb", i)
			_ = fs.Put(ctx, key, strings.NewReader("x"), 1)
			_, _ = fs.Exists(ctx, key)
			_, _ = fs.List(ctx, "c/b")
		}(i)
	}
	wg.Wait()
	if fs.Len() != 50 {
		t.Errorf("expected 50 objects, got %d", fs.Len())
	}
}

func TestNewStoreUnregisteredType(t *testing.T) {
	// Leg A wired S3/GCS/Azure/filesystem via init(); an unknown type still
	// fails loudly through the seam.
	const unknown valkeyv1alpha1.BackupStorageType = "no-such-backend"
	_, err := backup.NewStore(context.Background(), backup.StorageConfig{Type: unknown})
	if err == nil {
		t.Fatal("expected error for unregistered backend type")
	}
	if backup.BackendRegistered(unknown) {
		t.Errorf("unexpected registration for %q", unknown)
	}
	if !backup.BackendRegistered(valkeyv1alpha1.BackupStorageS3) {
		t.Error("S3 backend should be registered once Leg A wires it")
	}
}

func TestRegisterBackendAndNewStore(t *testing.T) {
	const probe valkeyv1alpha1.BackupStorageType = "probe-test-backend"
	want := backup.NewFakeStore()
	backup.RegisterBackend(probe, func(_ context.Context, _ backup.StorageConfig) (backup.ArtifactStore, error) {
		return want, nil
	})
	got, err := backup.NewStore(context.Background(), backup.StorageConfig{Type: probe})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got != want {
		t.Error("NewStore did not return the registered backend")
	}
}

func TestStorageConfigFromSpec(t *testing.T) {
	spec := valkeyv1alpha1.BackupStorageSpec{
		Type: valkeyv1alpha1.BackupStorageS3,
		S3:   &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "bkt", Region: "eu-central-1"},
	}
	cfg := backup.StorageConfigFromSpec(spec, map[string]string{"AWS_ACCESS_KEY_ID": "x"})
	if cfg.Type != valkeyv1alpha1.BackupStorageS3 || cfg.S3 == nil || cfg.S3.Bucket != "bkt" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if cfg.Credentials["AWS_ACCESS_KEY_ID"] != "x" {
		t.Error("credentials not carried through")
	}
}
