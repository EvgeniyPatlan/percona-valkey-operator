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

// Package backup is the storage-backend abstraction that decouples the backup,
// restore and scheduling subsystems from any one object store (S3 / GCS / Azure
// / filesystem-for-tests). It defines the repository-pattern ArtifactStore seam
// (06 §8.1), the authoritative Manifest written last / deleted first
// (06 §4.5, §6.1), the object-key naming scheme (keys.go), a StorageConfig
// resolver that maps a PerconaValkeyBackup's named storage + credentials Secret
// to a concrete backend, and an in-memory FakeStore for hermetic tests.
//
// The concrete cloud backends (s3Store/gcsStore/azureStore) are filled in by the
// M4 storage leg behind newBackend (the constructor seam in config.go); this
// foundation ships only the interface, the manifest, the key helpers, the
// resolver/seam and the fake. See docs/implementation/05-phase4-backup-restore.md
// (GO-4.1 .. GO-4.3).
package backup

import (
	"context"
	"errors"
	"io"
)

// ErrNotExist is returned (or wrapped) by ArtifactStore implementations when a
// requested key/prefix does not exist. Callers test it with errors.Is so the
// finalizer cleanup can treat an already-gone object as success (idempotent
// delete, 06 §6.1) and Exists can map a miss to (false, nil).
var ErrNotExist = errors.New("backup: object does not exist")

// ArtifactStore hides the object-store backend behind a single, swappable,
// mockable interface (the repository pattern, 06 §8.1). It is the ONLY data-access
// seam the backup/restore controllers and the cmd/valkey-backup sidecar depend
// on. Implementations MUST stream — never buffering a whole RDB in memory — so a
// Job's memory footprint is independent of dataset size (06 §4.8).
//
// Keys are store-relative object keys produced by the helpers in keys.go
// (ManifestKey/ShardRDBKey/...); a backend prepends its own bucket/prefix at
// construction time. All methods accept a context for cancellation/deadline.
type ArtifactStore interface {
	// Put writes the object at key from r. size is the exact number of bytes r
	// will yield (-1 when unknown) so a backend can pick single-shot vs multipart
	// upload; implementations still read r to EOF. Put overwrites an existing key
	// (uploads are overwrite-by-key, making backup re-issue idempotent, 06 §9.3).
	Put(ctx context.Context, key string, r io.Reader, size int64) error

	// Get opens the object at key for reading. The caller MUST Close the returned
	// ReadCloser. Get returns a wrapped ErrNotExist when key is absent.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// List returns the store-relative keys under prefix (recursive). The order is
	// unspecified; callers that need determinism sort the result. An empty/missing
	// prefix yields an empty slice and a nil error (not ErrNotExist).
	List(ctx context.Context, prefix string) ([]string, error)

	// Delete removes the object at key. Deleting an absent key is a no-op success
	// (idempotent) so the manifest-first / delete-first teardown can be retried
	// safely after a crash (06 §6.1).
	Delete(ctx context.Context, key string) error

	// Exists reports whether key is present. A missing key is (false, nil); only a
	// real backend/transport failure returns a non-nil error.
	Exists(ctx context.Context, key string) (bool, error)
}

// WriteManifest marshals m and Puts it at key (typically ManifestKey(...)). It is
// a convenience layered on the ArtifactStore primitives so every caller writes
// the manifest identically — and ALWAYS last, after every shard RDB is uploaded
// (06 §4.5). Bodies of the cloud backends need not implement it.
func WriteManifest(ctx context.Context, store ArtifactStore, key string, m Manifest) error {
	data, err := MarshalManifest(m)
	if err != nil {
		return err
	}
	return store.Put(ctx, key, bytesReader(data), int64(len(data)))
}

// ReadManifest Gets the object at key and unmarshals it into a Manifest. It
// is the first artifact restore reads (06 §7.5); a missing manifest surfaces as a
// wrapped ErrNotExist so the caller can treat the set as incomplete.
func ReadManifest(ctx context.Context, store ArtifactStore, key string) (Manifest, error) {
	rc, err := store.Get(ctx, key)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return Manifest{}, err
	}
	return UnmarshalManifest(data)
}

// bytesReader returns an io.Reader over b without pulling in bytes at every call
// site (keeps the public API import-light for the legs).
func bytesReader(b []byte) io.Reader {
	return &sliceReader{data: b}
}

// sliceReader is a minimal io.Reader over a byte slice used by WriteManifest.
type sliceReader struct {
	data []byte
	pos  int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.pos:])
	s.pos += n
	return n, nil
}
