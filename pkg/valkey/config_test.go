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
	"strings"
	"testing"
)

// TestRenderServerConfigRequirepass covers the default-user password (requirepass)
// rendering: present when resolved, absent when empty, and override-proof against
// a user attempt to set it via spec.config.
func TestRenderServerConfigRequirepass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          ConfigInput
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "requirepass rendered when set, with companion masterauth",
			in:   ConfigInput{ACL: true, Requirepass: "s3cr3t"},
			// masterauth MUST mirror requirepass: otherwise a replica cannot
			// authenticate to its passworded primary and master_link_status stays
			// down, so the cluster never reaches Ready.
			wantContain: []string{"requirepass s3cr3t", "masterauth s3cr3t"},
		},
		{
			name:       "no requirepass/masterauth line when empty (auth disabled)",
			in:         ConfigInput{ACL: true},
			wantAbsent: []string{"requirepass", "masterauth"},
		},
		{
			name: "operator requirepass wins over user config",
			in: ConfigInput{
				ACL:         true,
				UserConfig:  map[string]string{"requirepass": "user-tried-this"},
				Requirepass: "operator-managed",
			},
			wantContain: []string{"requirepass operator-managed"},
			wantAbsent:  []string{"requirepass user-tried-this"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := RenderServerConfig(tt.in)
			for _, want := range tt.wantContain {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in:\n%s", want, out)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(out, absent) {
					t.Fatalf("unexpected %q in:\n%s", absent, out)
				}
			}
		})
	}
}

// TestRenderServerConfigDisableCommands covers the rename-command "" lines: one
// per command, sorted, override-proof, blank entries skipped, and none when the
// list is empty (M1 minimal cluster).
func TestRenderServerConfigDisableCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          ConfigInput
		wantContain []string
		wantAbsent  []string
		wantOrder   []string // substrings expected in this relative order
	}{
		{
			name: "default-style disable set renders rename-command lines",
			in:   ConfigInput{ACL: true, DisableCommands: []string{"FLUSHALL", "FLUSHDB"}},
			wantContain: []string{
				`rename-command FLUSHALL ""`,
				`rename-command FLUSHDB ""`,
			},
		},
		{
			name:       "empty list renders no rename-command lines",
			in:         ConfigInput{ACL: true},
			wantAbsent: []string{"rename-command"},
		},
		{
			name:        "blank entries are skipped",
			in:          ConfigInput{ACL: true, DisableCommands: []string{"DEBUG", "   ", ""}},
			wantContain: []string{`rename-command DEBUG ""`},
			wantAbsent:  []string{`rename-command  ""`},
		},
		{
			name:      "lines are emitted in sorted order",
			in:        ConfigInput{ACL: true, DisableCommands: []string{"KEYS", "CONFIG", "FLUSHALL"}},
			wantOrder: []string{"rename-command CONFIG", "rename-command FLUSHALL", "rename-command KEYS"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := RenderServerConfig(tt.in)
			for _, want := range tt.wantContain {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in:\n%s", want, out)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(out, absent) {
					t.Fatalf("unexpected %q in:\n%s", absent, out)
				}
			}
			prev := -1
			for _, sub := range tt.wantOrder {
				idx := strings.Index(out, sub)
				if idx < 0 {
					t.Fatalf("missing ordered substring %q in:\n%s", sub, out)
				}
				if idx < prev {
					t.Fatalf("substring %q out of order in:\n%s", sub, out)
				}
				prev = idx
			}
		})
	}
}

// TestRenderServerConfigDisableCommandsWinOverUserRenameCommand documents that the
// disableCommands lines are appended AFTER the key/value config: a user cannot
// re-enable a disabled command via spec.config because Valkey resolves the LAST
// rename-command directive, and the operator's empty-rename line is emitted last.
func TestRenderServerConfigDisableCommandsAppendedLast(t *testing.T) {
	t.Parallel()
	in := ConfigInput{
		ACL:             true,
		UserConfig:      map[string]string{"appendonly": "yes"},
		DisableCommands: []string{"FLUSHALL"},
	}
	out := RenderServerConfig(in)
	disableIdx := strings.Index(out, `rename-command FLUSHALL ""`)
	kvIdx := strings.Index(out, "appendonly yes")
	if disableIdx < 0 || kvIdx < 0 {
		t.Fatalf("expected both directives present:\n%s", out)
	}
	if disableIdx < kvIdx {
		t.Fatalf("rename-command must be emitted after key/value config:\n%s", out)
	}
}

// TestRenderServerConfigTLSHardening covers each TLS hardening directive:
// authClients enum mapping, ciphers, cipherSuites, and dh-params-file. The
// existing tls-*-file block must stay intact.
func TestRenderServerConfigTLSHardening(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          ConfigInput
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "authClients off maps to no",
			in:          ConfigInput{ACL: true, TLS: true, TLSAuthClients: authClientsOff},
			wantContain: []string{"tls-auth-clients no"},
		},
		{
			name:        "authClients require maps to yes",
			in:          ConfigInput{ACL: true, TLS: true, TLSAuthClients: authClientsRequire},
			wantContain: []string{"tls-auth-clients yes"},
		},
		{
			name:        "authClients optional maps to optional",
			in:          ConfigInput{ACL: true, TLS: true, TLSAuthClients: authClientsOptional},
			wantContain: []string{"tls-auth-clients optional"},
		},
		{
			name:        "empty authClients defaults to optional",
			in:          ConfigInput{ACL: true, TLS: true},
			wantContain: []string{"tls-auth-clients optional"},
		},
		{
			name:        "unrecognized authClients defaults to optional (never silently no)",
			in:          ConfigInput{ACL: true, TLS: true, TLSAuthClients: "bogus"},
			wantContain: []string{"tls-auth-clients optional"},
		},
		{
			name:        "ciphers rendered when set",
			in:          ConfigInput{ACL: true, TLS: true, TLSCiphers: "DEFAULT:!MEDIUM"},
			wantContain: []string{"tls-ciphers DEFAULT:!MEDIUM"},
		},
		{
			name:        "cipherSuites rendered when set",
			in:          ConfigInput{ACL: true, TLS: true, TLSCipherSuites: "TLS_AES_128_GCM_SHA256"},
			wantContain: []string{"tls-ciphersuites TLS_AES_128_GCM_SHA256"},
		},
		{
			name:        "dh-params file rendered when set",
			in:          ConfigInput{ACL: true, TLS: true, TLSDHParamsFile: "/etc/valkey/tls-dhparams/dh-params.pem"},
			wantContain: []string{"tls-dh-params-file /etc/valkey/tls-dhparams/dh-params.pem"},
		},
		{
			name:       "hardening directives absent when TLS off",
			in:         ConfigInput{ACL: true, TLSAuthClients: authClientsRequire, TLSCiphers: "X", TLSCipherSuites: "Y", TLSDHParamsFile: "/z"},
			wantAbsent: []string{"tls-auth-clients", "tls-ciphers", "tls-ciphersuites", "tls-dh-params-file"},
		},
		{
			name: "existing tls-*-file block stays intact alongside hardening",
			in:   ConfigInput{ACL: true, TLS: true, TLSAuthClients: authClientsRequire},
			wantContain: []string{
				"tls-port 6379",
				"port 0",
				"tls-cluster yes",
				"tls-replication yes",
				"tls-cert-file /etc/valkey/tls/tls.crt",
				"tls-key-file /etc/valkey/tls/tls.key",
				"tls-ca-cert-file /etc/valkey/tls/ca.crt",
			},
		},
		{
			name:       "optional hardening directives omitted when unset",
			in:         ConfigInput{ACL: true, TLS: true},
			wantAbsent: []string{"tls-ciphers ", "tls-ciphersuites", "tls-dh-params-file"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := RenderServerConfig(tt.in)
			for _, want := range tt.wantContain {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in:\n%s", want, out)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(out, absent) {
					t.Fatalf("unexpected %q in:\n%s", absent, out)
				}
			}
		})
	}
}

// TestServerConfigRollHashRequirepassAndDisableCommands verifies the roll-hash
// policy for the new fields: a requirepass-only change must NOT roll (rotated
// live), but a disableCommands change MUST roll, and identical inputs are
// deterministic.
func TestServerConfigRollHashRequirepassAndDisableCommands(t *testing.T) {
	t.Parallel()
	base := ConfigInput{ACL: true, Requirepass: "p1", DisableCommands: []string{"FLUSHALL"}}
	h := ServerConfigRollHash(base)

	// requirepass-only change: same hash (live rotation, no roll).
	pwChanged := ConfigInput{ACL: true, Requirepass: "p2", DisableCommands: []string{"FLUSHALL"}}
	if ServerConfigRollHash(pwChanged) != h {
		t.Fatal("requirepass change altered the roll hash (would cause a spurious roll)")
	}
	// Adding requirepass where there was none: still same hash.
	noPass := ConfigInput{ACL: true, DisableCommands: []string{"FLUSHALL"}}
	if ServerConfigRollHash(noPass) != h {
		t.Fatal("presence/absence of requirepass changed the roll hash")
	}
	// disableCommands change: hash MUST change (a real, roll-worthy config change).
	cmdChanged := ConfigInput{ACL: true, Requirepass: "p1", DisableCommands: []string{"FLUSHALL", "FLUSHDB"}}
	if ServerConfigRollHash(cmdChanged) == h {
		t.Fatal("disableCommands change did not change the roll hash")
	}
	// TLS hardening change: hash MUST change (rolls to re-read the directive).
	tlsBase := ConfigInput{ACL: true, TLS: true, TLSAuthClients: authClientsOptional}
	tlsHardened := ConfigInput{ACL: true, TLS: true, TLSAuthClients: authClientsRequire}
	if ServerConfigRollHash(tlsBase) == ServerConfigRollHash(tlsHardened) {
		t.Fatal("tls-auth-clients change did not change the roll hash")
	}
	// Determinism: identical input -> identical hash.
	if ServerConfigRollHash(base) != h {
		t.Fatal("roll hash is non-deterministic for identical input")
	}
}
