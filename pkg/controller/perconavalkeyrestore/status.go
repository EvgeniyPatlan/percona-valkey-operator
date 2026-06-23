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

package perconavalkeyrestore

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
)

// Event types (k8s Event Normal/Warning) used with the EventRecorder.
const (
	eventNormal  = corev1.EventTypeNormal
	eventWarning = corev1.EventTypeWarning
)

// Restore Event reasons (06 §10 vocabulary).
const (
	// EventRestoreStarted is emitted when source resolution begins.
	EventRestoreStarted = "RestoreStarted"
	// EventRestoreProvisioning is emitted when the target cluster is provisioned.
	EventRestoreProvisioning = "RestoreProvisioning"
	// EventRestoreSeeding is emitted when per-shard RDB seeding begins (06 §7.4).
	EventRestoreSeeding = "RestoreSeeding"
	// EventRestoreClusterFormed is emitted once the target cluster re-forms.
	EventRestoreClusterFormed = "RestoreClusterFormed"
	// EventRestoreSucceeded is emitted at proven full slot coverage.
	EventRestoreSucceeded = "RestoreSucceeded"
	// EventRestoreFailed is emitted on any terminal failure (Warning).
	EventRestoreFailed = "RestoreFailed"
)

// restorePhase is the conceptual restore phase from 06 §7.2
// (Pending→Provisioning→Seeding→Forming→Validating→Succeeded/Failed). The locked
// v1alpha1 RestoreState enum (03 §8.2) only carries ""/Starting/Running/
// Succeeded/Failed/Error, so the richer phase is tracked in this annotation and
// surfaced in status.stateDescription while status.state maps to the enum below.
// This keeps the CRD frozen (no API change in this leg) while implementing the
// full doc-06 phase machine (OPEN QUESTION Q3 — the richer status set is the
// intended shape; the API doc expansion is a separate cross-doc item).
type restorePhase string

const (
	// phasePending is the genesis phase before source resolution.
	phasePending restorePhase = "Pending"
	// phaseProvisioning provisions the target PerconaValkeyCluster (06 §7.4).
	phaseProvisioning restorePhase = "Provisioning"
	// phaseSeeding waits for each shard primary to seed its RDB (06 §7.4).
	phaseSeeding restorePhase = "Seeding"
	// phaseForming waits for the cluster controller to re-form topology (06 §7.5).
	phaseForming restorePhase = "Forming"
	// phaseValidating asserts full 16384-slot coverage before success (06 §7.5).
	phaseValidating restorePhase = "Validating"
	// phaseSucceeded is terminal success.
	phaseSucceeded restorePhase = "Succeeded"
	// phaseFailed is terminal failure.
	phaseFailed restorePhase = "Failed"
)

// annPhase carries the conceptual restorePhase across reconciles (the enum is too
// coarse — see restorePhase). annTargetCluster records the provisioned target
// cluster name for diagnostics (the doc-06 status.targetCluster, which is not in
// the locked CRD). annRestoredSlots records the validated coverage verdict
// (doc-06 status.restoredSlots). annRestoreStorage bridges the resolved named
// storage backend onto the target cluster so the cluster controller's restore-target
// seam can populate the required ValkeyNodeSpec.RestoreFrom.Storage for the seed
// init container (06 §7.4). All are operator-managed annotations, patched via
// client.MergeFrom so they never round-trip spec through defaults (M3 finding).
const (
	annPhase          = "valkey.percona.com/restore-phase"
	annTargetCluster  = "valkey.percona.com/restore-target-cluster"
	annRestoredSlots  = "valkey.percona.com/restore-restored-slots"
	annAllowPartial   = "valkey.percona.com/allow-partial-restore"
	annRestoreMarker  = "valkey.percona.com/restored-from"
	annRestoreStorage = "valkey.percona.com/restore-storage"
	annClusterTmpl    = "valkey.percona.com/restore-cluster-template"
	annSeedAppendonly = "valkey.percona.com/restore-seed-appendonly"
	// annSourceCluster carries the SOURCE cluster name (the manifest's clusterName)
	// the backup-set object keys are prefixed with. The restored-from marker records
	// only <restoreName>/<backupRef> for provenance and is "backupSource" for an
	// inline source, so the source cluster + backup names that derive the object keys
	// are stamped separately for the cluster controller to read into the per-node
	// RestoreSource (06 §7.4). Without them the seed init container cannot build the
	// VALKEY_BACKUP_CLUSTER/VALKEY_BACKUP_NAME that key the shard RDBs.
	annSourceCluster = "valkey.percona.com/restore-source-cluster"
	// annSourceBackup carries the SOURCE backup-set name (the manifest's backupName)
	// the shard RDBs live under; paired with annSourceCluster it derives the object
	// keys for the seed download.
	annSourceBackup = "valkey.percona.com/restore-source-backup"
)

// truthy reports whether an annotation value is an affirmative ("true"/"1"). It is
// the single place the operator interprets boolean-valued annotations so the
// accepted spellings stay consistent (and the literal is not repeated).
func truthy(v string) bool {
	return v == annValueTrue || v == "1"
}

// annValueTrue is the canonical affirmative annotation value.
const annValueTrue = "true"

// phaseToState maps a conceptual restorePhase onto the locked RestoreState enum
// (03 §8.2). Provisioning/Seeding/Forming are pre-terminal work the enum lumps
// under Starting; Validating maps to Running; the terminal phases map 1:1.
func phaseToState(p restorePhase) valkeyv1alpha1.RestoreState {
	switch p {
	case phasePending, phaseProvisioning, phaseSeeding, phaseForming:
		return valkeyv1alpha1.RestoreStateStarting
	case phaseValidating:
		return valkeyv1alpha1.RestoreStateRunning
	case phaseSucceeded:
		return valkeyv1alpha1.RestoreStateSucceeded
	case phaseFailed:
		return valkeyv1alpha1.RestoreStateFailed
	default:
		return valkeyv1alpha1.RestoreStateError
	}
}

// currentPhase reads the conceptual phase from the restore's annotation, defaulting
// to phasePending when unset (a freshly applied CR).
func currentPhase(rst *valkeyv1alpha1.PerconaValkeyRestore) restorePhase {
	if rst.Annotations == nil {
		return phasePending
	}
	if v, ok := rst.Annotations[annPhase]; ok && v != "" {
		return restorePhase(v)
	}
	return phasePending
}

// isTerminal reports whether the restore has reached a terminal phase, so the
// reconcile short-circuits without further work.
func isTerminal(rst *valkeyv1alpha1.PerconaValkeyRestore) bool {
	switch currentPhase(rst) {
	case phaseSucceeded, phaseFailed:
		return true
	default:
		return false
	}
}

// setPhaseAnnotation patches the conceptual-phase annotation onto the restore via a
// MergeFrom patch (never a full Update — the M3 round-trip bug). Returns the patched
// object so the caller keeps a fresh copy.
func (r *Reconciler) setPhaseAnnotation(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore, kv map[string]string) error {
	base := rst.DeepCopy()
	if rst.Annotations == nil {
		rst.Annotations = map[string]string{}
	}
	for k, v := range kv {
		rst.Annotations[k] = v
	}
	if err := r.Patch(ctx, rst, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch restore annotations %s: %w", client.ObjectKeyFromObject(rst), err)
	}
	return nil
}

// writeStatus mirrors the conceptual phase onto the locked status.{state,
// stateDescription,completed} fields and persists the status subresource via the
// shared re-fetch+MergeFrom patch helper (04 §9). It is the single status mutation
// point so state can never disagree with the recorded phase.
func (r *Reconciler) writeStatus(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore, phase restorePhase, desc string) error {
	rst.Status.State = phaseToState(phase)
	rst.Status.StateDescription = desc
	if phase == phaseSucceeded && rst.Status.Completed == nil {
		now := metav1.Now()
		rst.Status.Completed = &now
	}
	return k8s.WriteStatus(ctx, r.Client, rst, func(o *valkeyv1alpha1.PerconaValkeyRestore) *valkeyv1alpha1.PerconaValkeyRestoreStatus {
		return &o.Status
	})
}

// advance records the new conceptual phase (annotation) and writes the projected
// status in one step. It is the only way the phase machine moves forward.
func (r *Reconciler) advance(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore, phase restorePhase, desc string) error {
	if err := r.setPhaseAnnotation(ctx, rst, map[string]string{annPhase: string(phase)}); err != nil {
		return err
	}
	return r.writeStatus(ctx, rst, phase, desc)
}

// fail drives the restore into the terminal Failed phase with a loud Warning Event
// and a descriptive message, returning the wrapped error so the reconcile surfaces
// it once (no requeue storm on a permanent failure). The partially-built target
// cluster is intentionally left for inspection (06 §9.3).
func (r *Reconciler) fail(ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore, reason string, cause error) error {
	r.recorder.Eventf(rst, nil, eventWarning, EventRestoreFailed, reason, "%s", cause.Error())
	if err := r.advance(ctx, rst, phaseFailed, fmt.Sprintf("%s: %s", reason, cause.Error())); err != nil {
		return fmt.Errorf("record restore failure (%s): %w", reason, err)
	}
	return nil
}
