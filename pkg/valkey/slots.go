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

package valkey

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// TotalSlots is the fixed number of hash slots in a Valkey cluster: slots
// 0..16383 (05 §2). Full coverage of all TotalSlots slots is required before
// the cluster is marked Ready.
const TotalSlots = 16384

// maxSlot is the highest valid slot index (TotalSlots-1).
const maxSlot = TotalSlots - 1

// BatchSize is the maximum number of slots moved in a single atomic
// CLUSTER MIGRATESLOTS per reconcile (05 §4, §12). One move per reconcile gives
// cluster-aware clients time to absorb each -MOVED redirect.
const BatchSize = 400

// SlotRange is an inclusive interval of hash slots. A single slot is encoded
// as Start == End. The total assignable space is 0..maxSlot.
type SlotRange struct {
	// Start is the inclusive low slot.
	Start int
	// End is the inclusive high slot.
	End int
}

// String formats a SlotRange the same way Valkey prints it in CLUSTER NODES:
// "5" for a single slot, "0-5461" for a range.
func (r SlotRange) String() string {
	if r.Start == r.End {
		return strconv.Itoa(r.Start)
	}
	return strconv.Itoa(r.Start) + "-" + strconv.Itoa(r.End)
}

// Count returns the number of slots covered by the range (inclusive).
func (r SlotRange) Count() int {
	return r.End - r.Start + 1
}

// FormatSlotRanges joins ranges as "0-100,500-600" (the CLUSTER NODES form).
func FormatSlotRanges(ranges []SlotRange) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}

// CountSlots returns the total number of slots across all ranges.
func CountSlots(ranges []SlotRange) int {
	total := 0
	for _, r := range ranges {
		total += r.Count()
	}
	return total
}

// targetSlotsPerShard returns the per-shard slot target for n shards: every
// shard gets TotalSlots/n, and the remainder TotalSlots%n is handed out one
// extra slot at a time to the lowest-addressed shards (05 §2). For n=3 this
// yields [5462, 5461, 5461]. Returns nil for n <= 0.
func targetSlotsPerShard(n int) []int {
	if n <= 0 {
		return nil
	}
	base, rem := TotalSlots/n, TotalSlots%n
	out := make([]int, n)
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

// SplitUnassignedEvenly partitions the given unassigned slot ranges into n
// contiguous, address-order chunks summing to exactly the input slot count. The
// first remainder chunks get one extra slot (so 16384 over 3 -> 5462/5461/5461),
// matching targetSlotsPerShard's remainder-to-lowest distribution (05 §2-§3).
// Slots are handed out low-to-high so two reconcilers compute identical chunks.
// Returns n slices (some possibly empty if there are fewer slots than n).
func SplitUnassignedEvenly(unassigned []SlotRange, n int) [][]SlotRange {
	out := make([][]SlotRange, n)
	if n <= 0 {
		return out
	}
	total := CountSlots(unassigned)
	base, rem := total/n, total%n
	for i := range out {
		want := base
		if i < rem {
			want++
		}
		if want == 0 {
			continue
		}
		slots := takeSlots(unassigned, want)
		out[i] = SlotsToRanges(slots)
		unassigned = removeLowestSlots(unassigned, want)
	}
	return out
}

// removeLowestSlots drops the lowest n slots from ranges, returning the rest.
func removeLowestSlots(ranges []SlotRange, n int) []SlotRange {
	remaining := make([]SlotRange, 0, len(ranges))
	for _, r := range ranges {
		count := r.Count()
		switch {
		case n >= count:
			n -= count
		case n > 0:
			remaining = append(remaining, SlotRange{Start: r.Start + n, End: r.End})
			n = 0
		default:
			remaining = append(remaining, r)
		}
	}
	return remaining
}

// subtractSlotRange removes the slots in remove from base, returning the
// remaining (possibly two) fragments. Used by GetUnassignedSlots to carve
// assigned ranges out of the full 0..maxSlot space.
func subtractSlotRange(base, remove SlotRange) []SlotRange {
	// No overlap: base survives intact.
	if remove.End < base.Start || remove.Start > base.End {
		return []SlotRange{base}
	}
	var result []SlotRange
	if remove.Start > base.Start {
		result = append(result, SlotRange{Start: base.Start, End: remove.Start - 1})
	}
	if remove.End < base.End {
		result = append(result, SlotRange{Start: remove.End + 1, End: base.End})
	}
	return result
}

// NormalizeSlotRanges sorts ranges by Start and merges adjacent/overlapping
// ones into the minimal set of disjoint ranges. The input is not mutated.
func NormalizeSlotRanges(ranges []SlotRange) []SlotRange {
	if len(ranges) == 0 {
		return nil
	}
	sorted := append([]SlotRange(nil), ranges...)
	slices.SortFunc(sorted, func(a, b SlotRange) int {
		if a.Start != b.Start {
			return a.Start - b.Start
		}
		return a.End - b.End
	})
	out := []SlotRange{sorted[0]}
	for _, r := range sorted[1:] {
		last := &out[len(out)-1]
		// Merge when r starts within or immediately after the current range.
		if r.Start <= last.End+1 {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// SlotsToRanges collapses a slice of individual slot numbers into the minimal
// set of contiguous ranges. The input is not mutated.
func SlotsToRanges(slots []int) []SlotRange {
	if len(slots) == 0 {
		return nil
	}
	ordered := append([]int(nil), slots...)
	slices.Sort(ordered)
	ranges := make([]SlotRange, 0, len(ordered))
	start, prev := ordered[0], ordered[0]
	for _, slot := range ordered[1:] {
		if slot == prev+1 {
			prev = slot
			continue
		}
		ranges = append(ranges, SlotRange{Start: start, End: prev})
		start, prev = slot, slot
	}
	return append(ranges, SlotRange{Start: start, End: prev})
}

// takeSlots returns the first n slots (low-to-high) from ranges, expanded as a
// flat slice. Used to carve a migration batch off a shard's owned ranges.
func takeSlots(ranges []SlotRange, n int) []int {
	if n <= 0 {
		return nil
	}
	out := make([]int, 0, n)
	for _, r := range ranges {
		for slot := r.Start; slot <= r.End && len(out) < n; slot++ {
			out = append(out, slot)
		}
		if len(out) == n {
			break
		}
	}
	return out
}

// parseSlotRanges parses CLUSTER NODES slot fields into SlotRanges, skipping the
// importing/migrating markers "[slot-><id>]" / "[slot-<-<id>]" (those are not
// stably-owned ranges). Returns an error on a malformed numeric range.
func parseSlotRanges(fields []string) ([]SlotRange, error) {
	ranges := make([]SlotRange, 0, len(fields))
	for _, f := range fields {
		if strings.HasPrefix(f, "[") {
			// Migrating/importing marker — not an assignable range.
			continue
		}
		r, err := parseSlotRange(f)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, r)
	}
	return ranges, nil
}

// parseSlotRange parses one CLUSTER NODES slot token: "5" (single) or "0-5461"
// (range). Returns an error when the token is not a valid slot or range.
func parseSlotRange(s string) (SlotRange, error) {
	start, end, isRange := strings.Cut(s, "-")
	if !isRange {
		n, err := strconv.Atoi(s)
		if err != nil {
			return SlotRange{}, fmt.Errorf("invalid slot %q: %w", s, err)
		}
		return SlotRange{Start: n, End: n}, nil
	}
	lo, err := strconv.Atoi(start)
	if err != nil {
		return SlotRange{}, fmt.Errorf("invalid slot range %q: %w", s, err)
	}
	hi, err := strconv.Atoi(end)
	if err != nil {
		return SlotRange{}, fmt.Errorf("invalid slot range %q: %w", s, err)
	}
	if lo > hi {
		return SlotRange{}, fmt.Errorf("invalid slot range %q: start > end", s)
	}
	return SlotRange{Start: lo, End: hi}, nil
}
