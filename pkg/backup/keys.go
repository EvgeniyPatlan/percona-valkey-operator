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
	"path"
	"strconv"
	"strings"
)

// Object-key layout for a backup-set, rooted at the per-backup prefix
// (06 §3.2, §4.5). For backup "<backup>" of cluster "<cluster>" the layout is:
//
//	<cluster>/<backup>/manifest.json        (written LAST, deleted FIRST)
//	<cluster>/<backup>/shard-0/dump.rdb
//	<cluster>/<backup>/shard-1/dump.rdb
//	...
//
// These helpers produce store-relative keys; a backend's own bucket/prefix
// (BackupStorageS3Spec.Prefix, ...) is prepended by the StorageConfig at
// construction time, never here, so the same key scheme is backend-agnostic.

const (
	// rdbFilename is the object name of a shard's RDB dump within its shard prefix.
	rdbFilename = "dump.rdb"
	// shardPrefix is the per-shard key prefix ("shard-<i>/...").
	shardPrefix = "shard-"
)

// SetPrefix returns the store-relative root key of a backup-set:
// "<cluster>/<backup>". Every object in the set lives under this prefix; the
// finalizer cleanup deletes it recursively after the manifest and RDBs are gone
// (06 §6.1).
func SetPrefix(cluster, backup string) string {
	return path.Join(cluster, backup)
}

// ManifestKey returns the store-relative key of the backup-set manifest:
// "<cluster>/<backup>/manifest.json". The manifest is written LAST on create and
// deleted FIRST on teardown — its presence is the completeness marker (06 §4.5,
// §6.1).
func ManifestKey(cluster, backup string) string {
	return path.Join(SetPrefix(cluster, backup), ManifestFilename)
}

// ShardPrefix returns the store-relative prefix of a shard's objects:
// "<cluster>/<backup>/shard-<i>". Used by the cleanup pass to reclaim a shard's
// blobs.
func ShardPrefix(cluster, backup string, shardIndex int) string {
	return path.Join(SetPrefix(cluster, backup), shardName(shardIndex))
}

// ShardRDBKey returns the store-relative object key of a shard's RDB dump:
// "<cluster>/<backup>/shard-<i>/dump.rdb" (06 §4.3 step 5).
func ShardRDBKey(cluster, backup string, shardIndex int) string {
	return path.Join(ShardPrefix(cluster, backup, shardIndex), rdbFilename)
}

// ShardRDBRelKey returns the manifest-relative RDB key recorded in
// ShardManifest.RDBKey: "shard-<i>/dump.rdb". The manifest records shard keys
// relative to the set root so a set is portable if its prefix moves.
func ShardRDBRelKey(shardIndex int) string {
	return path.Join(shardName(shardIndex), rdbFilename)
}

// shardName returns the per-shard directory component "shard-<i>".
func shardName(shardIndex int) string {
	return shardPrefix + strconv.Itoa(shardIndex)
}

// joinKey joins key segments with the object-store "/" separator, trimming any
// empty/edge slashes so callers never produce "//" or a leading "/". Object
// stores treat "/" purely as a key separator; this keeps keys canonical and
// prevents path-traversal-style "../" segments from leaking into a key.
func joinKey(segments ...string) string {
	cleaned := make([]string, 0, len(segments))
	for _, s := range segments {
		s = strings.Trim(s, "/")
		if s == "" {
			continue
		}
		cleaned = append(cleaned, s)
	}
	return strings.Join(cleaned, "/")
}
