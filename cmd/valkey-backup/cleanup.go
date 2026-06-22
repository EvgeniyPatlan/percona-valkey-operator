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
	"fmt"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// cleanupOptions is the resolved input to runCleanup, the finalizer teardown
// mode (06 §6.1). It deletes the backup-set artifacts in the crash-safe
// manifest-FIRST order so a crash mid-cleanup never leaves a manifest pointing
// at already-deleted RDBs.
type cleanupOptions struct {
	cluster    string
	backupName string
	store      backup.ArtifactStore
}

// runCleanup removes a backup-set in the order that makes partial-delete
// recovery safe (06 §6.1):
//
//  1. manifest.json FIRST — atomically invalidates the set, so any concurrent
//     restore/GC immediately sees it as incomplete.
//  2. then every shard-*/dump.rdb (and anything else under the set prefix).
//  3. confirm the prefix is empty.
//
// Deletes are idempotent (already-gone == success), so the finalizer-audit pass
// can re-run this safely after a crash.
func runCleanup(ctx context.Context, o cleanupOptions) error {
	if o.store == nil {
		return fmt.Errorf("cleanup: nil store")
	}

	// 1. manifest FIRST.
	manifestKey := backup.ManifestKey(o.cluster, o.backupName)
	if err := o.store.Delete(ctx, manifestKey); err != nil {
		return fmt.Errorf("delete manifest: %w", err)
	}

	// 2. then every remaining object under the set prefix (the shard RDBs and any
	// stragglers). List + delete is overwrite-safe and idempotent.
	setPrefix := backup.SetPrefix(o.cluster, o.backupName)
	keys, err := o.store.List(ctx, setPrefix)
	if err != nil {
		return fmt.Errorf("list set %q: %w", setPrefix, err)
	}
	for _, key := range keys {
		if delErr := o.store.Delete(ctx, key); delErr != nil {
			return fmt.Errorf("delete %q: %w", key, delErr)
		}
	}

	// 3. confirm the prefix is fully reclaimed.
	remaining, err := o.store.List(ctx, setPrefix)
	if err != nil {
		return fmt.Errorf("verify set %q empty: %w", setPrefix, err)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("set %q not fully removed: %d objects remain", setPrefix, len(remaining))
	}
	return nil
}
