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
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// errorReader fails partway through, exercising the Put copy-error branch on the
// streaming filesystem backend (no partial object left behind).
type errorReader struct{ emitted bool }

func (e *errorReader) Read(p []byte) (int, error) {
	if !e.emitted {
		e.emitted = true
		return copy(p, []byte("partial")), nil
	}
	return 0, errors.New("reader broke mid-stream")
}

func TestFilesystemStorePutReaderError(t *testing.T) {
	ctx := context.Background()
	store, err := newFilesystemStore(ctx, StorageConfig{FilesystemRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("newFilesystemStore: %v", err)
	}
	key := "c/b/shard-0/dump.rdb"
	if putErr := store.Put(ctx, key, &errorReader{}, -1); putErr == nil {
		t.Fatalf("Put(broken reader) = nil error, want failure")
	}
	// No partial object may remain at key (temp-file-then-rename guarantee).
	if ok, _ := store.Exists(ctx, key); ok {
		t.Fatalf("Put left a partial object after a stream error")
	}
}

func TestAzureServiceURLDefault(t *testing.T) {
	got := azureServiceURL("myaccount")
	if got != "https://myaccount.blob.core.windows.net/" {
		t.Fatalf("azureServiceURL = %q", got)
	}
}

func TestS3DeleteRealErrorSurfaces(t *testing.T) {
	ctx := context.Background()
	// A server that 500s every request: Delete must surface a non-NotFound error
	// rather than swallowing it.
	srv := startFakeS3Failing(t)
	store := newS3StoreAgainst(t, srv.URL, "")
	if err := store.Delete(ctx, "c/b/x"); err == nil {
		t.Fatalf("Delete(server 500) = nil error, want surfaced failure")
	}
}

func TestFakeStoreKeysHelper(t *testing.T) {
	store := NewFakeStore()
	_ = store.Put(context.Background(), "c/b/k1", strings.NewReader("x"), 1)
	_ = store.Put(context.Background(), "c/b/k2", strings.NewReader("y"), 1)
	keys := store.Keys()
	if len(keys) != 2 || keys[0] != "c/b/k1" {
		t.Fatalf("Keys() = %v, want [c/b/k1 c/b/k2]", keys)
	}
}

func TestWriteManifestPropagatesPutError(t *testing.T) {
	store := NewFakeStore()
	store.SetPutError(func(string) error { return fmt.Errorf("backend down") })
	err := WriteManifest(context.Background(), store, ManifestKey("c", "b"), Manifest{Cluster: "c"})
	if err == nil {
		t.Fatalf("WriteManifest(put error) = nil error, want failure")
	}
}
