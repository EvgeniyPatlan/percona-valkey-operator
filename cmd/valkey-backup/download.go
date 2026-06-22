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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// downloadOptions is the resolved input to runDownload, the restore-seed init
// container mode (06 §7.4). It reads the manifest FIRST, locates the requested
// shard fragment, streams that shard's RDB to dst, and verifies the SHA-256
// against the manifest — failing (so the pod never starts) on mismatch (06 §9.3).
type downloadOptions struct {
	cluster    string
	backupName string
	shardIndex int
	store      backup.ArtifactStore
	// dst is where the verified RDB is written (the engine's /data/dump.rdb).
	dst io.Writer
}

// runDownload fetches one shard's RDB, verifying its SHA-256 against the
// manifest before the bytes are considered valid. The manifest is read FIRST so
// a manifest-less (incomplete / mid-teardown) set is rejected loudly rather than
// seeding a partial dataset (06 §6.1, §7.4).
func runDownload(ctx context.Context, o downloadOptions) error {
	if o.store == nil {
		return fmt.Errorf("download: nil store")
	}
	if o.dst == nil {
		return fmt.Errorf("download: nil destination")
	}

	man, err := backup.ReadManifest(ctx, o.store, backup.ManifestKey(o.cluster, o.backupName))
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	frag, ok := findShard(man, o.shardIndex)
	if !ok {
		return fmt.Errorf("manifest has no shard %d", o.shardIndex)
	}

	key := backup.ShardRDBKey(o.cluster, o.backupName, o.shardIndex)
	rc, err := o.store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = rc.Close() }()

	hasher := sha256.New()
	if _, err = io.Copy(o.dst, io.TeeReader(rc, hasher)); err != nil {
		return fmt.Errorf("stream %s: %w", key, err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if frag.SHA256 != "" && got != frag.SHA256 {
		return fmt.Errorf("shard %d sha256 mismatch: got %s want %s", o.shardIndex, got, frag.SHA256)
	}
	return nil
}

// findShard returns the manifest fragment for shardIndex.
func findShard(man backup.Manifest, shardIndex int) (backup.ShardManifest, bool) {
	idx := slices.IndexFunc(man.Shards, func(s backup.ShardManifest) bool { return s.Index == shardIndex })
	if idx < 0 {
		return backup.ShardManifest{}, false
	}
	return man.Shards[idx], true
}
