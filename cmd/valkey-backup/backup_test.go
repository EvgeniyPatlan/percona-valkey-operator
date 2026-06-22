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
	"io"
	"strings"
	"testing"
	"time"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// fakeRDBSource is a hermetic RDBSource yielding fixed bytes, so the backup
// upload/manifest logic is exercised without a live Valkey (CR-10 seam).
type fakeRDBSource struct {
	data    []byte
	openErr error
}

func (f *fakeRDBSource) Open(_ context.Context) (io.ReadCloser, int64, error) {
	if f.openErr != nil {
		return nil, 0, f.openErr
	}
	return io.NopCloser(strings.NewReader(string(f.data))), int64(len(f.data)), nil
}

// threeShardPrimaries returns a complete 3-shard 0..16383 cover.
func threeShardPrimaries() []shardPrimary {
	return []shardPrimary{
		{index: 0, nodeID: "node0", addr: "10.0.0.1:6379", slots: []valkey.SlotRange{{Start: 0, End: 5460}}, replOffset: 100},
		{index: 1, nodeID: "node1", addr: "10.0.0.2:6379", slots: []valkey.SlotRange{{Start: 5461, End: 10922}}, replOffset: 200},
		{index: 2, nodeID: "node2", addr: "10.0.0.3:6379", slots: []valkey.SlotRange{{Start: 10923, End: 16383}}, replOffset: 300},
	}
}

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 6, 22, 2, 0, 0, 0, time.UTC) }
}

func TestRunBackupHappyPath(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	shards := threeShardPrimaries()
	payloads := map[int][]byte{0: []byte("rdb-shard-0"), 1: []byte("rdb-shard-1"), 2: []byte("rdb-shard-2")}

	o := backupOptions{
		cluster:       "prod",
		backupName:    "prod-bk",
		mode:          "cluster",
		engineVersion: "9.0.0",
		consistency:   consistencyStrict,
		store:         store,
		shards:        shards,
		now:           fixedClock(),
		newRDBSource: func(sp shardPrimary) RDBSource {
			return &fakeRDBSource{data: payloads[sp.index]}
		},
	}
	if err := runBackup(ctx, o); err != nil {
		t.Fatalf("runBackup: %v", err)
	}

	// Manifest is written LAST: every Put op before the manifest is a shard RDB.
	ops := store.Ops()
	if len(ops) == 0 {
		t.Fatalf("no ops recorded")
	}
	last := ops[len(ops)-1]
	if last.Kind != backup.FakeOpPut || last.Key != backup.ManifestKey("prod", "prod-bk") {
		t.Fatalf("last op = %+v, want Put manifest", last)
	}
	for _, op := range ops[:len(ops)-1] {
		if op.Key == backup.ManifestKey("prod", "prod-bk") {
			t.Fatalf("manifest written before a shard RDB (op %+v)", op)
		}
	}

	// Manifest content: coverage complete, 3 shards, correct sha256 + size.
	man, err := backup.ReadManifest(ctx, store, backup.ManifestKey("prod", "prod-bk"))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if man.SlotCoverage != string(coverageComplete) {
		t.Fatalf("slotCoverage = %q, want complete", man.SlotCoverage)
	}
	if len(man.Shards) != 3 {
		t.Fatalf("shards = %d, want 3", len(man.Shards))
	}
	if man.CreatedAt != "2026-06-22T02:00:00Z" {
		t.Fatalf("createdAt = %q", man.CreatedAt)
	}
	for _, frag := range man.Shards {
		wantSum := sha256.Sum256(payloads[frag.Index])
		if frag.SHA256 != hex.EncodeToString(wantSum[:]) {
			t.Errorf("shard %d sha256 = %q, want %x", frag.Index, frag.SHA256, wantSum)
		}
		if frag.SizeBytes != int64(len(payloads[frag.Index])) {
			t.Errorf("shard %d size = %d, want %d", frag.Index, frag.SizeBytes, len(payloads[frag.Index]))
		}
		if frag.RDBKey != backup.ShardRDBRelKey(frag.Index) {
			t.Errorf("shard %d rdbKey = %q, want %q", frag.Index, frag.RDBKey, backup.ShardRDBRelKey(frag.Index))
		}
		if frag.MasterReplOffset == 0 {
			t.Errorf("shard %d masterReplOffset not recorded", frag.Index)
		}
	}
}

func TestRunBackupSortsShardsAscending(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	shards := threeShardPrimaries()
	// Shuffle input order; runBackup must still produce ascending shard manifest.
	shards[0], shards[2] = shards[2], shards[0]

	o := backupOptions{
		cluster: "c", backupName: "b", consistency: consistencyStrict, store: store, shards: shards,
		now:          fixedClock(),
		newRDBSource: func(_ shardPrimary) RDBSource { return &fakeRDBSource{data: []byte("x")} },
	}
	if err := runBackup(ctx, o); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	man, _ := backup.ReadManifest(ctx, store, backup.ManifestKey("c", "b"))
	for i, frag := range man.Shards {
		if frag.Index != i {
			t.Fatalf("manifest shard[%d].Index = %d, want %d (not ascending)", i, frag.Index, i)
		}
	}
}

func TestRunBackupStrictShardFailureNoManifest(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	shards := threeShardPrimaries()

	o := backupOptions{
		cluster: "c", backupName: "b", consistency: consistencyStrict, store: store, shards: shards,
		now: fixedClock(),
		newRDBSource: func(sp shardPrimary) RDBSource {
			if sp.index == 1 {
				return &fakeRDBSource{openErr: errors.New("primary unreachable")}
			}
			return &fakeRDBSource{data: []byte("ok")}
		},
	}
	err := runBackup(ctx, o)
	if err == nil {
		t.Fatalf("runBackup(strict, shard fails) = nil error, want failure")
	}
	// No manifest may exist — the set is recognizably incomplete (06 §4.5).
	if ok, _ := store.Exists(ctx, backup.ManifestKey("c", "b")); ok {
		t.Fatalf("manifest written despite strict shard failure")
	}
}

func TestRunBackupBestEffortShardSkipPartial(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	shards := threeShardPrimaries()

	o := backupOptions{
		cluster: "c", backupName: "b", consistency: consistencyBestEffort, store: store, shards: shards,
		now: fixedClock(),
		newRDBSource: func(sp shardPrimary) RDBSource {
			if sp.index == 1 {
				return &fakeRDBSource{openErr: errors.New("primary unreachable")}
			}
			return &fakeRDBSource{data: []byte("ok")}
		},
	}
	if err := runBackup(ctx, o); err != nil {
		t.Fatalf("runBackup(best-effort) = %v, want success with partial", err)
	}
	man, err := backup.ReadManifest(ctx, store, backup.ManifestKey("c", "b"))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if man.SlotCoverage != string(coveragePartial) {
		t.Fatalf("slotCoverage = %q, want partial", man.SlotCoverage)
	}
	if len(man.Shards) != 2 {
		t.Fatalf("shards = %d, want 2 (one skipped)", len(man.Shards))
	}
}

func TestRunBackupStrictIncompleteCoverageFails(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	// Only two of three shards present -> gap -> strict failure.
	shards := threeShardPrimaries()[:2]

	o := backupOptions{
		cluster: "c", backupName: "b", consistency: consistencyStrict, store: store, shards: shards,
		now:          fixedClock(),
		newRDBSource: func(_ shardPrimary) RDBSource { return &fakeRDBSource{data: []byte("x")} },
	}
	err := runBackup(ctx, o)
	if !errors.Is(err, errIncompleteCoverage) {
		t.Fatalf("runBackup(strict, gap) err = %v, want errIncompleteCoverage", err)
	}
	if ok, _ := store.Exists(ctx, backup.ManifestKey("c", "b")); ok {
		t.Fatalf("manifest written despite incomplete coverage")
	}
}

func TestRunBackupAbortedUploadLeavesNoManifest(t *testing.T) {
	ctx := context.Background()
	store := backup.NewFakeStore()
	// Fail the upload of shard 1's RDB mid-backup (simulates interrupted stream).
	store.SetPutError(func(key string) error {
		if strings.Contains(key, "shard-1/") {
			return errors.New("upload aborted")
		}
		return nil
	})
	o := backupOptions{
		cluster: "c", backupName: "b", consistency: consistencyStrict, store: store, shards: threeShardPrimaries(),
		now:          fixedClock(),
		newRDBSource: func(_ shardPrimary) RDBSource { return &fakeRDBSource{data: []byte("x")} },
	}
	if err := runBackup(ctx, o); err == nil {
		t.Fatalf("runBackup(upload abort) = nil error, want failure")
	}
	if ok, _ := store.Exists(ctx, backup.ManifestKey("c", "b")); ok {
		t.Fatalf("manifest written despite aborted upload")
	}
}

func TestRunBackupNilGuards(t *testing.T) {
	ctx := context.Background()
	if err := runBackup(ctx, backupOptions{}); err == nil {
		t.Fatalf("runBackup(nil store) = nil error, want failure")
	}
	if err := runBackup(ctx, backupOptions{store: backup.NewFakeStore()}); err == nil {
		t.Fatalf("runBackup(nil source factory) = nil error, want failure")
	}
}

func TestAssertCoverage(t *testing.T) {
	complete := []valkey.SlotRange{{Start: 0, End: 5460}, {Start: 5461, End: 10922}, {Start: 10923, End: 16383}}
	cases := []struct {
		name   string
		ranges []valkey.SlotRange
		want   coverageVerdict
	}{
		{"complete 3-shard", complete, coverageComplete},
		{"complete single", []valkey.SlotRange{{Start: 0, End: 16383}}, coverageComplete},
		{"empty", nil, coveragePartial},
		{"gap", []valkey.SlotRange{{Start: 0, End: 100}, {Start: 200, End: 16383}}, coveragePartial},
		{"does not start at 0", []valkey.SlotRange{{Start: 1, End: 16383}}, coveragePartial},
		{"does not reach end", []valkey.SlotRange{{Start: 0, End: 16382}}, coveragePartial},
		{
			"overlap",
			[]valkey.SlotRange{{Start: 0, End: 8000}, {Start: 5000, End: 16383}},
			coveragePartial,
		},
		{"out-of-order complete", []valkey.SlotRange{{Start: 10923, End: 16383}, {Start: 0, End: 5460}, {Start: 5461, End: 10922}}, coverageComplete},
		{"beyond max", []valkey.SlotRange{{Start: 0, End: 20000}}, coveragePartial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assertCoverage(tc.ranges); got != tc.want {
				t.Errorf("assertCoverage(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestCountingReader(t *testing.T) {
	c := &countingReader{}
	n, err := c.Write([]byte("hello"))
	if err != nil || n != 5 || c.n != 5 {
		t.Fatalf("countingReader = n%d c.n%d err%v", n, c.n, err)
	}
	_, _ = c.Write([]byte("!"))
	if c.n != 6 {
		t.Fatalf("countingReader cumulative = %d, want 6", c.n)
	}
}

func TestSnapshotShardOpenError(t *testing.T) {
	ctx := context.Background()
	o := backupOptions{
		cluster: "c", backupName: "b", store: backup.NewFakeStore(),
		newRDBSource: func(_ shardPrimary) RDBSource { return &fakeRDBSource{openErr: errors.New("nope")} },
	}
	_, _, err := snapshotShard(ctx, o, shardPrimary{index: 0})
	if err == nil {
		t.Fatalf("snapshotShard(open error) = nil error, want failure")
	}
}
