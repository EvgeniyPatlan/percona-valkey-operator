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
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// Consistency modes (06 §9.2). strict (default) fails the whole backup if any
// shard cannot produce a verified RDB or coverage is incomplete; best-effort
// records partial coverage and exits Degraded.
const (
	consistencyStrict     = "strict"
	consistencyBestEffort = "best-effort"
)

// errIncompleteCoverage is the strict-mode failure when the union of captured
// shard slots does not cover all 16384 slots (06 §4.4).
var errIncompleteCoverage = errors.New("incomplete slot coverage")

// shardPrimary is the minimal per-shard input the backup loop needs: the live
// primary's identity, dial address, slots and replication offset, resolved from
// CLUSTER NODES / INFO — never from labels (06 §1.3).
type shardPrimary struct {
	index      int
	nodeID     string
	addr       string
	slots      []valkey.SlotRange
	replOffset int64
}

// rdbSourceFactory builds an RDBSource for a shard primary. Production wires the
// SYNC source; tests inject a fake so the upload/manifest logic is exercised
// without a live engine.
type rdbSourceFactory func(sp shardPrimary) RDBSource

// backupOptions is the fully-resolved input to runBackup. The controller's Job
// builder populates these from the PerconaValkeyBackup CR + cluster spec; the
// store and shard topology are pre-resolved so runBackup is pure orchestration.
type backupOptions struct {
	cluster       string
	backupName    string
	mode          string
	crVersion     string
	engineVersion string
	consistency   string
	store         backup.ArtifactStore
	shards        []shardPrimary
	newRDBSource  rdbSourceFactory
	// now is overridable for deterministic manifest timestamps in tests.
	now func() time.Time
}

// runBackup snapshots every shard's primary in ascending shardIndex order,
// uploads each RDB, asserts full slot coverage, and writes the manifest LAST
// (06 §4.3-§4.5). On strict-mode shard failure or incomplete coverage it returns
// an error and writes NO manifest, so the set is recognizably incomplete.
func runBackup(ctx context.Context, o backupOptions) error {
	if o.store == nil {
		return fmt.Errorf("backup: nil store")
	}
	if o.newRDBSource == nil {
		return fmt.Errorf("backup: nil RDB source factory")
	}
	shards := slices.Clone(o.shards)
	slices.SortFunc(shards, func(a, b shardPrimary) int { return a.index - b.index })

	man := backup.Manifest{
		Cluster:       o.cluster,
		BackupName:    o.backupName,
		Mode:          o.mode,
		CRVersion:     o.crVersion,
		EngineVersion: o.engineVersion,
		Consistency:   o.consistency,
		CreatedAt:     o.timestamp(),
	}

	var captured []valkey.SlotRange
	for _, sp := range shards {
		frag, capturedRanges, err := snapshotShard(ctx, o, sp)
		if err != nil {
			if o.consistency != consistencyBestEffort {
				return fmt.Errorf("shard %d: %w", sp.index, err)
			}
			man.SlotCoverage = string(coveragePartial)
			continue
		}
		man.Shards = append(man.Shards, frag)
		captured = append(captured, capturedRanges...)
	}

	switch assertCoverage(captured) {
	case coverageComplete:
		man.SlotCoverage = string(coverageComplete)
	case coveragePartial:
		if o.consistency != consistencyBestEffort {
			return errIncompleteCoverage
		}
		man.SlotCoverage = string(coveragePartial)
	}

	// Manifest LAST — its presence is the durable "set complete" marker (06 §4.5).
	if err := backup.WriteManifest(ctx, o.store, backup.ManifestKey(o.cluster, o.backupName), man); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// snapshotShard captures one shard: it opens the RDB stream from the primary,
// streams it to <set>/shard-<i>/dump.rdb while computing SHA-256 incrementally
// (never buffering the whole RDB, 06 §4.8), and returns the manifest fragment
// plus the slot ranges captured for the coverage union.
func snapshotShard(ctx context.Context, o backupOptions, sp shardPrimary) (backup.ShardManifest, []valkey.SlotRange, error) {
	src := o.newRDBSource(sp)
	stream, length, err := src.Open(ctx)
	if err != nil {
		return backup.ShardManifest{}, nil, fmt.Errorf("open RDB source: %w", err)
	}
	defer func() { _ = stream.Close() }()

	hasher := sha256.New()
	counter := &countingReader{}
	tee := io.TeeReader(io.TeeReader(stream, hasher), counter)

	key := backup.ShardRDBKey(o.cluster, o.backupName, sp.index)
	if err = o.store.Put(ctx, key, tee, length); err != nil {
		return backup.ShardManifest{}, nil, fmt.Errorf("upload %s: %w", key, err)
	}

	frag := backup.ShardManifest{
		Index:            sp.index,
		PrimaryNodeID:    sp.nodeID,
		SlotRanges:       valkey.FormatSlotRanges(sp.slots),
		RDBKey:           backup.ShardRDBRelKey(sp.index),
		SizeBytes:        counter.n,
		SHA256:           hex.EncodeToString(hasher.Sum(nil)),
		MasterReplOffset: sp.replOffset,
	}
	return frag, sp.slots, nil
}

// coverageVerdict is the result of asserting the captured-slot union.
type coverageVerdict string

const (
	// coverageComplete means the union covers all 16384 slots with no gap/overlap.
	coverageComplete coverageVerdict = "complete"
	// coveragePartial means the union has a gap or overlap.
	coveragePartial coverageVerdict = "partial"
)

// assertCoverage reports whether the union of captured slot ranges covers
// exactly 0..16383 with no gap and no overlap (06 §4.4). Overlap is detected by
// counting: a complete, disjoint cover of 16384 slots has exactly 16384 slots
// after sorting with no two ranges touching the same slot.
func assertCoverage(ranges []valkey.SlotRange) coverageVerdict {
	if len(ranges) == 0 {
		return coveragePartial
	}
	sorted := slices.Clone(ranges)
	slices.SortFunc(sorted, func(a, b valkey.SlotRange) int {
		if a.Start != b.Start {
			return a.Start - b.Start
		}
		return a.End - b.End
	})
	// Must start at slot 0.
	if sorted[0].Start != 0 {
		return coveragePartial
	}
	next := 0
	for _, r := range sorted {
		if r.Start != next { // gap (r.Start > next) or overlap (r.Start < next)
			return coveragePartial
		}
		if r.End < r.Start {
			return coveragePartial
		}
		next = r.End + 1
	}
	if next != valkey.TotalSlots { // must end exactly at slot 16383
		return coveragePartial
	}
	return coverageComplete
}

// timestamp returns the RFC3339 UTC manifest-write time, using the overridable
// clock for deterministic tests.
func (o backupOptions) timestamp() string {
	now := time.Now
	if o.now != nil {
		now = o.now
	}
	return now().UTC().Format(time.RFC3339)
}

// countingReader counts the bytes that flow through it (the uploaded RDB size),
// without buffering them.
type countingReader struct{ n int64 }

func (c *countingReader) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
