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

package v1alpha1_test

import (
	"context"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/version"
)

func clusterWith(mutate func(*v1.PerconaValkeyCluster)) *v1.PerconaValkeyCluster {
	cr := &v1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster", Namespace: "default"},
		Spec: v1.PerconaValkeyClusterSpec{
			Mode:         v1.ModeCluster,
			WorkloadType: v1.WorkloadStatefulSet,
		},
	}
	if mutate != nil {
		mutate(cr)
	}
	return cr
}

// defaultsAssert is the per-case verification callback for the
// CheckNSetDefaults table. Each concrete assertion lives in its own helper so
// the table itself stays low-complexity (the branching is distributed across
// the helpers rather than concentrated in TestCheckNSetDefaults).
type defaultsAssert func(t *testing.T, cr *v1.PerconaValkeyCluster, err error)

func assertCrVersionStamped(t *testing.T, cr *v1.PerconaValkeyCluster, err error) {
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Spec.CrVersion != version.MajorMinor() {
		t.Errorf("crVersion = %q, want %q", cr.Spec.CrVersion, version.MajorMinor())
	}
}

func assertCrVersionPreserved(t *testing.T, cr *v1.PerconaValkeyCluster, _ error) {
	if cr.Spec.CrVersion != "9.9" {
		t.Errorf("crVersion = %q, want 9.9", cr.Spec.CrVersion)
	}
}

func assertShards(want int32) defaultsAssert {
	return func(t *testing.T, cr *v1.PerconaValkeyCluster, _ error) {
		if cr.Spec.Shards != want {
			t.Errorf("shards = %d, want %d", cr.Spec.Shards, want)
		}
	}
}

func assertImagesDefaulted(t *testing.T, cr *v1.PerconaValkeyCluster, _ error) {
	if cr.Spec.Image != version.DefaultServerImage() {
		t.Errorf("image = %q, want %q", cr.Spec.Image, version.DefaultServerImage())
	}
	if cr.Spec.Backup.Image != version.DefaultBackupImage() {
		t.Errorf("backup.image = %q, want %q", cr.Spec.Backup.Image, version.DefaultBackupImage())
	}
}

func assertImagesPreserved(t *testing.T, cr *v1.PerconaValkeyCluster, _ error) {
	if cr.Spec.Image != "myrepo/valkey:1" || cr.Spec.Backup.Image != "myrepo/backup:1" {
		t.Errorf("images overwritten: %q %q", cr.Spec.Image, cr.Spec.Backup.Image)
	}
}

func assertUpgradeApplyDisabled(t *testing.T, cr *v1.PerconaValkeyCluster, _ error) {
	if cr.Spec.UpgradeOptions.Apply != v1.UpgradeApplyDisabled {
		t.Errorf("apply = %q, want Disabled", cr.Spec.UpgradeOptions.Apply)
	}
}

func assertUpgradeFilledWhenEnabled(t *testing.T, cr *v1.PerconaValkeyCluster, _ error) {
	if cr.Spec.UpgradeOptions.Schedule == "" {
		t.Error("expected upgradeOptions.schedule to be filled")
	}
	if cr.Spec.UpgradeOptions.VersionServiceEndpoint == "" {
		t.Error("expected upgradeOptions.versionServiceEndpoint to be filled")
	}
}

func assertUserSecretNamesDerived(t *testing.T, cr *v1.PerconaValkeyCluster, _ error) {
	if cr.Spec.Users[0].PasswordSecret.Name != "mycluster-users" {
		t.Errorf("users[0] secret = %q, want mycluster-users", cr.Spec.Users[0].PasswordSecret.Name)
	}
	if cr.Spec.Users[1].PasswordSecret.Name != "custom" {
		t.Errorf("users[1] secret = %q, want custom (preserved)", cr.Spec.Users[1].PasswordSecret.Name)
	}
}

func assertNoError(t *testing.T, _ *v1.PerconaValkeyCluster, err error) {
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertUnknownStorageNameRejected(t *testing.T, _ *v1.PerconaValkeyCluster, err error) {
	if err == nil {
		t.Fatal("expected error for unknown storageName")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error %q should mention the bad storageName", err.Error())
	}
}

func TestCheckNSetDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cr     *v1.PerconaValkeyCluster
		assert defaultsAssert
	}{
		{
			name:   "empty crVersion is stamped to operator major.minor",
			cr:     clusterWith(nil),
			assert: assertCrVersionStamped,
		},
		{
			name:   "explicit crVersion is preserved",
			cr:     clusterWith(func(c *v1.PerconaValkeyCluster) { c.Spec.CrVersion = "9.9" }),
			assert: assertCrVersionPreserved,
		},
		{
			name:   "cluster mode defaults shards to 3",
			cr:     clusterWith(func(c *v1.PerconaValkeyCluster) { c.Spec.Mode = v1.ModeCluster }),
			assert: assertShards(3),
		},
		{
			name:   "replication mode defaults shards to 1",
			cr:     clusterWith(func(c *v1.PerconaValkeyCluster) { c.Spec.Mode = v1.ModeReplication }),
			assert: assertShards(1),
		},
		{
			name:   "standalone mode defaults shards to 1",
			cr:     clusterWith(func(c *v1.PerconaValkeyCluster) { c.Spec.Mode = v1.ModeStandalone }),
			assert: assertShards(1),
		},
		{
			name:   "explicit shards is preserved",
			cr:     clusterWith(func(c *v1.PerconaValkeyCluster) { c.Spec.Shards = 5 }),
			assert: assertShards(5),
		},
		{
			name:   "image and backup image defaulted when empty",
			cr:     clusterWith(nil),
			assert: assertImagesDefaulted,
		},
		{
			name: "explicit images preserved",
			cr: clusterWith(func(c *v1.PerconaValkeyCluster) {
				c.Spec.Image = "myrepo/valkey:1"
				c.Spec.Backup.Image = "myrepo/backup:1"
			}),
			assert: assertImagesPreserved,
		},
		{
			name:   "upgradeOptions.apply defaults to Disabled",
			cr:     clusterWith(nil),
			assert: assertUpgradeApplyDisabled,
		},
		{
			name:   "upgradeOptions schedule+endpoint filled when enabled",
			cr:     clusterWith(func(c *v1.PerconaValkeyCluster) { c.Spec.UpgradeOptions.Apply = v1.UpgradeApplyRecommended }),
			assert: assertUpgradeFilledWhenEnabled,
		},
		{
			name: "users[].passwordSecret.name derived to <cluster>-users",
			cr: clusterWith(func(c *v1.PerconaValkeyCluster) {
				c.Spec.Users = []v1.UserACLSpec{{Name: "app"}, {Name: "ops", PasswordSecret: v1.UserPasswordSecret{Name: "custom"}}}
			}),
			assert: assertUserSecretNamesDerived,
		},
		{
			name: "good schedule storageName passes",
			cr: clusterWith(func(c *v1.PerconaValkeyCluster) {
				c.Spec.Backup.Storages = map[string]v1.BackupStorageSpec{"s3": {Type: v1.BackupStorageS3}}
				c.Spec.Backup.Schedule = []v1.BackupScheduleSpec{{Name: "n", Schedule: "0 2 * * *", StorageName: "s3"}}
			}),
			assert: assertNoError,
		},
		{
			name: "bad schedule storageName fails closed",
			cr: clusterWith(func(c *v1.PerconaValkeyCluster) {
				c.Spec.Backup.Storages = map[string]v1.BackupStorageSpec{"s3": {Type: v1.BackupStorageS3}}
				c.Spec.Backup.Schedule = []v1.BackupScheduleSpec{{Name: "n", Schedule: "0 2 * * *", StorageName: "typo"}}
			}),
			assert: assertUnknownStorageNameRejected,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cr.CheckNSetDefaults(context.Background(), v1.PlatformVanilla)
			tc.assert(t, tc.cr, err)
		})
	}
}

func TestCheckNSetDefaults_Idempotent(t *testing.T) {
	t.Parallel()
	cr := clusterWith(func(c *v1.PerconaValkeyCluster) {
		c.Spec.Users = []v1.UserACLSpec{{Name: "app"}}
	})
	if err := cr.CheckNSetDefaults(context.Background(), v1.PlatformVanilla); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := cr.DeepCopy()
	if err := cr.CheckNSetDefaults(context.Background(), v1.PlatformVanilla); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if cr.Spec.CrVersion != first.Spec.CrVersion ||
		cr.Spec.Shards != first.Spec.Shards ||
		cr.Spec.Image != first.Spec.Image ||
		cr.Spec.Backup.Image != first.Spec.Backup.Image ||
		cr.Spec.Users[0].PasswordSecret.Name != first.Spec.Users[0].PasswordSecret.Name {
		t.Error("CheckNSetDefaults is not idempotent: second call changed the spec")
	}
}

// TestLeafRule asserts the pkg/apis leaf rule (doc 02 §3 / OQ-1.3): the package
// must NOT import pkg/naming, pkg/platform or pkg/controller. pkg/version is the
// single permitted near-leaf exception.
//
// The non-test .go files in this directory are parsed individually with
// parser.ParseFile (over a filepath.Glob) rather than the deprecated
// parser.ParseDir, which does not consider build tags when grouping files.
func TestLeafRule(t *testing.T) {
	t.Parallel()
	forbidden := []string{
		"valkey.percona.com/percona-valkey-operator/pkg/naming",
		"valkey.percona.com/percona-valkey-operator/pkg/platform",
		"valkey.percona.com/percona-valkey-operator/pkg/controller",
		"valkey.percona.com/percona-valkey-operator/pkg/valkey",
	}

	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	for _, fname := range matches {
		if strings.HasSuffix(fname, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, fname, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", fname, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad {
					t.Errorf("%s imports forbidden package %s (pkg/apis leaf rule)", fname, bad)
				}
			}
		}
	}
}
