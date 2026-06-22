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
	"reflect"
	"testing"
)

func TestTargetSlotsPerShard(t *testing.T) {
	// The split table from 05 §2.
	tests := []struct {
		shards int
		want   []int
	}{
		{1, []int{16384}},
		{2, []int{8192, 8192}},
		{3, []int{5462, 5461, 5461}},
		{4, []int{4096, 4096, 4096, 4096}},
		{5, []int{3277, 3277, 3277, 3277, 3276}},
		{6, []int{2731, 2731, 2731, 2731, 2730, 2730}},
	}
	for _, tt := range tests {
		got := targetSlotsPerShard(tt.shards)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("targetSlotsPerShard(%d) = %v, want %v", tt.shards, got, tt.want)
		}
		sum := 0
		for _, n := range got {
			sum += n
		}
		if sum != TotalSlots {
			t.Errorf("targetSlotsPerShard(%d) sums to %d, want %d", tt.shards, sum, TotalSlots)
		}
	}
}

func TestTargetSlotsPerShard_Invalid(t *testing.T) {
	if got := targetSlotsPerShard(0); got != nil {
		t.Errorf("targetSlotsPerShard(0) = %v, want nil", got)
	}
	if got := targetSlotsPerShard(-3); got != nil {
		t.Errorf("targetSlotsPerShard(-3) = %v, want nil", got)
	}
}

func TestGetUnassignedSlots(t *testing.T) {
	tests := []struct {
		name   string
		shards []*ShardState
		want   []SlotRange
	}{
		{
			name:   "full coverage",
			shards: []*ShardState{{Slots: []SlotRange{{0, 16383}}}},
			want:   nil,
		},
		{
			name:   "missing slot 0",
			shards: []*ShardState{{Slots: []SlotRange{{1, 16383}}}},
			want:   []SlotRange{{0, 0}},
		},
		{
			name: "three shards with gaps",
			shards: []*ShardState{
				{Slots: []SlotRange{{100, 200}, {300, 400}}},
				{Slots: []SlotRange{{700, 800}}},
				{Slots: []SlotRange{{500, 600}}},
			},
			want: []SlotRange{{0, 99}, {201, 299}, {401, 499}, {601, 699}, {801, 16383}},
		},
		{
			name:   "empty cluster — everything unassigned",
			shards: nil,
			want:   []SlotRange{{0, 16383}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &ClusterState{Shards: tt.shards}
			got := s.GetUnassignedSlots()
			// Full coverage may return an empty (non-nil) slice; assert by
			// length so the "no gaps" contract is what's checked.
			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Errorf("GetUnassignedSlots() = %v, want no gaps", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetUnassignedSlots() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSubtractSlotRange(t *testing.T) {
	tests := []struct {
		base, remove SlotRange
		want         []SlotRange
	}{
		{SlotRange{0, 16383}, SlotRange{10, 16380}, []SlotRange{{0, 9}, {16381, 16383}}},
		{SlotRange{0, 10}, SlotRange{5, 10}, []SlotRange{{0, 4}}},
		{SlotRange{0, 10}, SlotRange{0, 9}, []SlotRange{{10, 10}}},
		{SlotRange{0, 10}, SlotRange{0, 10}, nil},
		{SlotRange{0, 10}, SlotRange{20, 30}, []SlotRange{{0, 10}}}, // no overlap
	}
	for _, tt := range tests {
		got := subtractSlotRange(tt.base, tt.remove)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("subtract(%v,%v) = %v, want %v", tt.base, tt.remove, got, tt.want)
		}
	}
}

func TestNormalizeSlotRanges(t *testing.T) {
	tests := []struct {
		name string
		in   []SlotRange
		want []SlotRange
	}{
		{"empty", nil, nil},
		{"single", []SlotRange{{5, 10}}, []SlotRange{{5, 10}}},
		{"adjacent merge", []SlotRange{{0, 5}, {6, 10}}, []SlotRange{{0, 10}}},
		{"overlap merge", []SlotRange{{0, 8}, {5, 10}}, []SlotRange{{0, 10}}},
		{"unsorted", []SlotRange{{6, 10}, {0, 5}}, []SlotRange{{0, 10}}},
		{"disjoint kept", []SlotRange{{0, 5}, {10, 15}}, []SlotRange{{0, 5}, {10, 15}}},
		{"contained", []SlotRange{{0, 100}, {10, 20}}, []SlotRange{{0, 100}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeSlotRanges(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Normalize(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSlotsToRanges(t *testing.T) {
	tests := []struct {
		in   []int
		want []SlotRange
	}{
		{nil, nil},
		{[]int{5}, []SlotRange{{5, 5}}},
		{[]int{0, 1, 2, 3}, []SlotRange{{0, 3}}},
		{[]int{0, 1, 5, 6, 7}, []SlotRange{{0, 1}, {5, 7}}},
		{[]int{7, 6, 5, 1, 0}, []SlotRange{{0, 1}, {5, 7}}}, // unsorted input
	}
	for _, tt := range tests {
		got := SlotsToRanges(tt.in)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SlotsToRanges(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestTakeSlots(t *testing.T) {
	ranges := []SlotRange{{0, 4}, {100, 104}}
	if got := takeSlots(ranges, 0); got != nil {
		t.Errorf("takeSlots(_,0) = %v, want nil", got)
	}
	got := takeSlots(ranges, 3)
	if !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("takeSlots(_,3) = %v, want [0 1 2]", got)
	}
	got = takeSlots(ranges, 7)
	if !reflect.DeepEqual(got, []int{0, 1, 2, 3, 4, 100, 101}) {
		t.Errorf("takeSlots(_,7) crossing ranges = %v", got)
	}
	// Asking for more than available returns all.
	got = takeSlots(ranges, 100)
	if len(got) != 10 {
		t.Errorf("takeSlots(_,100) len = %d, want 10", len(got))
	}
}

func TestParseSlotRange(t *testing.T) {
	tests := []struct {
		in      string
		want    SlotRange
		wantErr bool
	}{
		{"5", SlotRange{5, 5}, false},
		{"0-16383", SlotRange{0, 16383}, false},
		{"abc", SlotRange{}, true},
		{"5-x", SlotRange{}, true},
		{"x-5", SlotRange{}, true},
		{"10-5", SlotRange{}, true}, // start > end
	}
	for _, tt := range tests {
		got, err := parseSlotRange(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseSlotRange(%q) err = nil, want error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSlotRange(%q) err = %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("parseSlotRange(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSlotRangeStringAndFormat(t *testing.T) {
	if got := (SlotRange{5, 5}).String(); got != "5" {
		t.Errorf("single = %q, want 5", got)
	}
	if got := (SlotRange{0, 5461}).String(); got != "0-5461" {
		t.Errorf("range = %q, want 0-5461", got)
	}
	ranges := []SlotRange{{0, 100}, {500, 600}}
	if got := FormatSlotRanges(ranges); got != "0-100,500-600" {
		t.Errorf("FormatSlotRanges = %q", got)
	}
	if got := CountSlots(ranges); got != 202 {
		t.Errorf("CountSlots = %d, want 202", got)
	}
}
