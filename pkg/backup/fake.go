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
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// FakeStore is a fully in-memory, thread-safe ArtifactStore for hermetic unit and
// envtest use. It decouples the parallel M4 legs from any real object store: the
// backup-controller and restore-controller suites inject a FakeStore where
// production injects S3/GCS/Azure, and the storage/sidecar leg uses it to prove
// write-last / delete-first ordering without a cloud SDK.
//
// FakeStore records the order of Put and Delete operations (Ops) so a test can
// assert manifest-LAST on create and manifest-FIRST on teardown (06 §4.5, §6.1)
// without inspecting timestamps. It is NOT a generated mock — it is a real,
// faithful in-memory backend, which keeps coverage meaningful.
type FakeStore struct {
	mu      sync.RWMutex
	objects map[string][]byte
	ops     []FakeOp
	// failPut, when set and returning non-nil for a key, makes Put fail — used to
	// exercise the abort-on-error path that must leave no manifest (06 §9.3).
	failPut func(key string) error
}

// FakeOpKind is the kind of recorded FakeStore operation.
type FakeOpKind string

const (
	// FakeOpPut records a Put.
	FakeOpPut FakeOpKind = "put"
	// FakeOpDelete records a Delete.
	FakeOpDelete FakeOpKind = "delete"
)

// FakeOp is one recorded mutation against a FakeStore, in call order.
type FakeOp struct {
	// Kind is the operation kind.
	Kind FakeOpKind
	// Key is the object key the operation targeted.
	Key string
}

// NewFakeStore returns an empty, ready-to-use in-memory ArtifactStore.
func NewFakeStore() *FakeStore {
	return &FakeStore{objects: map[string][]byte{}}
}

// compile-time assertion that FakeStore satisfies the interface.
var _ ArtifactStore = (*FakeStore)(nil)

// SetPutError installs a per-key Put failure predicate (nil clears it). When fn
// returns a non-nil error for a key, Put fails with it and stores nothing,
// letting tests assert that an interrupted upload leaves no manifest.
func (f *FakeStore) SetPutError(fn func(key string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPut = fn
}

// Put stores key from r (reading it fully into memory — fine for tests), records
// the op, and overwrites any prior value at key.
func (f *FakeStore) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("fake store: read %q: %w", key, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		if perr := f.failPut(key); perr != nil {
			return perr
		}
	}
	f.objects[key] = data
	f.ops = append(f.ops, FakeOp{Kind: FakeOpPut, Key: key})
	return nil
}

// Get returns a ReadCloser over a copy of the object at key, or a wrapped
// ErrNotExist when absent.
func (f *FakeStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("get %q: %w", key, ErrNotExist)
	}
	clone := slices.Clone(data)
	return io.NopCloser(bytes.NewReader(clone)), nil
}

// List returns the keys under prefix (recursive), sorted for deterministic tests.
// An empty prefix lists everything.
func (f *FakeStore) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if prefix == "" || k == prefix || strings.HasPrefix(k, strings.TrimSuffix(prefix, "/")+"/") {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Delete removes the object at key (idempotent: absent key is a no-op success)
// and records the op so delete-ORDER can be asserted.
func (f *FakeStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	f.ops = append(f.ops, FakeOp{Kind: FakeOpDelete, Key: key})
	return nil
}

// Exists reports whether key is present.
func (f *FakeStore) Exists(_ context.Context, key string) (bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, ok := f.objects[key]
	return ok, nil
}

// Ops returns a copy of the recorded Put/Delete operations in call order. Tests
// use it to assert manifest-LAST on create and manifest-FIRST on teardown.
func (f *FakeStore) Ops() []FakeOp {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return slices.Clone(f.ops)
}

// Keys returns a sorted snapshot of every stored key (convenience for assertions).
func (f *FakeStore) Keys() []string {
	keys, _ := f.List(context.Background(), "")
	return keys
}

// Len returns the number of stored objects.
func (f *FakeStore) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.objects)
}
