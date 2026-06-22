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
	"strconv"
	"strings"

	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// parseManifestSlotRanges parses a manifest ShardManifest.SlotRanges field — the
// CLUSTER NODES form "0-5460" or "0-100,500-600" — into typed valkey.SlotRange
// values. It fails loudly on a malformed token so a corrupt manifest can never be
// silently mis-restored (06 §1.3 "no silent data loss"). An empty string yields no
// ranges (a shard that owned no slots at snapshot time).
func parseManifestSlotRanges(s string) ([]valkey.SlotRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	tokens := strings.Split(s, ",")
	ranges := make([]valkey.SlotRange, 0, len(tokens))
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		rng, err := parseOneRange(tok)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, rng)
	}
	return ranges, nil
}

// parseOneRange parses a single CLUSTER NODES slot token: "5" (single slot) or
// "0-5460" (inclusive range), validating bounds against the 0..TotalSlots-1 space.
func parseOneRange(tok string) (valkey.SlotRange, error) {
	lo, hi, isRange := strings.Cut(tok, "-")
	start, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return valkey.SlotRange{}, fmt.Errorf("invalid slot %q: %w", tok, err)
	}
	end := start
	if isRange {
		end, err = strconv.Atoi(strings.TrimSpace(hi))
		if err != nil {
			return valkey.SlotRange{}, fmt.Errorf("invalid slot range %q: %w", tok, err)
		}
	}
	if start < 0 || end >= valkey.TotalSlots || start > end {
		return valkey.SlotRange{}, fmt.Errorf("slot range %q is out of bounds (want 0..%d, start<=end)", tok, valkey.TotalSlots-1)
	}
	return valkey.SlotRange{Start: start, End: end}, nil
}

// coverageVerdict is the result of unioning every shard's slot ranges and checking
// the union against the full 0..16383 space.
type coverageVerdict struct {
	// Covered is the number of distinct slots covered by the union.
	Covered int
	// Complete is true iff the union is exactly all 16384 slots with no gap.
	Complete bool
	// Overlap is true iff two shards claimed the same slot (a corrupt manifest).
	Overlap bool
	// Detail is a human-readable summary for status.stateDescription / Events.
	Detail string
}

// validateManifestCoverage unions the per-shard slot ranges recorded in the manifest
// and reports whether they cover all 16384 slots with no gap and no overlap — the
// 06 §1.3 / §7.5 slot-completeness invariant that gates Succeeded. It is the
// manifest-side mirror of the cluster controller's live GetUnassignedSlots() check
// and lets the restore reject a partial-coverage source before provisioning anything
// (06 §7.5 source-coverage precondition).
func validateManifestCoverage(ranges [][]valkey.SlotRange) (coverageVerdict, error) {
	seen := make([]bool, valkey.TotalSlots)
	overlap := false
	covered := 0
	for _, shard := range ranges {
		for _, rng := range shard {
			if rng.Start < 0 || rng.End >= valkey.TotalSlots || rng.Start > rng.End {
				return coverageVerdict{}, fmt.Errorf("slot range %s out of bounds", rng.String())
			}
			for slot := rng.Start; slot <= rng.End; slot++ {
				if seen[slot] {
					overlap = true
					continue
				}
				seen[slot] = true
				covered++
			}
		}
	}
	complete := covered == valkey.TotalSlots && !overlap
	v := coverageVerdict{Covered: covered, Complete: complete, Overlap: overlap}
	switch {
	case overlap:
		v.Detail = fmt.Sprintf("slot overlap detected (%d/%d slots, shards claim duplicate slots)", covered, valkey.TotalSlots)
	case complete:
		v.Detail = fmt.Sprintf("all %d slots covered", valkey.TotalSlots)
	default:
		v.Detail = fmt.Sprintf("incomplete slot coverage: %d/%d slots (gap)", covered, valkey.TotalSlots)
	}
	return v, nil
}
