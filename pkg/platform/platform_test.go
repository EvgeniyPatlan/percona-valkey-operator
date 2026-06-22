package platform

import "testing"

func TestDetectReturnsVanilla(t *testing.T) {
	if got := Detect(); got != Vanilla {
		t.Fatalf("Detect() = %q, want %q (OpenShift detection is M5)", got, Vanilla)
	}
}

func TestPlatformString(t *testing.T) {
	if Vanilla.String() != "vanilla" {
		t.Errorf("Vanilla.String() = %q, want %q", Vanilla.String(), "vanilla")
	}
	if OpenShift.String() != "openshift" {
		t.Errorf("OpenShift.String() = %q, want %q", OpenShift.String(), "openshift")
	}
}
