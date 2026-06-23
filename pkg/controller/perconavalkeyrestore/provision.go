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
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// seedAppendonlyNo is the value the restore stamps so the seeded primary boots with
// appendonly OFF and the engine loads dump.rdb instead of an (empty) AOF — the one
// place restore overrides persistence (06 §7.4, R3 "silent zero-key restore").
// Re-enabling AOF post-load (CONFIG SET appendonly yes) is the cluster/node
// controller's job once the keyspace is in memory.
const seedAppendonlyNo = "no"

// targetClusterName returns the target PerconaValkeyCluster name for a restore: the
// spec.clusterName (06 §7.1 — for NewCluster this is the cluster the operator
// creates, for InPlace it already exists).
func targetClusterName(rst *valkeyv1alpha1.PerconaValkeyRestore) string {
	return rst.Spec.ClusterName
}

// templateShards returns the desired shard count from the cluster-template
// annotation, if the operator supplied one. The locked restore CRD has no
// clusterTemplate field (03 §8.1 lists only clusterName/backupName/backupSource/
// strategy), so for this leg the optional template is carried as a JSON blob in the
// valkey.percona.com/restore-cluster-template annotation; only its shards field is
// consumed here. ok=false means inherit the manifest topology.
func templateShards(rst *valkeyv1alpha1.PerconaValkeyRestore) (int, bool) {
	tmpl, ok := clusterTemplate(rst)
	if !ok || tmpl.Shards == 0 {
		return 0, false
	}
	return int(tmpl.Shards), true
}

// clusterTemplate decodes the optional embedded cluster spec carried in the
// cluster-template annotation. Returns ok=false when absent or unparseable (an
// unparseable template is treated as absent and the manifest topology is inherited).
func clusterTemplate(rst *valkeyv1alpha1.PerconaValkeyRestore) (valkeyv1alpha1.PerconaValkeyClusterSpec, bool) {
	if rst.Annotations == nil {
		return valkeyv1alpha1.PerconaValkeyClusterSpec{}, false
	}
	raw, ok := rst.Annotations[annClusterTmpl]
	if !ok || strings.TrimSpace(raw) == "" {
		return valkeyv1alpha1.PerconaValkeyClusterSpec{}, false
	}
	var spec valkeyv1alpha1.PerconaValkeyClusterSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return valkeyv1alpha1.PerconaValkeyClusterSpec{}, false
	}
	return spec, true
}

// provisionTargetCluster implements the NewCluster provision step (06 §7.4): it
// creates (or adopts) the target PerconaValkeyCluster sized to the manifest's shard
// topology and stamps the restore markers the cluster/node controllers consume —
// the restored-from marker (so rolls are gated and the seed init container is
// injected) and the appendonly-no seed override (so the engine loads dump.rdb, not
// an empty AOF — 06 §7.4). The cluster is NOT owned by the restore (its lifecycle
// is independent; a failed restore leaves it for inspection — 06 §9.3). InPlace
// restores skip creation and stamp the markers onto the existing cluster.
//
// The created cluster is created with the restore markers in place from the start so
// the very first cluster reconcile injects the seed init container before the engine
// boots (06 §7.4 — the RDB must be present before the Valkey process starts).
func (r *Reconciler) provisionTargetCluster(
	ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore, src resolvedSource, man backup.Manifest,
) (*valkeyv1alpha1.PerconaValkeyCluster, error) {
	name := targetClusterName(rst)
	key := types.NamespacedName{Name: name, Namespace: rst.Namespace}

	existing := &valkeyv1alpha1.PerconaValkeyCluster{}
	err := r.Get(ctx, key, existing)
	switch {
	case err == nil:
		// Adopt: stamp the restore markers onto the existing cluster (InPlace, or a
		// NewCluster re-reconcile after the create already landed).
		return existing, r.stampRestoreMarkers(ctx, existing, rst, src)
	case apierrors.IsNotFound(err):
		if rst.Spec.Strategy == valkeyv1alpha1.RestoreStrategyInPlace {
			return nil, fmt.Errorf("InPlace restore target cluster %s does not exist", key)
		}
		return r.createTargetCluster(ctx, rst, src, man)
	default:
		return nil, fmt.Errorf("get target cluster %s: %w", key, err)
	}
}

// createTargetCluster builds the new PerconaValkeyCluster from the (optional)
// cluster-template annotation, sizing shards to the manifest, with the restore
// markers stamped at creation time.
func (r *Reconciler) createTargetCluster(
	ctx context.Context, rst *valkeyv1alpha1.PerconaValkeyRestore, src resolvedSource, man backup.Manifest,
) (*valkeyv1alpha1.PerconaValkeyCluster, error) {
	spec, _ := clusterTemplate(rst)
	// Shards always match the manifest topology (06 §7.1 — must equal or be
	// inherited); the exact slot map is reproduced from the manifest at re-form.
	spec.Shards = int32(len(man.Shards))

	cluster := &valkeyv1alpha1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        targetClusterName(rst),
			Namespace:   rst.Namespace,
			Annotations: restoreMarkerAnnotations(rst, src),
		},
		Spec: spec,
	}
	if err := r.Create(ctx, cluster); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a race with our own re-reconcile; re-fetch and adopt.
			fresh := &valkeyv1alpha1.PerconaValkeyCluster{}
			if gerr := r.Get(ctx, client.ObjectKeyFromObject(cluster), fresh); gerr != nil {
				return nil, fmt.Errorf("re-fetch raced target cluster: %w", gerr)
			}
			return fresh, r.stampRestoreMarkers(ctx, fresh, rst, src)
		}
		return nil, fmt.Errorf("create target cluster %s: %w", client.ObjectKeyFromObject(cluster), err)
	}
	r.recorder.Eventf(rst, cluster, eventNormal, EventRestoreProvisioning, "ProvisionCluster",
		"Provisioned target cluster %s with %d shard(s) for restore", cluster.Name, len(man.Shards))
	return cluster, nil
}

// stampRestoreMarkers patches the restore markers onto an existing cluster via
// MergeFrom (never a full Update — the M3 round-trip bug). Idempotent: a no-op when
// the markers already match.
func (r *Reconciler) stampRestoreMarkers(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	rst *valkeyv1alpha1.PerconaValkeyRestore, src resolvedSource,
) error {
	want := restoreMarkerAnnotations(rst, src)
	if annotationsContain(cluster.Annotations, want) {
		return nil
	}
	base := cluster.DeepCopy()
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	for k, v := range want {
		cluster.Annotations[k] = v
	}
	if err := r.Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("stamp restore markers on cluster %s: %w", client.ObjectKeyFromObject(cluster), err)
	}
	return nil
}

// restoreMarkerAnnotations is the marker set the restore stamps on its target
// cluster: the restored-from marker (so the cluster controller gates engine rolls
// and the node controller injects the seed init container — 06 §7.4), the
// appendonly-no seed override (so the engine loads dump.rdb, not an empty AOF —
// 06 §7.4, R3), and the resolved named storage backend (so the cluster controller's
// restore-target seam can populate the required ValkeyNodeSpec.RestoreFrom.Storage
// the seed init container downloads from). The restored-from value records the source
// backup identity for provenance; the storage marker is omitted when no named storage
// resolved (an inline backupSource with no storageName) so the seam falls back to the
// cluster's own resolved backup storage.
func restoreMarkerAnnotations(rst *valkeyv1alpha1.PerconaValkeyRestore, src resolvedSource) map[string]string {
	markers := map[string]string{
		annRestoreMarker:  restoreMarkerValue(rst),
		annSeedAppendonly: seedAppendonlyNo,
	}
	if src.StorageName != "" {
		markers[annRestoreStorage] = src.StorageName
	}
	// The source cluster + backup names (from the manifest) derive the object keys
	// the seed init container downloads; the restored-from provenance marker does not
	// preserve them (it is "backupSource" for an inline source), so stamp them
	// explicitly for the cluster controller's restore-target seam (06 §7.4).
	if src.Cluster != "" {
		markers[annSourceCluster] = src.Cluster
	}
	if src.Backup != "" {
		markers[annSourceBackup] = src.Backup
	}
	return markers
}

// restoreMarkerValue is the provenance string stamped into the restored-from marker:
// the restore CR name and its source reference.
func restoreMarkerValue(rst *valkeyv1alpha1.PerconaValkeyRestore) string {
	if rst.Spec.BackupName != "" {
		return rst.Name + "/" + rst.Spec.BackupName
	}
	return rst.Name + "/backupSource"
}

// annotationsContain reports whether have already carries every key=value in want.
func annotationsContain(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// lastTwoPathComponents splits a "/"-joined object path and returns its final two
// non-empty components (the "<cluster>/<backup>" suffix of a destination path).
// ok=false when fewer than two components are present.
func lastTwoPathComponents(p string) (penultimate, last string, ok bool) {
	parts := make([]string, 0, 4)
	for _, seg := range strings.Split(p, "/") {
		if seg = strings.TrimSpace(seg); seg != "" {
			parts = append(parts, seg)
		}
	}
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[len(parts)-2], parts[len(parts)-1], true
}
