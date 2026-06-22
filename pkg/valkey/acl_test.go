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

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// TestBackupRulesCarrySyncGrants locks the M6 SECURITY REFACTOR (07 §10): the
// SYNC-as-replica replication grants live on _backup (the snapshot+replication
// user the backup Job AUTHs as), and _operator carries NEITHER the replication
// grants NOR +bgsave — it is narrowed back to the canonical orchestration-only
// floor. The replication tokens are APPENDED after _backup's canonical +ping so
// its original 07 §4.3 substring stays contiguous.
func TestBackupRulesCarrySyncGrants(t *testing.T) {
	t.Parallel()
	users := SystemUsers(true)
	operator := users[0].Rules
	backup := users[2].Rules

	// _operator is narrowed: it must be EXACTLY the canonical orchestration floor,
	// with no replication or snapshot grant left behind.
	if operator != wantOperatorRules {
		t.Fatalf("operator rules drifted (should be orchestration-only):\n got: %q\nwant: %q", operator, wantOperatorRules)
	}
	for _, tok := range []string{"+bgsave", "+sync", "+psync", "+replconf"} {
		if strings.Contains(operator, " "+tok) {
			t.Fatalf("operator STILL carries snapshot/replication grant %q (trust-boundary regression, 07 §10):\n%s", tok, operator)
		}
	}

	// _backup is the snapshot+replication user: canonical prefix preserved
	// contiguous, with the SYNC-as-replica grants APPENDED.
	if !strings.HasPrefix(backup, wantBackupBaseRules+" ") {
		t.Fatalf("backup rules lost the canonical prefix:\n got: %q\nwant prefix: %q", backup, wantBackupBaseRules)
	}
	for _, tok := range []string{"+bgsave", "+sync", "+psync", "+replconf"} {
		if !strings.Contains(backup, " "+tok) {
			t.Fatalf("backup rules missing replication grant %q (SYNC-as-replica backup would stay blocked):\n%s", tok, backup)
		}
	}
	if backup != wantBackupRules {
		t.Fatalf("backup rules drifted:\n got: %q\nwant: %q", backup, wantBackupRules)
	}
}

// TestBuildUserACL is the table-driven renderer test: each UserACLSpec maps to
// an EXACT `user <name> ...` line in the fixed 07 §4.1 token order.
func TestBuildUserACL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		user     valkeyv1alpha1.UserACLSpec
		pwHashes []string
		want     string
	}{
		{
			name: "full app user (07 §4.1 worked example)",
			user: valkeyv1alpha1.UserACLSpec{
				Name:    "app",
				Enabled: true,
				Commands: &valkeyv1alpha1.UserCommands{
					Allow: []string{"@read", "@write"},
					Deny:  []string{"@admin", "@dangerous", "flushall", "flushdb"},
				},
				Keys:     &valkeyv1alpha1.UserKeys{ReadWrite: []string{"app:*"}},
				Channels: &valkeyv1alpha1.UserChannels{Patterns: []string{"app.*"}},
			},
			pwHashes: []string{"#aaa", "#bbb"},
			want:     "user app on #aaa #bbb ~app:* resetchannels &app.* +@read +@write -@admin -@dangerous -flushall -flushdb",
		},
		{
			name: "multi-password rotation (two #hashes)",
			user: valkeyv1alpha1.UserACLSpec{
				Name:    "rot",
				Enabled: true,
				Commands: &valkeyv1alpha1.UserCommands{
					Allow: []string{"@read"},
				},
			},
			pwHashes: []string{"#h1", "#h2"},
			want:     "user rot on #h1 #h2 +@read",
		},
		{
			name: "disabled user renders off",
			user: valkeyv1alpha1.UserACLSpec{
				Name:    "stale",
				Enabled: false,
			},
			pwHashes: []string{"#h"},
			want:     "user stale off #h",
		},
		{
			name: "nopass user (passwordless, hashes ignored)",
			user: valkeyv1alpha1.UserACLSpec{
				Name:     "anon",
				Enabled:  true,
				Nopass:   true,
				Commands: &valkeyv1alpha1.UserCommands{Allow: []string{"@read"}},
			},
			pwHashes: []string{"#ignored"},
			want:     "user anon on nopass +@read",
		},
		{
			name: "resetpass strips passwords (lock-out) and wins over nopass",
			user: valkeyv1alpha1.UserACLSpec{
				Name:      "locked",
				Enabled:   true,
				Resetpass: true,
				Nopass:    true,
				Commands:  &valkeyv1alpha1.UserCommands{Allow: []string{"get"}},
			},
			pwHashes: []string{"#ignored"},
			want:     "user locked on resetpass +get",
		},
		{
			name: "all three key classes in fixed order ~ %R~ %W~",
			user: valkeyv1alpha1.UserACLSpec{
				Name:    "keys",
				Enabled: true,
				Keys: &valkeyv1alpha1.UserKeys{
					ReadWrite: []string{"rw:*"},
					ReadOnly:  []string{"ro:*"},
					WriteOnly: []string{"wo:*"},
				},
			},
			pwHashes: []string{"#h"},
			want:     "user keys on #h ~rw:* %R~ro:* %W~wo:*",
		},
		{
			name: "channels emit resetchannels once before patterns",
			user: valkeyv1alpha1.UserACLSpec{
				Name:     "chan",
				Enabled:  true,
				Channels: &valkeyv1alpha1.UserChannels{Patterns: []string{"a.*", "b.*"}},
			},
			pwHashes: []string{"#h"},
			want:     "user chan on #h resetchannels &a.* &b.*",
		},
		{
			name: "raw permissions appended verbatim last",
			user: valkeyv1alpha1.UserACLSpec{
				Name:        "raw",
				Enabled:     true,
				Commands:    &valkeyv1alpha1.UserCommands{Allow: []string{"@read"}},
				Permissions: "+@connection -debug",
			},
			pwHashes: []string{"#h"},
			want:     "user raw on #h +@read +@connection -debug",
		},
		{
			name: "no password, no nopass, no resetpass (cannot auth until pw added)",
			user: valkeyv1alpha1.UserACLSpec{
				Name:     "pending",
				Enabled:  true,
				Commands: &valkeyv1alpha1.UserCommands{Allow: []string{"@read"}},
			},
			pwHashes: nil,
			want:     "user pending on +@read",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := BuildUserACL(tc.user, tc.pwHashes)
			if got != tc.want {
				t.Fatalf("BuildUserACL line mismatch:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestBuildUserACLDeterministic asserts byte-stability for an identical spec so
// the rendered users.acl hash does not churn (04 §3 no-spurious-roll rule).
func TestBuildUserACLDeterministic(t *testing.T) {
	t.Parallel()
	u := valkeyv1alpha1.UserACLSpec{
		Name:     "det",
		Enabled:  true,
		Keys:     &valkeyv1alpha1.UserKeys{ReadWrite: []string{"k:*"}},
		Channels: &valkeyv1alpha1.UserChannels{Patterns: []string{"c.*"}},
		Commands: &valkeyv1alpha1.UserCommands{Allow: []string{"@read"}, Deny: []string{"@admin"}},
	}
	first := BuildUserACL(u, []string{"#h1", "#h2"})
	for i := 0; i < 5; i++ {
		if BuildUserACL(u, []string{"#h1", "#h2"}) != first {
			t.Fatal("BuildUserACL is not deterministic for an identical spec")
		}
	}
}

// TestBuildUserACLNoCleartext is a defence-in-depth check: BuildUserACL only
// ever emits the caller-supplied hash tokens, never a cleartext password.
func TestBuildUserACLNoCleartext(t *testing.T) {
	t.Parallel()
	// The caller is responsible for hashing; BuildUserACL must emit exactly what
	// it is given. Verify it does not echo any extra material.
	got := BuildUserACL(valkeyv1alpha1.UserACLSpec{Name: "u", Enabled: true}, []string{HashACLPassword("super-secret")})
	if strings.Contains(got, "super-secret") {
		t.Fatalf("cleartext leaked through BuildUserACL: %s", got)
	}
	if !strings.Contains(got, "#") {
		t.Fatalf("expected a hashed password token: %s", got)
	}
}
