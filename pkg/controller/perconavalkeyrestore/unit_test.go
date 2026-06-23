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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// testCtxBG returns a background context for unit tests that do not touch the
// envtest apiserver.
func testCtxBG() context.Context { return context.Background() }

func TestPhaseToState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		phase restorePhase
		want  valkeyv1alpha1.RestoreState
	}{
		{phasePending, valkeyv1alpha1.RestoreStateStarting},
		{phaseProvisioning, valkeyv1alpha1.RestoreStateStarting},
		{phaseSeeding, valkeyv1alpha1.RestoreStateStarting},
		{phaseForming, valkeyv1alpha1.RestoreStateStarting},
		{phaseValidating, valkeyv1alpha1.RestoreStateRunning},
		{phaseSucceeded, valkeyv1alpha1.RestoreStateSucceeded},
		{phaseFailed, valkeyv1alpha1.RestoreStateFailed},
		{restorePhase("bogus"), valkeyv1alpha1.RestoreStateError},
	}
	for _, tc := range cases {
		if got := phaseToState(tc.phase); got != tc.want {
			t.Errorf("phaseToState(%q) = %q, want %q", tc.phase, got, tc.want)
		}
	}
}

func TestCurrentPhaseDefaultsPending(t *testing.T) {
	t.Parallel()
	rst := &valkeyv1alpha1.PerconaValkeyRestore{}
	if got := currentPhase(rst); got != phasePending {
		t.Fatalf("currentPhase(empty) = %q, want Pending", got)
	}
	rst.Annotations = map[string]string{annPhase: string(phaseForming)}
	if got := currentPhase(rst); got != phaseForming {
		t.Fatalf("currentPhase = %q, want Forming", got)
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	for _, p := range []restorePhase{phaseSucceeded, phaseFailed} {
		rst := &valkeyv1alpha1.PerconaValkeyRestore{}
		rst.Annotations = map[string]string{annPhase: string(p)}
		if !isTerminal(rst) {
			t.Errorf("isTerminal(%q) = false, want true", p)
		}
	}
	for _, p := range []restorePhase{phasePending, phaseProvisioning, phaseSeeding, phaseForming, phaseValidating} {
		rst := &valkeyv1alpha1.PerconaValkeyRestore{}
		rst.Annotations = map[string]string{annPhase: string(p)}
		if isTerminal(rst) {
			t.Errorf("isTerminal(%q) = true, want false", p)
		}
	}
}

func TestParseManifestSlotRanges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    []valkey.SlotRange
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"single", "5", []valkey.SlotRange{{Start: 5, End: 5}}, false},
		{"range", "0-5460", []valkey.SlotRange{{Start: 0, End: 5460}}, false},
		{"multi", "0-100,500-600", []valkey.SlotRange{{Start: 0, End: 100}, {Start: 500, End: 600}}, false},
		{"spaces", " 0-100 , 500-600 ", []valkey.SlotRange{{Start: 0, End: 100}, {Start: 500, End: 600}}, false},
		{"bad-token", "abc", nil, true},
		{"reversed", "100-50", nil, true},
		{"out-of-bounds", "0-99999", nil, true},
		{"negative", "-5", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseManifestSlotRanges(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseManifestSlotRanges(%q) = nil err, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseManifestSlotRanges(%q) error: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("range %d: got %v want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestValidateManifestCoverage(t *testing.T) {
	t.Parallel()
	full := [][]valkey.SlotRange{
		{{Start: 0, End: 5460}},
		{{Start: 5461, End: 10922}},
		{{Start: 10923, End: 16383}},
	}
	v, err := validateManifestCoverage(full)
	if err != nil {
		t.Fatalf("complete coverage error: %v", err)
	}
	if !v.Complete || v.Overlap || v.Covered != valkey.TotalSlots {
		t.Fatalf("complete: got %+v", v)
	}

	gap := [][]valkey.SlotRange{
		{{Start: 0, End: 5460}},
		{{Start: 5461, End: 10922}},
		// missing 10923-16383
	}
	v, err = validateManifestCoverage(gap)
	if err != nil {
		t.Fatalf("gap coverage error: %v", err)
	}
	if v.Complete || v.Overlap {
		t.Fatalf("gap: expected incomplete non-overlap, got %+v", v)
	}
	if v.Covered != 10923 {
		t.Fatalf("gap covered = %d, want 10923", v.Covered)
	}

	overlap := [][]valkey.SlotRange{
		{{Start: 0, End: 8000}},
		{{Start: 5000, End: 16383}}, // 5000-8000 overlaps shard 0
	}
	v, err = validateManifestCoverage(overlap)
	if err != nil {
		t.Fatalf("overlap coverage error: %v", err)
	}
	if !v.Overlap || v.Complete {
		t.Fatalf("overlap: expected overlap & not complete, got %+v", v)
	}
}

func TestValidateCompat(t *testing.T) {
	t.Parallel()
	completeManifest := func() backup.Manifest {
		return backup.Manifest{
			SlotCoverage: coverageComplete,
			Shards: []backup.ShardManifest{
				{Index: 0, SlotRanges: "0-5460"},
				{Index: 1, SlotRanges: "5461-10922"},
				{Index: 2, SlotRanges: "10923-16383"},
			},
		}
	}

	t.Run("complete-ok", func(t *testing.T) {
		t.Parallel()
		rst := &valkeyv1alpha1.PerconaValkeyRestore{}
		v, err := validateCompat(rst, completeManifest())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.Complete {
			t.Fatalf("expected complete coverage")
		}
	})

	t.Run("no-shards", func(t *testing.T) {
		t.Parallel()
		rst := &valkeyv1alpha1.PerconaValkeyRestore{}
		if _, err := validateCompat(rst, backup.Manifest{}); err == nil {
			t.Fatalf("expected error for empty manifest")
		}
	})

	t.Run("partial-rejected", func(t *testing.T) {
		t.Parallel()
		man := completeManifest()
		man.Shards = man.Shards[:2] // drop the last shard -> gap
		man.SlotCoverage = coveragePartial
		rst := &valkeyv1alpha1.PerconaValkeyRestore{}
		if _, err := validateCompat(rst, man); err == nil {
			t.Fatalf("expected partial-coverage rejection")
		}
	})

	t.Run("partial-allowed-by-annotation", func(t *testing.T) {
		t.Parallel()
		man := completeManifest()
		man.Shards = man.Shards[:2]
		man.SlotCoverage = coveragePartial
		rst := &valkeyv1alpha1.PerconaValkeyRestore{}
		rst.Annotations = map[string]string{annAllowPartial: "true"}
		if _, err := validateCompat(rst, man); err != nil {
			t.Fatalf("partial restore should be allowed with override: %v", err)
		}
	})

	t.Run("shard-count-mismatch", func(t *testing.T) {
		t.Parallel()
		rst := &valkeyv1alpha1.PerconaValkeyRestore{}
		rst.Annotations = map[string]string{annClusterTmpl: clusterTemplateAnnotation(5)}
		if _, err := validateCompat(rst, completeManifest()); err == nil {
			t.Fatalf("expected shard-count mismatch error (template 5 vs manifest 3)")
		}
	})

	t.Run("shard-count-match", func(t *testing.T) {
		t.Parallel()
		rst := &valkeyv1alpha1.PerconaValkeyRestore{}
		rst.Annotations = map[string]string{annClusterTmpl: clusterTemplateAnnotation(3)}
		if _, err := validateCompat(rst, completeManifest()); err != nil {
			t.Fatalf("matching shard count should pass: %v", err)
		}
	})
}

func TestRestoreInitContainerAppendonlyNoSeed(t *testing.T) {
	t.Parallel()
	op := backup.JobEnvParams{
		Cluster: "target",
		Backup:  "bk",
		Spec: valkeyv1alpha1.BackupStorageSpec{
			Type: valkeyv1alpha1.BackupStorageS3,
			S3:   &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "bkt", Prefix: "p/", Region: "eu-central-1"},
		},
	}
	c := restoreInitContainer(2, "percona/valkey-backup:9.0.0", "prod-s3-creds", op)
	if c.Name != seedContainerName {
		t.Fatalf("init container name = %q, want %q", c.Name, seedContainerName)
	}
	// The seed runs cmd/valkey-backup --download --shard=2 into /data BEFORE boot.
	joined := c.Command
	if len(joined) != 3 || joined[1] != "--download" || joined[2] != "--shard=2" {
		t.Fatalf("init container command = %v, want --download --shard=2", joined)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != seedDataDir {
		t.Fatalf("init container must mount the data dir at %q, got %+v", seedDataDir, c.VolumeMounts)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "prod-s3-creds" {
		t.Fatalf("creds must be mounted as env from the secret (06 §8.2), got %+v", c.EnvFrom)
	}
	// The sidecar reads storage coords + cluster/backup names from VALKEY_BACKUP_*
	// env (the seam the controller Job builders share with cmd/valkey-backup). The
	// download seed must carry them so the backend + object keys resolve.
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env[backup.EnvCluster] != "target" || env[backup.EnvBackupName] != "bk" {
		t.Fatalf("seed env missing cluster/backup names: %v", env)
	}
	if env[backup.EnvStorageType] != string(valkeyv1alpha1.BackupStorageS3) || env[backup.EnvS3Bucket] != "bkt" {
		t.Fatalf("seed env missing S3 coordinates: %v", env)
	}
}

func TestSeedOverrideMarkers(t *testing.T) {
	t.Parallel()
	// A cluster with the appendonly-no override marker reads as seed-override-applied;
	// this is the CR-8 invariant that the engine loads dump.rdb, not an empty AOF.
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	if seedOverrideApplied(cluster) || restoreMarkerApplied(cluster) {
		t.Fatalf("bare cluster must not report seed markers applied")
	}
	cluster.Annotations = map[string]string{
		annSeedAppendonly: seedAppendonlyNo,
		annRestoreMarker:  "rst/bk",
	}
	if !seedOverrideApplied(cluster) {
		t.Fatalf("expected appendonly-no seed override to be detected")
	}
	if !restoreMarkerApplied(cluster) {
		t.Fatalf("expected restored-from marker to be detected")
	}
	// A cluster that booted with appendonly yes (no override) must NOT pass — that
	// is exactly the silent-zero-key-restore trap (06 §7.4, R3).
	cluster.Annotations[annSeedAppendonly] = "yes"
	if seedOverrideApplied(cluster) {
		t.Fatalf("appendonly=yes must not be treated as the seed override")
	}
}

func TestDestinationIdentity(t *testing.T) {
	t.Parallel()
	cluster, bk, err := destinationIdentity("s3://my-bucket/prefix/prod/prod-20260622-020000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cluster != "prod" || bk != "prod-20260622-020000" {
		t.Fatalf("identity = (%q,%q), want (prod, prod-20260622-020000)", cluster, bk)
	}
	if _, _, err := destinationIdentity("s3://only-bucket"); err == nil {
		t.Fatalf("expected error for destination without <cluster>/<backup> suffix")
	}
	if _, _, err := destinationIdentity("not-a-scheme/x"); err == nil {
		t.Fatalf("expected parse error for unknown scheme")
	}
}

func TestReformClusterGates(t *testing.T) {
	t.Parallel()
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	if clusterFormed(cluster) || clusterSlotsComplete(cluster) || clusterReady(cluster) {
		t.Fatalf("bare cluster must not satisfy any re-form gate")
	}
	setCond(cluster, condClusterFormed)
	if !clusterFormed(cluster) {
		t.Fatalf("ClusterFormed=True should satisfy clusterFormed")
	}
	setCond(cluster, condSlotsAssigned)
	if !clusterSlotsComplete(cluster) {
		t.Fatalf("SlotsAssigned=True should satisfy clusterSlotsComplete")
	}
	cluster.Status.State = valkeyv1alpha1.StateReady
	if !clusterReady(cluster) {
		t.Fatalf("state Ready should satisfy clusterReady")
	}
}

// setCond is a tiny helper that sets a condition True on a cluster for gate tests.
func setCond(cluster *valkeyv1alpha1.PerconaValkeyCluster, condType string) {
	cluster.Status.Conditions = append(cluster.Status.Conditions, metav1.Condition{
		Type:   condType,
		Status: metav1.ConditionTrue,
		Reason: "Test",
	})
}

func TestStorageConfigFromSource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		dest   backup.Destination
		src    *valkeyv1alpha1.BackupSource
		want   valkeyv1alpha1.BackupStorageType
		errFor bool
	}{
		{"s3", backup.Destination{Scheme: backup.SchemeS3}, &valkeyv1alpha1.BackupSource{S3: &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "b"}}, valkeyv1alpha1.BackupStorageS3, false},
		{"gcs", backup.Destination{Scheme: backup.SchemeGCS}, &valkeyv1alpha1.BackupSource{GCS: &valkeyv1alpha1.BackupStorageGCSSpec{Bucket: "b"}}, valkeyv1alpha1.BackupStorageGCS, false},
		{"azure", backup.Destination{Scheme: backup.SchemeAzure}, &valkeyv1alpha1.BackupSource{Azure: &valkeyv1alpha1.BackupStorageAzureSpec{Container: "c"}}, valkeyv1alpha1.BackupStorageAzure, false},
		{"pvc", backup.Destination{Scheme: backup.SchemePVC, Path: "root/x"}, &valkeyv1alpha1.BackupSource{}, valkeyv1alpha1.BackupStorageFilesystem, false},
		{"unknown", backup.Destination{Scheme: "ftp://"}, &valkeyv1alpha1.BackupSource{}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := storageConfigFromSource(tc.src, tc.dest)
			if tc.errFor {
				if err == nil {
					t.Fatalf("expected error for scheme %q", tc.dest.Scheme)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Type != tc.want {
				t.Fatalf("type = %q, want %q", cfg.Type, tc.want)
			}
			if tc.want == valkeyv1alpha1.BackupStorageFilesystem && cfg.FilesystemRoot != tc.dest.Path {
				t.Fatalf("pvc FilesystemRoot = %q, want %q", cfg.FilesystemRoot, tc.dest.Path)
			}
		})
	}
}

func TestClusterTemplateAndShards(t *testing.T) {
	t.Parallel()
	// Absent annotation -> inherit (ok=false).
	rst := &valkeyv1alpha1.PerconaValkeyRestore{}
	if _, ok := templateShards(rst); ok {
		t.Fatalf("no template should mean inherit (ok=false)")
	}
	// Valid template -> shards parsed.
	rst.Annotations = map[string]string{annClusterTmpl: clusterTemplateAnnotation(4)}
	n, ok := templateShards(rst)
	if !ok || n != 4 {
		t.Fatalf("templateShards = (%d,%v), want (4,true)", n, ok)
	}
	// Unparseable template -> treated as absent (ok=false), inherit manifest.
	rst.Annotations[annClusterTmpl] = "{not json"
	if _, ok := clusterTemplate(rst); ok {
		t.Fatalf("unparseable template must be treated as absent")
	}
	// shards=0 template -> inherit.
	rst.Annotations[annClusterTmpl] = clusterTemplateAnnotation(0)
	if _, ok := templateShards(rst); ok {
		t.Fatalf("shards=0 template should inherit (ok=false)")
	}
}

func TestAnnotationsContainAndMarkers(t *testing.T) {
	t.Parallel()
	rst := &valkeyv1alpha1.PerconaValkeyRestore{}
	rst.Name = "myrestore"
	rst.Spec.BackupName = "mybackup"
	src := resolvedSource{StorageName: "s3-primary"}
	markers := restoreMarkerAnnotations(rst, src)
	if markers[annSeedAppendonly] != seedAppendonlyNo {
		t.Fatalf("markers must carry appendonly-no override")
	}
	if markers[annRestoreMarker] != "myrestore/mybackup" {
		t.Fatalf("restore marker value = %q", markers[annRestoreMarker])
	}
	// The resolved named storage is bridged onto the cluster so the cluster
	// controller's restore-target seam can populate RestoreSource.Storage.
	if markers[annRestoreStorage] != "s3-primary" {
		t.Fatalf("storage marker = %q, want s3-primary", markers[annRestoreStorage])
	}
	if annotationsContain(nil, markers) {
		t.Fatalf("nil annotations cannot contain the markers")
	}
	if !annotationsContain(markers, markers) {
		t.Fatalf("a set contains itself")
	}
	// An inline source with no named storage omits the storage marker so the seam
	// falls back to the cluster's own resolved backup storage.
	if got := restoreMarkerAnnotations(rst, resolvedSource{}); got[annRestoreStorage] != "" {
		t.Fatalf("no named storage must omit the storage marker, got %q", got[annRestoreStorage])
	}
	// backupSource variant of the marker value.
	rst.Spec.BackupName = ""
	rst.Spec.BackupSource = &valkeyv1alpha1.BackupSource{}
	if got := restoreMarkerValue(rst); got != "myrestore/backupSource" {
		t.Fatalf("backupSource marker value = %q", got)
	}
}

func TestLastTwoPathComponents(t *testing.T) {
	t.Parallel()
	cluster, bk, ok := lastTwoPathComponents("prefix/src/bk")
	if !ok || cluster != "src" || bk != "bk" {
		t.Fatalf("lastTwo = (%q,%q,%v)", cluster, bk, ok)
	}
	if _, _, ok := lastTwoPathComponents("only"); ok {
		t.Fatalf("single component must be ok=false")
	}
	if _, _, ok := lastTwoPathComponents("//a//b//"); !ok {
		t.Fatalf("slashes should be trimmed, leaving a/b")
	}
}

func TestResolveSourceOneOf(t *testing.T) {
	t.Parallel()
	r := &Reconciler{}
	// Neither set -> error (CEL also enforces this, but the controller guards too).
	rst := &valkeyv1alpha1.PerconaValkeyRestore{}
	if _, err := r.resolveSource(testCtxBG(), rst); err == nil {
		t.Fatalf("expected one-of error when neither source set")
	}
	// Both set -> error.
	rst.Spec.BackupName = "bk"
	rst.Spec.BackupSource = &valkeyv1alpha1.BackupSource{Destination: "s3://b/src/bk"}
	if _, err := r.resolveSource(testCtxBG(), rst); err == nil {
		t.Fatalf("expected one-of error when both sources set")
	}
}
