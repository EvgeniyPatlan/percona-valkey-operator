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

	corev1 "k8s.io/api/core/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
)

// Event type aliases (k8s Event Normal/Warning) used with the EventRecorder.
const (
	eventNormal  = corev1.EventTypeNormal
	eventWarning = corev1.EventTypeWarning
)

// Backup state reasons (06 §10 vocabulary, machine-readable, surfaced in
// status.stateDescription). The v1alpha1 PerconaValkeyBackupStatus has no
// conditions array (06 §3.2 wants Initialized/Running/Uploaded/Verified/Complete
// conditions and a Degraded state; both are doc-03 reconciliation items, OPEN
// QUESTION Q3). The phase machine therefore uses status.state as the single
// source of truth and records the human-readable reason in stateDescription —
// loud, never silent — without inventing out-of-enum fields.
const (
	ReasonValidating       = "Validating"
	ReasonStorageError     = "StorageError"
	ReasonWaitingForLock   = "WaitingForLock"
	ReasonClusterNotReady  = "ClusterNotReady"
	ReasonJobRunning       = "JobRunning"
	ReasonJobCreateError   = "JobCreateError"
	ReasonManifestError    = "ManifestError"
	ReasonIncompleteCover  = "IncompleteCoverage"
	ReasonJobEvicted       = "JobEvicted"
	ReasonDeadlineExceeded = "DeadlineExceeded"
	ReasonSucceeded        = "BackupSucceeded"
	ReasonDegraded         = "BackupDegraded"
)

// Backup event reasons (06 §10 Event vocabulary).
const (
	EventBackupStarted        = "BackupStarted"
	EventBackupSucceeded      = "BackupSucceeded"
	EventBackupDegraded       = "BackupDegraded"
	EventBackupFailed         = "BackupFailed"
	EventBackupWaitingForLock = "BackupWaitingForLock"
	EventBackupJobEvicted     = "BackupJobEvicted"
	EventBackupSkippedOverlap = "BackupSkippedOverlap"
	EventArtifactsDeleted     = "ArtifactsDeleted"
	EventBackupCleanupFailed  = "BackupCleanupFailed"
	EventBackupScheduled      = "BackupScheduled"
)

// setState transitions the backup to a new state with a human-readable
// description. State only ever moves forward (the caller guards terminal states
// via isTerminal), so this is the single mutation point for status.state.
func setState(bk *valkeyv1alpha1.PerconaValkeyBackup, state valkeyv1alpha1.BackupState, description string) {
	bk.Status.State = state
	bk.Status.StateDescription = description
}

// isTerminal reports whether the backup is in a terminal state (Succeeded or
// Failed/Error) — the phase machine never leaves a terminal state.
func isTerminal(bk *valkeyv1alpha1.PerconaValkeyBackup) bool {
	return isTerminalState(bk.Status.State)
}

// isTerminalState reports whether a BackupState is terminal.
func isTerminalState(state valkeyv1alpha1.BackupState) bool {
	switch state {
	case valkeyv1alpha1.BackupStateSucceeded, valkeyv1alpha1.BackupStateFailed, valkeyv1alpha1.BackupStateError:
		return true
	default:
		return false
	}
}

// writeStatus persists the status subresource via the shared re-fetch+patch
// helper (04 §9 re-fetch-before-update).
func (r *Reconciler) writeStatus(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) error {
	return k8s.WriteStatus(ctx, r.Client, bk, func(b *valkeyv1alpha1.PerconaValkeyBackup) *valkeyv1alpha1.PerconaValkeyBackupStatus {
		return &b.Status
	})
}
