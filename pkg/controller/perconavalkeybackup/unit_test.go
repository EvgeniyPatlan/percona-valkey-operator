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

// These are pure-helper unit tests in the internal package so they can reach the
// unexported lease/retention/schedule/job math directly without standing up
// envtest. The reconcile-against-apiserver behaviour lives in the Ginkgo suite
// (suite_test.go + *_envtest_test.go), package perconavalkeybackup_test.
package perconavalkeybackup

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

func ptr[T any](v T) *T { return &v }

// scheduleStateFor builds a scheduleState for the pure slot-math tests.
func scheduleStateFor(t *testing.T, cronText string, lastFire time.Time) *scheduleState {
	t.Helper()
	parsed, err := cron.ParseStandard(cronText)
	if err != nil {
		t.Fatalf("parse cron %q: %v", cronText, err)
	}
	return &scheduleState{spec: parsed, cronText: cronText, lastFire: lastFire}
}

// TestLeaseHeldByOther exercises the fail-open expiry math (06 §4.7): a fresh
// Lease held by another blocks; an expired one fails open (free); our own Lease
// is held-by-us.
func TestLeaseHeldByOther(t *testing.T) {
	base := time.Date(2026, 6, 22, 2, 0, 0, 0, time.UTC)
	dur := int32(30)
	mk := func(holder string, renew time.Time) *coordinationv1.Lease {
		return &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			RenewTime:            &metav1.MicroTime{Time: renew},
		}}
	}
	tests := []struct {
		name     string
		lease    *coordinationv1.Lease
		holder   string
		now      time.Time
		wantHeld bool
		wantByUs bool
	}{
		{
			name:     "fresh held by other blocks",
			lease:    mk("restore/ns/r1", base),
			holder:   "backup/ns/b1",
			now:      base.Add(5 * time.Second),
			wantHeld: true, wantByUs: false,
		},
		{
			name:     "fresh held by us",
			lease:    mk("backup/ns/b1", base),
			holder:   "backup/ns/b1",
			now:      base.Add(5 * time.Second),
			wantHeld: true, wantByUs: true,
		},
		{
			name:     "expired fails open (free)",
			lease:    mk("restore/ns/r1", base),
			holder:   "backup/ns/b1",
			now:      base.Add(31 * time.Second),
			wantHeld: false, wantByUs: false,
		},
		{
			name:     "no holder is free",
			lease:    &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{RenewTime: &metav1.MicroTime{Time: base}}},
			holder:   "backup/ns/b1",
			now:      base,
			wantHeld: false, wantByUs: false,
		},
		{
			name:     "no renewTime is free",
			lease:    &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr("restore/ns/r1")}},
			holder:   "backup/ns/b1",
			now:      base,
			wantHeld: false, wantByUs: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			held, byUs := leaseHeldByOther(tc.lease, tc.holder, tc.now)
			if held != tc.wantHeld || byUs != tc.wantByUs {
				t.Fatalf("leaseHeldByOther=(%v,%v) want (%v,%v)", held, byUs, tc.wantHeld, tc.wantByUs)
			}
		})
	}
}

// TestLeaseName pins the per-cluster lock name shared with the control plane.
func TestLeaseName(t *testing.T) {
	if got := LeaseName("prod"); got != "valkey-prod-backup-lock" {
		t.Fatalf("LeaseName=%q", got)
	}
}

// TestSelectSurplus proves keep-N retention deletes the oldest Succeeded backups
// only (06 §5.3): keep=2 over 4 succeeded => 2 oldest selected; Failed never
// counts; terminating skipped.
func TestSelectSurplus(t *testing.T) {
	base := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	mk := func(name string, state valkeyv1alpha1.BackupState, completed time.Time, terminating bool) valkeyv1alpha1.PerconaValkeyBackup {
		bk := valkeyv1alpha1.PerconaValkeyBackup{}
		bk.Name = name
		bk.Status.State = state
		bk.Status.Completed = &metav1.Time{Time: completed}
		if terminating {
			now := metav1.NewTime(base)
			bk.DeletionTimestamp = &now
		}
		return bk
	}
	items := []valkeyv1alpha1.PerconaValkeyBackup{
		mk("b1", valkeyv1alpha1.BackupStateSucceeded, base.Add(1*time.Hour), false),
		mk("b2", valkeyv1alpha1.BackupStateSucceeded, base.Add(2*time.Hour), false),
		mk("b3", valkeyv1alpha1.BackupStateSucceeded, base.Add(3*time.Hour), false),
		mk("b4", valkeyv1alpha1.BackupStateSucceeded, base.Add(4*time.Hour), false),
		mk("bf", valkeyv1alpha1.BackupStateFailed, base.Add(5*time.Hour), false),
		mk("bt", valkeyv1alpha1.BackupStateSucceeded, base.Add(6*time.Hour), true),
	}
	surplus := selectSurplus(items, 2, 0, base.Add(10*time.Hour))
	if len(surplus) != 2 {
		t.Fatalf("got %d surplus, want 2: %v", len(surplus), names(surplus))
	}
	// Newest two (b4,b3) kept; oldest two succeeded (b1,b2) deleted.
	got := map[string]bool{}
	for _, s := range surplus {
		got[s.Name] = true
	}
	if !got["b1"] || !got["b2"] {
		t.Fatalf("expected b1,b2 surplus; got %v", names(surplus))
	}
}

// TestSelectSurplusKeepAge proves age-based retention selects too-old backups.
func TestSelectSurplusKeepAge(t *testing.T) {
	base := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	mk := func(name string, completed time.Time) valkeyv1alpha1.PerconaValkeyBackup {
		bk := valkeyv1alpha1.PerconaValkeyBackup{}
		bk.Name = name
		bk.Status.State = valkeyv1alpha1.BackupStateSucceeded
		bk.Status.Completed = &metav1.Time{Time: completed}
		return bk
	}
	items := []valkeyv1alpha1.PerconaValkeyBackup{
		mk("old", base),
		mk("new", base.Add(9*time.Hour)),
	}
	surplus := selectSurplus(items, 0, 2*time.Hour, base.Add(10*time.Hour))
	if len(surplus) != 1 || surplus[0].Name != "old" {
		t.Fatalf("keepAge surplus=%v want [old]", names(surplus))
	}
}

func names(s []*valkeyv1alpha1.PerconaValkeyBackup) []string {
	out := make([]string, 0, len(s))
	for _, b := range s {
		out = append(out, b.Name)
	}
	return out
}

// TestMostRecentElapsedSlot proves at-most-one catch-up: a schedule with many
// missed slots collapses to the single most recent elapsed slot (06 §5.2).
func TestMostRecentElapsedSlot(t *testing.T) {
	st := scheduleStateFor(t, "*/5 * * * *", time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC))
	// 22 minutes later: slots at :05,:10,:15,:20 elapsed -> most recent is :20.
	now := time.Date(2026, 6, 22, 0, 22, 0, 0, time.UTC)
	slot, ok := mostRecentElapsedSlot(st, now)
	if !ok {
		t.Fatalf("expected an elapsed slot")
	}
	want := time.Date(2026, 6, 22, 0, 20, 0, 0, time.UTC)
	if !slot.Equal(want) {
		t.Fatalf("slot=%v want %v", slot, want)
	}
}

// TestMostRecentElapsedSlotNone proves no slot fires before the first tick.
func TestMostRecentElapsedSlotNone(t *testing.T) {
	st := scheduleStateFor(t, "0 2 * * *", time.Date(2026, 6, 22, 1, 0, 0, 0, time.UTC))
	now := time.Date(2026, 6, 22, 1, 30, 0, 0, time.UTC) // before 02:00
	if _, ok := mostRecentElapsedSlot(st, now); ok {
		t.Fatalf("expected no elapsed slot before 02:00")
	}
}

// TestScheduledBackupName pins the deterministic generated-backup name (R9 dedup).
func TestScheduledBackupName(t *testing.T) {
	slot := time.Date(2026, 6, 22, 2, 0, 0, 0, time.UTC)
	if got := scheduledBackupName("prod", "nightly", slot); got != "prod-nightly-20260622020000" {
		t.Fatalf("scheduledBackupName=%q", got)
	}
}

// TestStorageCoordinates covers the discriminated-union flattening for each
// backend plus the missing-sub-object error.
func TestStorageCoordinates(t *testing.T) {
	s3 := valkeyv1alpha1.BackupStorageSpec{Type: valkeyv1alpha1.BackupStorageS3, S3: &valkeyv1alpha1.BackupStorageS3Spec{
		Bucket: "b", Prefix: "p", CredentialsSecret: "s3creds",
	}}
	scheme, bucket, prefix, creds, err := storageCoordinates(s3)
	if err != nil || scheme != backup.SchemeS3 || bucket != "b" || prefix != "p" || creds != "s3creds" {
		t.Fatalf("s3 coords=(%q,%q,%q,%q) err=%v", scheme, bucket, prefix, creds, err)
	}
	az := valkeyv1alpha1.BackupStorageSpec{Type: valkeyv1alpha1.BackupStorageAzure, Azure: &valkeyv1alpha1.BackupStorageAzureSpec{
		Container: "c", CredentialsSecret: "azcreds",
	}}
	scheme, bucket, _, creds, err = storageCoordinates(az)
	if err != nil || scheme != backup.SchemeAzure || bucket != "c" || creds != "azcreds" {
		t.Fatalf("azure coords=(%q,%q,%q) err=%v", scheme, bucket, creds, err)
	}
	gcs := valkeyv1alpha1.BackupStorageSpec{Type: valkeyv1alpha1.BackupStorageGCS, GCS: &valkeyv1alpha1.BackupStorageGCSSpec{
		Bucket: "g", Prefix: "gp", CredentialsSecret: "gcscreds",
	}}
	scheme, bucket, prefix, creds, err = storageCoordinates(gcs)
	if err != nil || scheme != backup.SchemeGCS || bucket != "g" || prefix != "gp" || creds != "gcscreds" {
		t.Fatalf("gcs coords=(%q,%q,%q,%q) err=%v", scheme, bucket, prefix, creds, err)
	}
	fsSpec := valkeyv1alpha1.BackupStorageSpec{Type: valkeyv1alpha1.BackupStorageFilesystem}
	scheme, _, _, creds, err = storageCoordinates(fsSpec)
	if err != nil || scheme != backup.SchemePVC || creds != "" {
		t.Fatalf("filesystem coords scheme=%q creds=%q err=%v", scheme, creds, err)
	}

	bad := valkeyv1alpha1.BackupStorageSpec{Type: valkeyv1alpha1.BackupStorageS3}
	if _, _, _, _, err := storageCoordinates(bad); err == nil {
		t.Fatalf("expected error for s3 type with nil s3 sub-object")
	}
	if _, _, _, _, err := storageCoordinates(valkeyv1alpha1.BackupStorageSpec{Type: "bogus"}); err == nil {
		t.Fatalf("expected error for unknown storage type")
	}
}

// TestJobFailureReason maps Failed-Job conditions to backup failure reasons.
func TestJobFailureReason(t *testing.T) {
	deadline := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded},
	}}}
	if r, _ := jobFailureReason(deadline); r != ReasonDeadlineExceeded {
		t.Fatalf("deadline reason=%q", r)
	}
	withMsg := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "X", Message: "boom"},
	}}}
	if r, m := jobFailureReason(withMsg); r != ReasonJobCreateError || m == "" {
		t.Fatalf("withMsg reason=%q msg=%q", r, m)
	}
	noCond := &batchv1.Job{Status: batchv1.JobStatus{Failed: 3}}
	if r, _ := jobFailureReason(noCond); r != ReasonJobCreateError {
		t.Fatalf("noCond reason=%q", r)
	}
}

// TestGCSRequiredCredKeys pins the GCS service-account key presence check.
func TestGCSRequiredCredKeys(t *testing.T) {
	if got := requiredCredKeys(valkeyv1alpha1.BackupStorageGCS); len(got) != 1 || got[0] != "GOOGLE_APPLICATION_CREDENTIALS_JSON" {
		t.Fatalf("gcs keys=%v", got)
	}
	if got := requiredCredKeys(valkeyv1alpha1.BackupStorageAzure); len(got) != 2 {
		t.Fatalf("azure keys=%v", got)
	}
}

// TestIsTerminalState covers the terminal-state classification.
func TestIsTerminalState(t *testing.T) {
	for _, s := range []valkeyv1alpha1.BackupState{
		valkeyv1alpha1.BackupStateSucceeded, valkeyv1alpha1.BackupStateFailed, valkeyv1alpha1.BackupStateError,
	} {
		if !isTerminalState(s) {
			t.Fatalf("%q should be terminal", s)
		}
	}
	for _, s := range []valkeyv1alpha1.BackupState{
		valkeyv1alpha1.BackupStateNew, valkeyv1alpha1.BackupStateStarting, valkeyv1alpha1.BackupStateRunning,
	} {
		if isTerminalState(s) {
			t.Fatalf("%q should not be terminal", s)
		}
	}
}

// TestRequiredCredKeys pins the per-backend Secret key presence checks (06 §8.2).
func TestRequiredCredKeys(t *testing.T) {
	if got := requiredCredKeys(valkeyv1alpha1.BackupStorageS3); len(got) != 2 {
		t.Fatalf("s3 keys=%v", got)
	}
	if got := requiredCredKeys(valkeyv1alpha1.BackupStorageFilesystem); got != nil {
		t.Fatalf("filesystem should need no creds, got %v", got)
	}
}

// TestJobEvicted proves an evicted Failed Job is recognised (06 §4.8) and a plain
// deadline failure is not.
func TestJobEvicted(t *testing.T) {
	evicted := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", Message: "Pod was evicted due to node pressure"},
	}}}
	if !jobEvicted(evicted) {
		t.Fatalf("expected eviction detected from message")
	}
	deadline := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, Message: "Job was active longer than specified deadline"},
	}}}
	if jobEvicted(deadline) {
		t.Fatalf("plain deadline must not be flagged as eviction")
	}
}

// TestHydrateFromManifest proves manifest fields map into the backup status.
func TestHydrateFromManifest(t *testing.T) {
	bk := &valkeyv1alpha1.PerconaValkeyBackup{}
	man := backup.Manifest{
		EngineVersion: "9.0.0",
		SlotCoverage:  "complete",
		Shards: []backup.ShardManifest{
			{Index: 0, SlotRanges: "0-8191", RDBKey: "shard-0/dump.rdb", SizeBytes: 100, SHA256: "abc"},
			{Index: 1, SlotRanges: "8192-16383", RDBKey: "shard-1/dump.rdb", SizeBytes: 200, SHA256: "def"},
		},
	}
	hydrateFromManifest(bk, man)
	if bk.Status.ValkeyVersion != "9.0.0" {
		t.Fatalf("version=%q", bk.Status.ValkeyVersion)
	}
	if bk.Status.SlotCoverage != valkeyv1alpha1.SlotCoverageComplete {
		t.Fatalf("coverage=%q", bk.Status.SlotCoverage)
	}
	if len(bk.Status.Shards) != 2 || bk.Status.Shards[1].RDBObject != "shard-1/dump.rdb" {
		t.Fatalf("shards=%+v", bk.Status.Shards)
	}
}
