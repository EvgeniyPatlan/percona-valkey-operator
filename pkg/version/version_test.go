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
