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

import "testing"

func TestSlotMigrationIsTerminal(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"success", true},
		{"failed", true},
		{"canceled", true},
		{"cancelled", true},
		{"running", false},
		{"", false},
		{"pending", false},
	}
	for _, tt := range tests {
		if got := (SlotMigration{State: tt.state}).IsTerminal(); got != tt.want {
			t.Errorf("IsTerminal(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestWrapUnsupportedErr(t *testing.T) {
	if wrapUnsupportedErr(nil) != nil {
		t.Error("nil error must stay nil")
	}
	for _, msg := range []string{
		"ERR unknown command 'CLUSTER|MIGRATESLOTS'",
		"ERR Unknown subcommand or wrong number of arguments",
		"ERR wrong number of arguments for 'cluster' command",
	} {
		err := wrapUnsupportedErr(errString(msg))
		if err == nil || !contains(err.Error(), "upgrade to Valkey 9.0") {
			t.Errorf("expected upgrade hint for %q, got %v", msg, err)
		}
	}
	// A plain connection error is returned unchanged.
	plain := errString("connection refused")
	if got := wrapUnsupportedErr(plain); got.Error() != "connection refused" {
		t.Errorf("plain error altered: %v", got)
	}
}

func TestIsSlotsNotServedByNode(t *testing.T) {
	if IsSlotsNotServedByNode(nil) {
		t.Error("nil must be benign-false")
	}
	if !IsSlotsNotServedByNode(errString("ERR Slots are not served by this node")) {
		t.Error("expected benign detection")
	}
	if IsSlotsNotServedByNode(errString("some other error")) {
		t.Error("unrelated error should not match")
	}
}

func TestFailoverModeConstants(t *testing.T) {
	if FailoverGraceful != "" {
		t.Errorf("FailoverGraceful = %q, want empty", FailoverGraceful)
	}
	if FailoverForce != "FORCE" {
		t.Errorf("FailoverForce = %q, want FORCE", FailoverForce)
	}
	if FailoverTakeover != "TAKEOVER" {
		t.Errorf("FailoverTakeover = %q, want TAKEOVER", FailoverTakeover)
	}
}

func TestBusPortConstant(t *testing.T) {
	if BusPort != 16379 {
		t.Errorf("BusPort = %d, want 16379", BusPort)
	}
	if ClientPort != 6379 {
		t.Errorf("ClientPort = %d, want 6379", ClientPort)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
