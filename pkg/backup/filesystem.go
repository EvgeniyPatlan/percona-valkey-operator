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
	"io"
	"os"
	"path/filepath"
	"strings"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// init wires the test-only filesystem backend into the constructor seam
// (config.go). It is deliberately the simplest ArtifactStore: it maps every
// store-relative key to a file under a base directory, which makes it the
// reference backend for the hermetic round-trip tests every other backend is
// validated against (06 §8.1, "pvc/ for testing only").
func init() {
	RegisterBackend(valkeyv1alpha1.BackupStorageFilesystem, newFilesystemStore)
}

// fsStore is the filesystem-backed ArtifactStore. It is TEST/DEV ONLY (the pvc/
// destination prefix): keys become files under root, "/" in a key maps to a
// directory separator. It streams to/from disk so its memory footprint is
// independent of object size, upholding the streaming contract the cloud
// backends also honour (06 §4.8).
type fsStore struct {
	root string
}

// compile-time assertion that fsStore satisfies the interface.
var _ ArtifactStore = (*fsStore)(nil)

// newFilesystemStore builds an fsStore rooted at cfg.FilesystemRoot. A missing
// root is an error (the test-only backend has no sane default base directory and
// must not silently write into the working directory).
func newFilesystemStore(_ context.Context, cfg StorageConfig) (ArtifactStore, error) {
	if cfg.FilesystemRoot == "" {
		return nil, fmt.Errorf("filesystem backend: empty FilesystemRoot")
	}
	if err := os.MkdirAll(cfg.FilesystemRoot, 0o750); err != nil {
		return nil, fmt.Errorf("filesystem backend: create root %q: %w", cfg.FilesystemRoot, err)
	}
	return &fsStore{root: cfg.FilesystemRoot}, nil
}

// pathFor resolves a store-relative key to an absolute on-disk path, rejecting
// any key that would escape root (path-traversal guard — keys come from helpers
// in keys.go but a download/cleanup mode could be handed an external key).
func (s *fsStore) pathFor(key string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(key))
	abs := filepath.Join(s.root, clean)
	rootClean := filepath.Clean(s.root)
	if abs != rootClean && !strings.HasPrefix(abs, rootClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("filesystem backend: key %q escapes root", key)
	}
	return abs, nil
}

// Put streams r to a file at key, creating parent directories. It writes to a
// temp file and renames so a partial write never leaves a half-object readable
// (the same crash-safety intent the cloud multipart-abort gives, 06 §9.3).
func (s *fsStore) Put(ctx context.Context, key string, r io.Reader, _ int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dst, err := s.pathFor(key)
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(dst), 0o750); mkErr != nil {
		return fmt.Errorf("filesystem backend: mkdir for %q: %w", key, mkErr)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return fmt.Errorf("filesystem backend: temp for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	if _, err = io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("filesystem backend: write %q: %w", key, err)
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("filesystem backend: close %q: %w", key, err)
	}
	if err = os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("filesystem backend: rename %q: %w", key, err)
	}
	return nil
}

// Get opens the file at key for streaming reads. A missing file is reported as a
// wrapped ErrNotExist so callers can errors.Is it.
func (s *fsStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	src, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("filesystem backend: get %q: %w", key, ErrNotExist)
		}
		return nil, fmt.Errorf("filesystem backend: get %q: %w", key, err)
	}
	return f, nil
}

// List walks root collecting store-relative keys under prefix (recursive). An
// absent prefix tree yields an empty slice and nil error (never ErrNotExist).
func (s *fsStore) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	walkErr := filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(s.root, p)
		if relErr != nil {
			return relErr
		}
		key := filepath.ToSlash(rel)
		if prefixMatch(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("filesystem backend: list %q: %w", prefix, walkErr)
	}
	return keys, nil
}

// Delete removes the file at key. Deleting an absent key is a no-op success
// (idempotent), matching the delete-first teardown contract (06 §6.1).
func (s *fsStore) Delete(_ context.Context, key string) error {
	dst, err := s.pathFor(key)
	if err != nil {
		return err
	}
	if rmErr := os.Remove(dst); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return fmt.Errorf("filesystem backend: delete %q: %w", key, rmErr)
	}
	return nil
}

// Exists reports whether a file is present at key. A missing file is
// (false, nil); only a real stat failure returns a non-nil error.
func (s *fsStore) Exists(_ context.Context, key string) (bool, error) {
	dst, err := s.pathFor(key)
	if err != nil {
		return false, err
	}
	info, statErr := os.Stat(dst)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("filesystem backend: exists %q: %w", key, statErr)
	}
	return !info.IsDir(), nil
}

// prefixMatch reports whether key is under prefix, treating an empty prefix as
// "everything" and matching on a "/"-delimited boundary so "shard-1" does not
// match prefix "shard-10". A prefix equal to the key matches too.
func prefixMatch(key, prefix string) bool {
	if prefix == "" {
		return true
	}
	trimmed := strings.TrimSuffix(prefix, "/")
	return key == trimmed || strings.HasPrefix(key, trimmed+"/")
}
