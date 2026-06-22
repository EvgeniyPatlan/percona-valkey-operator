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
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// RunRetention enforces a schedule's keep/keepAge retention (06 §5.3, §6): it
// lists the schedule's terminal-success backups (Succeeded only — only completed,
// restorable sets count toward keep), keeps the newest keep / those within
// keepAge, and DELETES the surplus. Deletion is a plain client.Delete on the
// PerconaValkeyBackup; the percona.com/delete-backup finalizer then runs the
// manifest-first cleanup Job (06 §6.1) — retention never touches object storage
// directly. It returns the count deleted. Failed/Error backups are retained here
// (a separate, longer age threshold GC's them — out of this pass).
func (s *Scheduler) RunRetention(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, scheduleName string, now time.Time,
) (int, error) {
	sched := findSchedule(cluster, scheduleName)
	if sched == nil {
		return 0, nil
	}
	keepAge, err := parseKeepAge(cluster, scheduleName)
	if err != nil {
		return 0, err
	}
	if sched.Keep <= 0 && keepAge == 0 {
		return 0, nil // unlimited retention: nothing to GC
	}

	list := &valkeyv1alpha1.PerconaValkeyBackupList{}
	if err := s.client.List(ctx, list,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{naming.LabelCluster: cluster.Name, LabelBackupSchedule: scheduleName},
	); err != nil {
		return 0, fmt.Errorf("list backups for retention of schedule %q: %w", scheduleName, err)
	}

	surplus := selectSurplus(list.Items, sched.Keep, keepAge, now)
	deleted := 0
	for _, bk := range surplus {
		if err := s.client.Delete(ctx, bk); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue // already being deleted
			}
			return deleted, fmt.Errorf("delete surplus backup %q: %w", bk.Name, err)
		}
		deleted++
	}
	return deleted, nil
}

// parseKeepAge resolves the schedule's keepAge into a duration. The schedule type
// (03 §2.9) currently carries only keep; an optional spec.retention.keepAge on a
// backup is honoured if present (OPEN QUESTION Q2: schedule keep wins on
// behaviour). Returns 0 when no age policy applies.
func parseKeepAge(cluster *valkeyv1alpha1.PerconaValkeyCluster, scheduleName string) (time.Duration, error) {
	// BackupScheduleSpec has no keepAge field in v1alpha1 (only keep); age-based
	// retention is therefore a no-op until the API adds it. Kept as a seam so the
	// selection logic is age-aware and ready for the doc-03 reconciliation (Q2).
	_ = cluster
	_ = scheduleName
	return 0, nil
}

// selectSurplus returns the backups to delete for a keep/keepAge policy. Only
// Succeeded backups count toward keep; they are sorted by completion time
// descending (newest first), the newest keep are retained, and the rest are
// surplus. When keepAge>0, any backup older than keepAge is also surplus. The
// selection is deterministic (stable sort by completion then name) so retention
// never deletes a different set across reconciles (R10 over-deletion guard).
func selectSurplus(
	items []valkeyv1alpha1.PerconaValkeyBackup, keep int, keepAge time.Duration, now time.Time,
) []*valkeyv1alpha1.PerconaValkeyBackup {
	var counted []*valkeyv1alpha1.PerconaValkeyBackup
	for i := range items {
		bk := &items[i]
		if bk.DeletionTimestamp != nil && !bk.DeletionTimestamp.IsZero() {
			continue // already terminating
		}
		if bk.Status.State == valkeyv1alpha1.BackupStateSucceeded {
			counted = append(counted, bk)
		}
	}
	slices.SortStableFunc(counted, func(a, b *valkeyv1alpha1.PerconaValkeyBackup) int {
		ta, tb := completionTime(a), completionTime(b)
		if !ta.Equal(tb) {
			if ta.After(tb) {
				return -1 // newest first
			}
			return 1
		}
		// tie-break deterministically by name (descending)
		return -cmp.Compare(a.Name, b.Name)
	})

	var surplus []*valkeyv1alpha1.PerconaValkeyBackup
	for idx, bk := range counted {
		overCount := keep > 0 && idx >= keep
		tooOld := keepAge > 0 && now.Sub(completionTime(bk)) > keepAge
		if overCount || tooOld {
			surplus = append(surplus, bk)
		}
	}
	return surplus
}

// completionTime returns a backup's completion timestamp, falling back to its
// creation timestamp so a backup with no completion (shouldn't happen for
// Succeeded) still sorts sensibly.
func completionTime(bk *valkeyv1alpha1.PerconaValkeyBackup) time.Time {
	if bk.Status.Completed != nil {
		return bk.Status.Completed.Time
	}
	return bk.CreationTimestamp.Time
}
