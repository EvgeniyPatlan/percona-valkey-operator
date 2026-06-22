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

package v1alpha1

import (
	"context"
	"fmt"

	"valkey.percona.com/percona-valkey-operator/pkg/version"
)

// defaultUpgradeSchedule is the cron used when smart updates are enabled but no
// schedule is supplied (03 §2.12).
const defaultUpgradeSchedule = "0 4 * * *"

// defaultVersionServiceEndpoint is the Percona-style version service endpoint
// used when smart updates are enabled but none is supplied (03 §2.12).
const defaultVersionServiceEndpoint = "https://check.percona.com"

// defaultActiveDeadlineSeconds is the default hard cap on a backup Job's runtime.
const defaultActiveDeadlineSeconds int64 = 3600

// usersSecretSuffix is appended to the cluster name to form the default Secret
// holding user/default passwords (<cluster>-users). Built inline (NOT
// naming.UsersSecretName) to honour the pkg/apis leaf rule (OQ-1.3).
const usersSecretSuffix = "-users"

// defaultDisabledCommands is the safe default rename-command-disable set applied
// when spec.disableCommands is nil (matches the chart's [FLUSHALL, FLUSHDB]).
var defaultDisabledCommands = []string{"FLUSHALL", "FLUSHDB"}

// Platform is the OpenShift-vs-vanilla discriminator supplied by the caller. It
// is a leaf-safe local type (a plain string) so that pkg/apis does NOT import
// pkg/platform (which pulls in client-go) and the pkg/apis leaf rule (doc 02 §3)
// holds. The reconciler (M3) converts platform.Detect()'s result into this value
// at the call site. See OQ-1.3 / OQ-1.4.
type Platform string

const (
	// PlatformVanilla is upstream Kubernetes.
	PlatformVanilla Platform = "vanilla"
	// PlatformOpenShift is Red Hat OpenShift.
	PlatformOpenShift Platform = "openshift"
)

// CheckNSetDefaults applies the cross-field / derived defaults that CEL and
// kubebuilder:default markers cannot express, plus the fail-closed runtime
// validations (03 §5, 04 §0). It is invoked every reconcile, in-memory (the CR
// is not persisted just for defaulting), and is idempotent: a second call on an
// already-defaulted CR is a no-op.
//
// Leaf-rule constraint (doc 02 §3 / OQ-1.3): pkg/apis must NOT import pkg/naming
// or pkg/platform. The <cluster>-users secret name is therefore built inline
// here (the single name builder the API layer needs); the Platform argument is
// the leaf-safe local type above; version helpers come from pkg/version (a
// near-leaf the rule permits).
func (cr *PerconaValkeyCluster) CheckNSetDefaults(ctx context.Context, platform Platform) error {
	_ = ctx
	_ = platform // reserved for platform-conditional defaults (M3+).

	cr.stampCrVersion()
	cr.setTopologyDefaults()
	cr.setImageDefaults()
	cr.setUpgradeOptionsDefaults()
	cr.setBackupDefaults()
	cr.deriveUserSecretNames()
	cr.setAuthDefaults()
	cr.setDisableCommandsDefaults()
	cr.setProbeDefaults()

	return cr.validateBackupStorages()
}

// stampCrVersion auto-stamps crVersion to the operator major.minor when empty,
// avoiding the PSMDB crVersion=="" pitfall (03 §5, ADR-005). Sourced solely from
// pkg/version (single source of truth) so it stays in lockstep with version.txt.
func (cr *PerconaValkeyCluster) stampCrVersion() {
	if cr.Spec.CrVersion == "" {
		cr.Spec.CrVersion = version.MajorMinor()
	}
}

// setTopologyDefaults applies the mode-dependent shards default (03 §2.3, §5):
// 3 for cluster mode, 1 otherwise. A non-zero shards (set by the user or the
// kubebuilder Minimum=1 floor) is left untouched. replicas is defaulted by a
// marker; nothing to do here.
func (cr *PerconaValkeyCluster) setTopologyDefaults() {
	if cr.Spec.Shards == 0 {
		if cr.Spec.Mode == ModeCluster {
			cr.Spec.Shards = 3
		} else {
			cr.Spec.Shards = 1
		}
	}
}

// setImageDefaults resolves the server and backup-tool images (03 §5).
func (cr *PerconaValkeyCluster) setImageDefaults() {
	if cr.Spec.Image == "" {
		cr.Spec.Image = version.DefaultServerImage()
	}
	if cr.Spec.Backup.Image == "" {
		cr.Spec.Backup.Image = version.DefaultBackupImage()
	}
}

// setUpgradeOptionsDefaults defaults upgradeOptions.apply to Disabled and, when
// smart updates are enabled, fills the schedule and version-service endpoint
// (03 §2.12, §5).
func (cr *PerconaValkeyCluster) setUpgradeOptionsDefaults() {
	if cr.Spec.UpgradeOptions.Apply == "" {
		cr.Spec.UpgradeOptions.Apply = UpgradeApplyDisabled
	}
	if cr.Spec.UpgradeOptions.Apply != UpgradeApplyDisabled {
		if cr.Spec.UpgradeOptions.Schedule == "" {
			cr.Spec.UpgradeOptions.Schedule = defaultUpgradeSchedule
		}
		if cr.Spec.UpgradeOptions.VersionServiceEndpoint == "" {
			cr.Spec.UpgradeOptions.VersionServiceEndpoint = defaultVersionServiceEndpoint
		}
	}
}

// setBackupDefaults fills backup-CR-independent defaults that the markers cannot
// (the activeDeadlineSeconds default for inline schedules has no CR to attach a
// marker to; the per-backup default lives on PerconaValkeyBackupSpec). Currently
// a no-op placeholder kept for cohesion / future schedule-level defaulting.
func (cr *PerconaValkeyCluster) setBackupDefaults() {
	_ = defaultActiveDeadlineSeconds
}

// deriveUserSecretNames derives users[].passwordSecret.name = <cluster>-users
// when empty (03 §5). Built inline (NOT naming.UsersSecretName) to honour the
// pkg/apis leaf rule (OQ-1.3).
func (cr *PerconaValkeyCluster) deriveUserSecretNames() {
	for i := range cr.Spec.Users {
		if cr.Spec.Users[i].PasswordSecret.Name == "" {
			cr.Spec.Users[i].PasswordSecret.Name = cr.Name + usersSecretSuffix
		}
	}
}

// setAuthDefaults materializes the default-user auth block (07 §3 / gap §2.3).
// When the block is absent it is created enabled (the marker default the API
// server would apply, mirrored here for the in-memory reconciler path). When
// enabled and no passwordSecret name is given, the name is derived to
// <cluster>-users (inline, leaf-rule). When auth is explicitly disabled the
// passwordSecret is left untouched (the default user becomes nopass). Idempotent.
func (cr *PerconaValkeyCluster) setAuthDefaults() {
	if cr.Spec.Auth == nil {
		cr.Spec.Auth = &AuthSpec{Enabled: boolPtr(true)}
	}
	if cr.Spec.Auth.Enabled == nil {
		cr.Spec.Auth.Enabled = boolPtr(true)
	}
	if *cr.Spec.Auth.Enabled && cr.Spec.Auth.PasswordSecret.Name == "" {
		cr.Spec.Auth.PasswordSecret.Name = cr.Name + usersSecretSuffix
	}
}

// setDisableCommandsDefaults applies the chart's safe default
// rename-command-disable set ([FLUSHALL, FLUSHDB]) when the user has not set the
// field. A nil slice means "use the default"; an explicit empty slice
// ([]string{}) means "disable nothing" and is preserved. Idempotent.
func (cr *PerconaValkeyCluster) setDisableCommandsDefaults() {
	if cr.Spec.DisableCommands == nil {
		cr.Spec.DisableCommands = append([]string(nil), defaultDisabledCommands...)
	}
}

// boolPtr returns a pointer to b (local helper; pkg/apis is a leaf so it cannot
// import a shared ptr helper from pkg/k8s).
func boolPtr(b bool) *bool { return &b }

// setProbeDefaults sets probe-timeout defaults that the markers cannot express.
// Probe fields are not modelled in v1alpha1 spec (probes are operator-managed),
// so this is a documented no-op placeholder kept for the 03 §5 contract; the
// concrete probe wiring lands with the node controller (M2).
func (cr *PerconaValkeyCluster) setProbeDefaults() {}

// validateBackupStorages fails closed when any schedule references a storageName
// not present in spec.backup.storages (03 §4.3, §2.11). This is a runtime check,
// deliberately NOT CEL (the cross-map reference is awkward in CEL and the
// architecture assigns it to CheckNSetDefaults).
func (cr *PerconaValkeyCluster) validateBackupStorages() error {
	for _, s := range cr.Spec.Backup.Schedule {
		if _, ok := cr.Spec.Backup.Storages[s.StorageName]; !ok {
			return fmt.Errorf("backup.schedule[%q]: unknown storageName %q (not in spec.backup.storages)", s.Name, s.StorageName)
		}
	}
	return nil
}
