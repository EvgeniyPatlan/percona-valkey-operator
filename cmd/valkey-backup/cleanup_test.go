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
	"strings"
	"testing"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

func TestRunCleanupDeletesManifestFirst(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	seedSet(t, store, map[int][]byte{0: []byte("a"), 1: []byte("b"), 2: []byte("c")})

	if err := runCleanup(ctx, cleanupOptions{cluster: "c", backupName: "b", store: store}); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}

	// The first Delete op must target the manifest (delete-first invariant, 06 §6.1).
	var firstDelete *backup.FakeOp
	for i := range store.Ops() {
		op := store.Ops()[i]
		if op.Kind == backup.FakeOpDelete {
			firstDelete = &op
			break
		}
	}
	if firstDelete == nil {
		t.Fatalf("no delete ops recorded")
	}
	if firstDelete.Key != backup.ManifestKey("c", "b") {
		t.Fatalf("first delete = %q, want manifest %q", firstDelete.Key, backup.ManifestKey("c", "b"))
	}

	// Set must be fully reclaimed.
	if store.Len() != 0 {
		t.Fatalf("store not empty after cleanup: %v", store.Keys())
	}
}

func TestRunCleanupIdempotent(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	seedSet(t, store, map[int][]byte{0: []byte("a")})

	if err := runCleanup(ctx, cleanupOptions{cluster: "c", backupName: "b", store: store}); err != nil {
		t.Fatalf("runCleanup(1): %v", err)
	}
	// Re-running on an already-clean set is a no-op success (finalizer-audit retry).
	if err := runCleanup(ctx, cleanupOptions{cluster: "c", backupName: "b", store: store}); err != nil {
		t.Fatalf("runCleanup(2, idempotent): %v", err)
	}
}

func TestRunCleanupOnEmptySet(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	// Nothing seeded: cleanup of a never-completed set is still success.
	if err := runCleanup(ctx, cleanupOptions{cluster: "c", backupName: "b", store: store}); err != nil {
		t.Fatalf("runCleanup(empty): %v", err)
	}
}

func TestRunCleanupManifestlessSet(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	// A crash-mid-create set: RDBs present, no manifest. Cleanup must still reclaim.
	if err := store.Put(ctx, backup.ShardRDBKey("c", "b", 0), strings.NewReader("x"), 1); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := runCleanup(ctx, cleanupOptions{cluster: "c", backupName: "b", store: store}); err != nil {
		t.Fatalf("runCleanup(manifestless): %v", err)
	}
	if store.Len() != 0 {
		t.Fatalf("manifestless set not reclaimed: %v", store.Keys())
	}
}

func TestRunCleanupNilStore(t *testing.T) {
	if err := runCleanup(context.Background(), cleanupOptions{}); err == nil {
		t.Fatalf("runCleanup(nil store) = nil error, want failure")
	}
}
