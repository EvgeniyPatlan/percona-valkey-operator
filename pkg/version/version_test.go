package version

import "testing"

func TestVersion(t *testing.T) {
	if got := Version(); got != "0.1.0" {
		t.Fatalf("Version() = %q, want %q (from version.txt)", got, "0.1.0")
	}
}

func TestCompareVersion(t *testing.T) {
	// Version() is 0.1.0 (major.minor = 0.1).
	tests := []struct {
		name   string
		target string
		want   int
	}{
		{"equal major.minor", "0.1.0", 0},
		{"equal, patch immaterial", "0.1.7", 0},
		{"equal, no patch", "0.1", 0},
		{"operator older minor", "0.2.0", -1},
		{"operator newer minor", "0.0.9", 1},
		{"operator older major", "1.0.0", -1},
		{"operator newer major", "0.1.0", 0},
		{"garbage target defaults to 0.0", "not-a-version", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompareVersion(tt.target); got != tt.want {
				t.Errorf("CompareVersion(%q) = %d, want %d", tt.target, got, tt.want)
			}
		})
	}
}

func TestMajorMinor(t *testing.T) {
	tests := []struct {
		in       string
		maj, mnr int
	}{
		{"1.2.3", 1, 2},
		{"1.2", 1, 2},
		{"1", 1, 0},
		{"", 0, 0},
		{"  3.4.5  ", 3, 4},
		{"x.y.z", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			maj, mnr := majorMinor(tt.in)
			if maj != tt.maj || mnr != tt.mnr {
				t.Errorf("majorMinor(%q) = (%d,%d), want (%d,%d)", tt.in, maj, mnr, tt.maj, tt.mnr)
			}
		})
	}
}

func TestMajorMinorPatchAccessors(t *testing.T) {
	// Version() is 0.1.0 (the embedded version.txt seed).
	if got := Major(); got != 0 {
		t.Errorf("Major() = %d, want 0", got)
	}
	if got := Minor(); got != 1 {
		t.Errorf("Minor() = %d, want 1", got)
	}
	if got := Patch(); got != 0 {
		t.Errorf("Patch() = %d, want 0", got)
	}
}

func TestCompareMajorMinor(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"equal", "1.1.0", "1.1.5", 0},
		{"equal no patch", "1.1", "1.1.0", 0},
		{"a older minor", "1.0", "1.1", -1},
		{"a newer minor", "1.2", "1.1", 1},
		{"a older major", "1.9", "2.0", -1},
		{"cross-major one step", "2.0", "1.2", 1},
		{"empty a sorts lowest", "", "1.0", -1},
		{"both empty equal", "", "", 0},
		{"garbage sorts as 0.0", "x.y", "0.0", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompareMajorMinor(tt.a, tt.b); got != tt.want {
				t.Errorf("CompareMajorMinor(%q,%q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestSign(t *testing.T) {
	if sign(0) != 0 {
		t.Errorf("sign(0) = %d, want 0", sign(0))
	}
	if sign(-5) != -1 {
		t.Errorf("sign(-5) = %d, want -1", sign(-5))
	}
	if sign(5) != 1 {
		t.Errorf("sign(5) = %d, want 1", sign(5))
	}
}

// TestAcceptedCrVersions pins the 09 §8 compatibility matrix exactly: own minor
// plus the immediately-preceding released minor in release order, computed (not
// hardcoded), cross-major aware (2.0 => {1.2, 2.0}), with 1.0 having no
// predecessor line.
func TestAcceptedCrVersions(t *testing.T) {
	tests := []struct {
		op   string
		want []string
	}{
		{"1.0", []string{"1.0"}},
		{"1.1", []string{"1.0", "1.1"}},
		{"1.2", []string{"1.1", "1.2"}},
		{"2.0", []string{"1.2", "2.0"}}, // cross-major one-step-back
		{"2.1", []string{"2.0", "2.1"}},
		{"3.0", []string{"2.2", "3.0"}},
		{"0.1", []string{"0.0", "0.1"}}, // pre-1.0 dev line: within-major rule
	}
	for _, tt := range tests {
		got := AcceptedCrVersions(tt.op)
		if len(got) != len(tt.want) {
			t.Fatalf("AcceptedCrVersions(%q) = %v, want %v", tt.op, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("AcceptedCrVersions(%q) = %v, want %v", tt.op, got, tt.want)
			}
		}
	}
}
