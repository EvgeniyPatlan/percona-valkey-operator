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

package perconavalkeycluster

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// seamCluster builds a minimal cluster-mode cluster for seam unit tests.
func seamCluster() *valkeyv1alpha1.PerconaValkeyCluster {
	return &valkeyv1alpha1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "seamc", Namespace: "ns"},
		Spec: valkeyv1alpha1.PerconaValkeyClusterSpec{
			Mode:     valkeyv1alpha1.ModeCluster,
			Shards:   1,
			Replicas: 1,
		},
	}
}

// TestAnnounceWanted locks the predicate: only an external (NodePort/LoadBalancer)
// perPod expose in cluster mode wants the per-pod announce wiring.
func TestAnnounceWanted(t *testing.T) {
	cases := []struct {
		name   string
		expose *valkeyv1alpha1.ExposeSpec
		mode   valkeyv1alpha1.ClusterMode
		want   bool
	}{
		{"nil expose", nil, valkeyv1alpha1.ModeCluster, false},
		{"clusterIP perPod", &valkeyv1alpha1.ExposeSpec{Type: "ClusterIP", PerPod: true}, valkeyv1alpha1.ModeCluster, false},
		{"nodePort no perPod", &valkeyv1alpha1.ExposeSpec{Type: "NodePort"}, valkeyv1alpha1.ModeCluster, false},
		{"nodePort perPod cluster", &valkeyv1alpha1.ExposeSpec{Type: "NodePort", PerPod: true}, valkeyv1alpha1.ModeCluster, true},
		{"lb perPod cluster", &valkeyv1alpha1.ExposeSpec{Type: "LoadBalancer", PerPod: true}, valkeyv1alpha1.ModeCluster, true},
		{"nodePort perPod replication", &valkeyv1alpha1.ExposeSpec{Type: "NodePort", PerPod: true}, valkeyv1alpha1.ModeReplication, false},
	}
	for _, c := range cases {
		cl := seamCluster()
		cl.Spec.Mode = c.mode
		cl.Spec.Expose = c.expose
		if got := announceWanted(cl); got != c.want {
			t.Errorf("%s: announceWanted = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestIsRestoreTarget locks the restored-from marker predicate.
func TestIsRestoreTarget(t *testing.T) {
	cl := seamCluster()
	if isRestoreTarget(cl) {
		t.Error("fresh cluster must not be a restore target")
	}
	cl.Annotations = map[string]string{annRestoreMarker: "myrestore/nightly-full"}
	if !isRestoreTarget(cl) {
		t.Error("cluster with restored-from marker must be a restore target")
	}
}

// TestRestoreSourceForNodeOnlyPrimary verifies the restore-target seam only seeds a
// primary (node index 0): a replica re-syncs from the seeded primary, so it never
// carries a restoreFrom. A restore-target primary carries the backup name parsed
// from the marker, its shard index, and the named storage backend bridged from the
// restore controller's restore-storage marker; a non-restore cluster carries none.
func TestRestoreSourceForNodeOnlyPrimary(t *testing.T) {
	cl := seamCluster()
	cl.Annotations = map[string]string{
		annRestoreMarker:  "myrestore/nightly-full",
		annRestoreStorage: "s3-primary",
	}

	if got := restoreSourceForNode(cl, nodeKey{shard: 0, node: 1}); got != nil {
		t.Errorf("replica must never carry restoreFrom, got %+v", got)
	}
	prim := restoreSourceForNode(cl, nodeKey{shard: 2, node: 0})
	if prim == nil {
		t.Fatal("restore-target primary must carry a restoreFrom")
	}
	if prim.BackupName != "nightly-full" {
		t.Errorf("backupName = %q, want nightly-full (parsed from marker)", prim.BackupName)
	}
	if prim.ShardIndex != 2 {
		t.Errorf("shardIndex = %d, want 2 (the node's shard)", prim.ShardIndex)
	}
	// The required RestoreFrom.Storage is resolved from the restore-storage marker.
	if prim.Storage != "s3-primary" {
		t.Errorf("storage = %q, want s3-primary (from restore-storage marker)", prim.Storage)
	}
	// Non-restore cluster: nil regardless of node index.
	fresh := seamCluster()
	if got := restoreSourceForNode(fresh, nodeKey{shard: 0, node: 0}); got != nil {
		t.Errorf("non-restore cluster must not seed, got %+v", got)
	}
}

// TestStorageForRestore locks the storage-resolution precedence: the restore-storage
// marker wins; absent it, a single configured backup storage is the unambiguous
// fallback; with zero or multiple storages and no marker it resolves empty (the seed
// env builder must resolve it from the cluster's backup block at pod-build time).
func TestStorageForRestore(t *testing.T) {
	// 1) Marker present -> marker wins even when storages also exist.
	withMarker := seamCluster()
	withMarker.Annotations = map[string]string{annRestoreStorage: "marker-store"}
	withMarker.Spec.Backup.Storages = map[string]valkeyv1alpha1.BackupStorageSpec{
		"other": {Type: valkeyv1alpha1.BackupStorageS3},
	}
	if got := storageForRestore(withMarker); got != "marker-store" {
		t.Errorf("marker must win, got %q", got)
	}

	// 2) No marker, exactly one configured storage -> that storage (unambiguous).
	single := seamCluster()
	single.Spec.Backup.Storages = map[string]valkeyv1alpha1.BackupStorageSpec{
		"only-one": {Type: valkeyv1alpha1.BackupStorageS3},
	}
	if got := storageForRestore(single); got != "only-one" {
		t.Errorf("single storage fallback = %q, want only-one", got)
	}

	// 3) No marker, multiple storages -> ambiguous, empty (deferred to env builder).
	multi := seamCluster()
	multi.Spec.Backup.Storages = map[string]valkeyv1alpha1.BackupStorageSpec{
		"a": {Type: valkeyv1alpha1.BackupStorageS3},
		"b": {Type: valkeyv1alpha1.BackupStorageGCS},
	}
	if got := storageForRestore(multi); got != "" {
		t.Errorf("ambiguous multi-storage must resolve empty, got %q", got)
	}

	// 4) No marker, no storages -> empty.
	none := seamCluster()
	if got := storageForRestore(none); got != "" {
		t.Errorf("no storage must resolve empty, got %q", got)
	}
}

// TestBackupNameFromMarker locks the marker-parse: the trailing "<backupName>"
// component, "" for the inline-source sentinel.
func TestBackupNameFromMarker(t *testing.T) {
	cases := map[string]string{
		"myrestore/nightly-full": "nightly-full",
		"r/backupSource":         "",
		"noslash":                "",
		"trailing/":              "",
	}
	for marker, want := range cases {
		if got := backupNameFromMarker(marker); got != want {
			t.Errorf("backupNameFromMarker(%q) = %q, want %q", marker, got, want)
		}
	}
}

// TestBuildValkeyNodeSpecDefaultsNoSeams verifies buildValkeyNodeSpec leaves the
// announce + restore fields empty for a plain (no expose, no restore) cluster — the
// safe default that keeps the in-cluster POD_IP announce and no seed container.
func TestBuildValkeyNodeSpecDefaultsNoSeams(t *testing.T) {
	cl := seamCluster()
	node := buildValkeyNodeSpec(cl, nodeKey{shard: 0, node: 0}, "hash")
	if node.Spec.AnnounceHost != "" || node.Spec.AnnouncePort != nil {
		t.Errorf("default announce must be empty, got host=%q port=%v", node.Spec.AnnounceHost, node.Spec.AnnouncePort)
	}
	if node.Spec.RestoreFrom != nil {
		t.Errorf("default restoreFrom must be nil, got %+v", node.Spec.RestoreFrom)
	}
}

// TestBuildValkeyNodeSpecPropagatesSeams verifies buildValkeyNodeSpec wires the
// announce seam (per-pod external expose) and the restore seam (restored-from marker)
// onto the node spec so the node controller renders the external announce + the seed
// init container.
func TestBuildValkeyNodeSpecPropagatesSeams(t *testing.T) {
	cl := seamCluster()
	cl.Spec.Expose = &valkeyv1alpha1.ExposeSpec{Type: "LoadBalancer", PerPod: true}
	cl.Annotations = map[string]string{annRestoreMarker: "myrestore/nightly-full"}

	node := buildValkeyNodeSpec(cl, nodeKey{shard: 0, node: 0}, "hash")
	if node.Spec.AnnounceHost == "" || node.Spec.AnnouncePort == nil {
		t.Errorf("per-pod expose must propagate an announce host+port, got host=%q port=%v",
			node.Spec.AnnounceHost, node.Spec.AnnouncePort)
	}
	if node.Spec.RestoreFrom == nil || node.Spec.RestoreFrom.ShardIndex != 0 {
		t.Errorf("restore marker must propagate restoreFrom for the primary, got %+v", node.Spec.RestoreFrom)
	}
}
