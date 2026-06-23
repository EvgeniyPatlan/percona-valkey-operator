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
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestRenderServerConfigBaseOverridesUser(t *testing.T) {
	t.Parallel()
	in := ConfigInput{
		UserConfig: map[string]string{
			"cluster-enabled":      "no",    // base must override to "yes".
			"cluster-node-timeout": "20000", // user-tunable: must survive.
			"maxmemory":            "1gb",   // live-settable: rendered but not hashed.
		},
		ACL: true,
	}
	out := RenderServerConfig(in)
	if !strings.Contains(out, "cluster-enabled yes") {
		t.Fatalf("base did not override cluster-enabled:\n%s", out)
	}
	if strings.Contains(out, "cluster-enabled no") {
		t.Fatalf("user value leaked through override-proof base:\n%s", out)
	}
	if !strings.Contains(out, "cluster-node-timeout 20000") {
		t.Fatalf("user-tunable cluster-node-timeout was clobbered:\n%s", out)
	}
	if !strings.Contains(out, "aclfile /config/users/users.acl") {
		t.Fatalf("aclfile missing when ACL=true:\n%s", out)
	}
	if !strings.Contains(out, "maxmemory 1gb") {
		t.Fatalf("live-settable maxmemory should be rendered into the file:\n%s", out)
	}
}

func TestRenderServerConfigTLSAndPersistenceGated(t *testing.T) {
	t.Parallel()
	off := RenderServerConfig(ConfigInput{ACL: true})
	if strings.Contains(off, "tls-port") {
		t.Fatalf("tls directives present with TLS off:\n%s", off)
	}
	on := RenderServerConfig(ConfigInput{ACL: true, TLS: true, Persistence: true})
	for _, want := range []string{"tls-port 6379", "tls-cluster yes", "dir /data", "cluster-config-file /data/nodes.conf"} {
		if !strings.Contains(on, want) {
			t.Fatalf("missing %q with TLS+persistence on:\n%s", want, on)
		}
	}
}

func TestServerConfigRollHashExcludesLiveKeysAndIsDeterministic(t *testing.T) {
	t.Parallel()
	base := ConfigInput{UserConfig: map[string]string{"appendonly": "yes"}, ACL: true}
	h1 := ServerConfigRollHash(base)

	// Changing only a live-settable key must NOT change the hash (no roll).
	withLive := ConfigInput{UserConfig: map[string]string{"appendonly": "yes", "maxmemory": "2gb"}, ACL: true}
	if ServerConfigRollHash(withLive) != h1 {
		t.Fatal("live-settable key changed the roll hash (would cause a spurious roll)")
	}
	// Identical spec -> identical hash (determinism, no phantom rolls).
	if ServerConfigRollHash(base) != h1 {
		t.Fatal("hash is non-deterministic for identical spec")
	}
	// A real (roll-triggering) change DOES change the hash.
	rolled := ConfigInput{UserConfig: map[string]string{"appendonly": "no"}, ACL: true}
	if ServerConfigRollHash(rolled) == h1 {
		t.Fatal("a roll-triggering config change did not change the hash")
	}
}

// canonical ACL grant strings — must stay byte-identical to docs 07 §4.3.
// M6 SECURITY REFACTOR (07 §10): the replication/snapshot grants
// (+bgsave +sync +psync +replconf) were MOVED off _operator and ONTO _backup so
// the backup Job authenticates as _backup (the snapshot+replication user) and
// _operator is narrowed back to the canonical orchestration-only floor.
// _backup now carries +sync +psync +replconf APPENDED after its canonical +ping.
const (
	wantOperatorBaseRules = "resetchannels resetkeys -@all +cluster +config|get +config|set +info +client|setname +client|setinfo +replicaof +wait +ping"
	wantOperatorRules     = wantOperatorBaseRules
	wantExporterRules     = "resetchannels resetkeys -@all +info +cluster|info +latency +ping"
	wantBackupBaseRules   = "resetchannels resetkeys -@all +bgsave +lastsave +save +info +wait +ping"
	wantBackupRules       = wantBackupBaseRules + " +sync +psync +replconf"
)

func TestSystemUsersVerbatim(t *testing.T) {
	t.Parallel()
	users := SystemUsers(true)
	if len(users) != 3 {
		t.Fatalf("expected 3 system users with exporter enabled, got %d", len(users))
	}
	if users[0].Name != SystemUserOperator || users[0].Rules != wantOperatorRules {
		t.Fatalf("operator rules drifted:\n got: %q\nwant: %q", users[0].Rules, wantOperatorRules)
	}
	if users[1].Name != SystemUserExporter || users[1].Rules != wantExporterRules {
		t.Fatalf("exporter rules drifted:\n got: %q\nwant: %q", users[1].Rules, wantExporterRules)
	}
	if users[2].Name != SystemUserBackup || users[2].Rules != wantBackupRules {
		t.Fatalf("backup rules drifted:\n got: %q\nwant: %q", users[2].Rules, wantBackupRules)
	}
	// Exporter disabled -> only operator + backup.
	off := SystemUsers(false)
	if len(off) != 2 || off[1].Name != SystemUserBackup {
		t.Fatalf("exporter should be omitted when disabled: %+v", off)
	}
}

func TestRenderUsersACLDeterministicAndHashed(t *testing.T) {
	t.Parallel()
	pw := map[string]string{
		SystemUserOperator: "op-pass",
		SystemUserExporter: "ex-pass",
		SystemUserBackup:   "bk-pass",
	}
	// User lines passed out of order must be sorted deterministically.
	out := RenderUsersACL(true, pw, []string{"user zed on +@read", "user abe on +@read"}, "def-pass")

	opHash := sha256.Sum256([]byte("op-pass"))
	wantOpLine := "user _operator on #" + hex.EncodeToString(opHash[:]) + " " + wantOperatorRules
	if !strings.Contains(out, wantOpLine) {
		t.Fatalf("operator line not verbatim/hashed:\n%s\nwant line: %s", out, wantOpLine)
	}
	// The default-user line MUST be present and password-gated so the aclfile and
	// requirepass agree (otherwise the engine leaves default nopass — auth not
	// enforced). It must use the hash, never cleartext.
	defHash := sha256.Sum256([]byte("def-pass"))
	wantDefLine := "user default on #" + hex.EncodeToString(defHash[:]) + " ~* &* +@all"
	if !strings.Contains(out, wantDefLine) {
		t.Fatalf("default user line missing/not hashed:\n%s\nwant line: %s", out, wantDefLine)
	}
	// Cleartext passwords must never appear.
	if strings.Contains(out, "op-pass") || strings.Contains(out, "def-pass") {
		t.Fatalf("cleartext password leaked into ACL:\n%s", out)
	}
	// Deterministic ordering: abe before zed.
	if strings.Index(out, "user abe") > strings.Index(out, "user zed") {
		t.Fatalf("user lines not sorted:\n%s", out)
	}
	// Two renders are byte-identical.
	if RenderUsersACL(true, pw, []string{"user abe on +@read", "user zed on +@read"}, "def-pass") != out {
		t.Fatal("RenderUsersACL is not deterministic")
	}

	// Empty default-user password (auth disabled) => explicit nopass default line,
	// agreeing with the absent requirepass directive.
	noAuth := RenderUsersACL(true, pw, nil, "")
	if !strings.Contains(noAuth, "user default on nopass ~* &* +@all") {
		t.Fatalf("nopass default line missing when auth disabled:\n%s", noAuth)
	}
}

func TestHashACLPasswordFormat(t *testing.T) {
	t.Parallel()
	h := HashACLPassword("x")
	if !strings.HasPrefix(h, "#") || len(h) != 65 { // '#' + 64 hex.
		t.Fatalf("bad hash format: %q", h)
	}
}
