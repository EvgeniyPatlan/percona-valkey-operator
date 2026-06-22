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

package perconavalkeybackup

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// Scheduled-backup labels/annotations (06 §5.1).
const (
	// LabelBackupSchedule names the schedule a generated backup belongs to, so the
	// retention pass can select a schedule's backups.
	LabelBackupSchedule = "valkey.percona.com/backup-schedule"
	// AnnScheduledAt records the intended UTC fire time of a generated backup.
	AnnScheduledAt = "valkey.percona.com/scheduled-at"
	// scheduledTimeLayout is the compact UTC stamp embedded in a generated
	// backup's name: <cluster>-<schedule>-<UTCstamp>.
	scheduledTimeLayout = "20060102150405"
)

// scheduleState tracks one (cluster, schedule.name) registration so the
// scheduler can compute the next fire deterministically and dedupe overlapping
// fires. It is keyed inside the Scheduler by clusterKey/scheduleName.
type scheduleState struct {
	spec     cron.Schedule
	cronText string
	// lastFire is the most recent fire time the scheduler has acted on (zero until
	// the first Tick). It anchors missed-fire catch-up: a Tick fires at most one
	// backup for the most recent elapsed slot (06 §5.2 forward-only catch-up).
	lastFire time.Time
}

// Scheduler is an in-operator cron registry for backup schedules (06 §5.1),
// keyed by (cluster, schedule.name). It is deliberately self-contained and
// clock-driven (no background goroutine): Sync reconciles the registry to a
// cluster's spec.backup.schedule and Tick(now) fires due schedules by creating
// owned PerconaValkeyBackup CRs. Driving it from an explicit clock makes
// scheduling fully deterministic in tests (fake clock) and ties the lifecycle to
// the caller's reconcile rather than free goroutines.
//
// NOTE on placement: doc 06 §5.1 / the M4 plan (GO-4.11) site this registry in
// the perconavalkeycluster controller's reconcile. This leg owns only the
// perconavalkeybackup package, so the registry is implemented here as a reusable,
// injectable component; wiring Sync/Tick into the cluster reconcile loop is a
// one-line cross-package follow-up owned by the cluster-controller leg (recorded
// as an OPEN QUESTION). The behaviour (owned backups, overlap-Forbid, at-most-one
// catch-up, retention) is fully implemented and tested here.
type Scheduler struct {
	client   client.Client
	scheme   *runtime.Scheme
	recorder events.EventRecorder
	mu       sync.Mutex
	// states maps clusterKey -> scheduleName -> registration.
	states map[string]map[string]*scheduleState
}

// NewScheduler returns an empty backup Scheduler bound to a client, scheme (for
// owner-refs on generated backups) and an event recorder (overlap/scheduled
// Events, 06 §5.2, §10). recorder may be nil in tests that do not assert events.
func NewScheduler(c client.Client, scheme *runtime.Scheme, recorder events.EventRecorder) *Scheduler {
	return &Scheduler{
		client:   c,
		scheme:   scheme,
		recorder: recorder,
		states:   map[string]map[string]*scheduleState{},
	}
}

// event emits an Event via the recorder if one is configured.
func (s *Scheduler) event(obj *valkeyv1alpha1.PerconaValkeyCluster, eventtype, reason, note string, args ...interface{}) {
	if s.recorder == nil {
		return
	}
	s.recorder.Eventf(obj, nil, eventtype, reason, "Schedule", note, args...)
}

// clusterKey identifies a cluster across namespaces (namespace/name).
func clusterKey(namespace, name string) string {
	return namespace + "/" + name
}

// Sync reconciles the registry to a cluster's spec.backup.schedule (06 §5.1):
// it parses every schedule's cron, upserts new/changed schedules, and removes
// schedules no longer present (or all of them when backup is disabled / the
// cluster is being deleted). It returns an error if any cron string is invalid
// so the caller surfaces a misconfigured schedule loudly. The lifecycle is tied
// to the cluster CR: a removed schedule stops firing immediately.
func (s *Scheduler) Sync(cluster *valkeyv1alpha1.PerconaValkeyCluster, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := clusterKey(cluster.Namespace, cluster.Name)
	disabled := !cluster.DeletionTimestamp.IsZero() || len(cluster.Spec.Backup.Schedule) == 0
	if disabled {
		delete(s.states, key)
		return nil
	}

	want := map[string]bool{}
	cur := s.states[key]
	if cur == nil {
		cur = map[string]*scheduleState{}
	}
	for _, sched := range cluster.Spec.Backup.Schedule {
		want[sched.Name] = true
		existing, ok := cur[sched.Name]
		if ok && existing.cronText == sched.Schedule {
			continue // unchanged
		}
		parsed, err := cron.ParseStandard(sched.Schedule)
		if err != nil {
			return fmt.Errorf("schedule %q: invalid cron %q: %w", sched.Name, sched.Schedule, err)
		}
		st := &scheduleState{spec: parsed, cronText: sched.Schedule}
		if ok {
			st.lastFire = existing.lastFire // preserve fire anchor across a cron edit
		} else {
			// New schedule: anchor lastFire at now so the FIRST due slot is the next
			// one after registration (no spurious immediate fire on registration).
			st.lastFire = now
		}
		cur[sched.Name] = st
	}
	for name := range cur {
		if !want[name] {
			delete(cur, name)
		}
	}
	s.states[key] = cur
	return nil
}

// Tick fires every schedule whose next slot has elapsed at or before now,
// creating one owned PerconaValkeyBackup per due schedule (06 §5.1). It applies
// the §5.2 policies: at-most-one catch-up (only the most recent elapsed slot
// fires, older missed slots are skipped) and overlap-Forbid (skip if a previous
// run of the same schedule is still non-terminal, emitting BackupSkippedOverlap).
// It returns the number of backups created. Driving Tick from an explicit clock
// makes scheduling deterministic.
func (s *Scheduler) Tick(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, now time.Time) (int, error) {
	s.mu.Lock()
	cur := s.states[clusterKey(cluster.Namespace, cluster.Name)]
	// Snapshot the due schedules under lock, then act without holding it.
	type due struct {
		name string
		slot time.Time
		st   *scheduleState
	}
	var dues []due
	names := make([]string, 0, len(cur))
	for name := range cur {
		names = append(names, name)
	}
	slices.Sort(names) // deterministic order
	for _, name := range names {
		st := cur[name]
		if slot, ok := mostRecentElapsedSlot(st, now); ok {
			dues = append(dues, due{name: name, slot: slot, st: st})
		}
	}
	s.mu.Unlock()

	created := 0
	for _, d := range dues {
		sched := findSchedule(cluster, d.name)
		if sched == nil {
			continue
		}
		res, err := s.fireSchedule(ctx, cluster, *sched, d.slot)
		if err != nil {
			return created, err
		}
		// Advance the fire anchor regardless of overlap-skip so a skipped slot is
		// not re-attempted (forward-only, 06 §5.2).
		s.mu.Lock()
		if st := s.scheduleStateLocked(cluster, d.name); st != nil {
			st.lastFire = d.slot
		}
		s.mu.Unlock()
		switch res {
		case fireCreated:
			created++
			s.event(cluster, eventNormal, EventBackupScheduled,
				"created scheduled backup for schedule %q (slot %s)", d.name, d.slot.UTC().Format(time.RFC3339))
		case fireOverlap:
			s.event(cluster, eventNormal, EventBackupSkippedOverlap,
				"skipped scheduled backup for schedule %q: a previous run is still active (Forbid)", d.name)
		case fireDeduped:
			// duplicate fire across leader change/jitter — silently deduped.
		}
	}
	return created, nil
}

// scheduleStateLocked returns the live scheduleState for (cluster, name) while
// the caller holds s.mu, or nil if it has since been removed.
func (s *Scheduler) scheduleStateLocked(cluster *valkeyv1alpha1.PerconaValkeyCluster, name string) *scheduleState {
	cur := s.states[clusterKey(cluster.Namespace, cluster.Name)]
	if cur == nil {
		return nil
	}
	return cur[name]
}

// mostRecentElapsedSlot returns the most recent cron slot at or before now that
// is strictly after the schedule's lastFire, plus ok=true when at least one slot
// has elapsed (06 §5.2 forward-only at-most-one catch-up). Older missed slots are
// collapsed to this single slot.
func mostRecentElapsedSlot(st *scheduleState, now time.Time) (time.Time, bool) {
	var last time.Time
	anchor := st.lastFire
	for {
		next := st.spec.Next(anchor)
		if next.After(now) {
			break
		}
		last = next
		anchor = next
	}
	if last.IsZero() {
		return time.Time{}, false
	}
	return last, true
}

// fireResult is the outcome of attempting to fire one schedule slot.
type fireResult int

const (
	// fireCreated means a new owned backup was created.
	fireCreated fireResult = iota
	// fireOverlap means the fire was skipped because a previous run is still
	// active (overlap-Forbid, 06 §5.2) — the caller emits BackupSkippedOverlap.
	fireOverlap
	// fireDeduped means the deterministic-named backup for this slot already
	// exists (duplicate fire across leader change / jitter, R9) — no event.
	fireDeduped
)

// fireSchedule creates one owned PerconaValkeyBackup for a due schedule unless a
// previous run of the same schedule is still non-terminal (overlap-Forbid,
// 06 §5.2). The fireResult tells the caller which Event (if any) to emit.
func (s *Scheduler) fireSchedule(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	sched valkeyv1alpha1.BackupScheduleSpec, slot time.Time,
) (fireResult, error) {
	running, err := s.scheduleHasActiveRun(ctx, cluster, sched.Name)
	if err != nil {
		return fireOverlap, err
	}
	if running {
		return fireOverlap, nil
	}
	bk := scheduledBackup(cluster, sched, slot)
	if err := controllerutil.SetControllerReference(cluster, bk, s.scheme); err != nil {
		return fireOverlap, fmt.Errorf("set scheduled-backup owner-ref: %w", err)
	}
	if err := s.client.Create(ctx, bk); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Deterministic name already created this slot — dedupe (06 §5.2 / R9).
			return fireDeduped, nil
		}
		return fireOverlap, fmt.Errorf("create scheduled backup %q: %w", bk.Name, err)
	}
	return fireCreated, nil
}

// scheduleHasActiveRun reports whether the schedule has a non-terminal generated
// backup (overlap-Forbid input). It lists backups labelled with the schedule.
func (s *Scheduler) scheduleHasActiveRun(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, scheduleName string,
) (bool, error) {
	list := &valkeyv1alpha1.PerconaValkeyBackupList{}
	if err := s.client.List(ctx, list,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{naming.LabelCluster: cluster.Name, LabelBackupSchedule: scheduleName},
	); err != nil {
		return false, fmt.Errorf("list scheduled backups for %q: %w", scheduleName, err)
	}
	for i := range list.Items {
		bk := &list.Items[i]
		if bk.DeletionTimestamp != nil && !bk.DeletionTimestamp.IsZero() {
			continue
		}
		switch bk.Status.State {
		case valkeyv1alpha1.BackupStateSucceeded, valkeyv1alpha1.BackupStateFailed, valkeyv1alpha1.BackupStateError:
			continue
		default:
			return true, nil // New/Starting/Running == active
		}
	}
	return false, nil
}

// scheduledBackup builds the owned PerconaValkeyBackup for a fired schedule
// (06 §5.1): name <cluster>-<schedule>-<UTCstamp>, cluster/schedule labels, the
// scheduled-at annotation, and the schedule's storageName/type.
func scheduledBackup(
	cluster *valkeyv1alpha1.PerconaValkeyCluster, sched valkeyv1alpha1.BackupScheduleSpec, slot time.Time,
) *valkeyv1alpha1.PerconaValkeyBackup {
	labels := naming.Labels(cluster.Name, backupComponent)
	labels[LabelBackupSchedule] = sched.Name
	bkType := sched.Type
	if bkType == "" {
		bkType = valkeyv1alpha1.BackupTypeFull
	}
	return &valkeyv1alpha1.PerconaValkeyBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:        scheduledBackupName(cluster.Name, sched.Name, slot),
			Namespace:   cluster.Namespace,
			Labels:      labels,
			Annotations: map[string]string{AnnScheduledAt: slot.UTC().Format(time.RFC3339)},
		},
		Spec: valkeyv1alpha1.PerconaValkeyBackupSpec{
			ClusterName: cluster.Name,
			StorageName: sched.StorageName,
			Type:        bkType,
		},
	}
}

// scheduledBackupName is the deterministic generated-backup name
// <cluster>-<schedule>-<UTCstamp>; determinism dedupes duplicate fires across a
// leader change / clock jitter (06 §5.2 / R9).
func scheduledBackupName(cluster, schedule string, slot time.Time) string {
	return cluster + "-" + schedule + "-" + slot.UTC().Format(scheduledTimeLayout)
}

// findSchedule returns the named schedule from the cluster spec, or nil.
func findSchedule(cluster *valkeyv1alpha1.PerconaValkeyCluster, name string) *valkeyv1alpha1.BackupScheduleSpec {
	for i := range cluster.Spec.Backup.Schedule {
		if cluster.Spec.Backup.Schedule[i].Name == name {
			return &cluster.Spec.Backup.Schedule[i]
		}
	}
	return nil
}
