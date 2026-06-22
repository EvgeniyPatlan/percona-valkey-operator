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
	"fmt"
	"strings"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// manifestCoverage value carried on the manifest (backup.SlotCoverage strings).
const (
	coverageComplete = "complete"
	coveragePartial  = "partial"
)

// validateCompat runs every pre-provision gate the doc requires BEFORE any cluster
// is created (06 §7.5 source-coverage precondition, §9.3 fail-loud table):
//
//   - the manifest declares at least one shard;
//   - every shard's slot ranges parse and union to exactly 16384 slots with no gap
//     or overlap (the slot-completeness invariant, 06 §1.3);
//   - a partial-coverage source (manifest slotCoverage=partial) is REJECTED unless
//     the user set the valkey.percona.com/allow-partial-restore annotation (06 §7.5);
//   - the spec shard count, when present, equals the manifest shard count.
//
// On success it returns the coverage verdict so the caller records restoredSlots.
func validateCompat(rst *valkeyv1alpha1.PerconaValkeyRestore, man backup.Manifest) (coverageVerdict, error) {
	if len(man.Shards) == 0 {
		return coverageVerdict{}, fmt.Errorf("manifest records no shards — nothing to restore")
	}

	perShard := make([][]valkey.SlotRange, 0, len(man.Shards))
	for i := range man.Shards {
		sh := man.Shards[i]
		ranges, err := parseManifestSlotRanges(sh.SlotRanges)
		if err != nil {
			return coverageVerdict{}, fmt.Errorf("shard %d slot ranges %q: %w", sh.Index, sh.SlotRanges, err)
		}
		perShard = append(perShard, ranges)
	}

	verdict, err := validateManifestCoverage(perShard)
	if err != nil {
		return coverageVerdict{}, err
	}

	// Partial-coverage gate (06 §7.5): a partial set cannot rebuild a healthy
	// cluster (it would leave cluster_state:fail), so reject by default unless the
	// operator explicitly opts in. We treat BOTH the manifest's own verdict and our
	// recomputed union: either being partial trips the gate.
	partial := verdict.Overlap || !verdict.Complete || strings.EqualFold(man.SlotCoverage, coveragePartial)
	if partial && !allowPartial(rst) {
		return coverageVerdict{}, fmt.Errorf(
			"source backup has partial slot coverage (%s) — set annotation %s=true to restore a degraded set",
			verdict.Detail, annAllowPartial)
	}

	if err := validateShardCount(rst, man); err != nil {
		return coverageVerdict{}, err
	}
	return verdict, nil
}

// allowPartial reports whether the operator opted into restoring a partial-coverage
// source via the valkey.percona.com/allow-partial-restore annotation (06 §7.5).
func allowPartial(rst *valkeyv1alpha1.PerconaValkeyRestore) bool {
	if rst.Annotations == nil {
		return false
	}
	return truthy(rst.Annotations[annAllowPartial])
}

// validateShardCount enforces 06 §7.1: the target cluster's shard count must equal
// the manifest's shard count (or be inherited). The locked restore CRD has no
// clusterTemplate, so the desired shard count comes from an optional integer carried
// in the cluster-template annotation (see provision.go); when absent it is inherited
// from the manifest and always matches.
func validateShardCount(rst *valkeyv1alpha1.PerconaValkeyRestore, man backup.Manifest) error {
	want, ok := templateShards(rst)
	if !ok {
		return nil // inherit the manifest shard count — always matches.
	}
	if want != len(man.Shards) {
		return fmt.Errorf("clusterTemplate shards=%d does not match manifest shard count=%d", want, len(man.Shards))
	}
	return nil
}
