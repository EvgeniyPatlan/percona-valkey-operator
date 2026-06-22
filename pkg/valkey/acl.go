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
	"fmt"
	"slices"
	"strings"
)

// ACLFileKey is the data key under which the rendered users.acl is stored in
// the internal-<cluster>-acl Secret (type valkey.io/acl).
const ACLFileKey = "users.acl"

// SystemUser is one canonical least-privilege system user. The Rules are the
// VERBATIM grant string from 07 §4.3 / 04 §3 (everything after the hashed
// password slot). The renderer inserts `on #<sha256-of-password>` between the
// username and the rules. Order is fixed (operator, exporter, backup) so the
// rendered file is byte-stable across reconciles (04 §3 determinism rule).
type SystemUser struct {
	// Name is the reserved system-user name (_operator/_exporter/_backup).
	Name string
	// Rules is the verbatim ACL grant string after the hashed-password slot.
	Rules string
}

// SystemUsers returns the canonical system-user definitions in fixed order.
// _exporter is included only when exporterEnabled (07 §4.3: skipped when the
// exporter is disabled). The grant strings are byte-identical to the canonical
// least-privilege spec in 07 §4.3 / 04 §3 / 05 §10 — do NOT edit without
// updating those docs (an ACL drift would lock the operator out).
func SystemUsers(exporterEnabled bool) []SystemUser {
	users := []SystemUser{
		{
			Name:  SystemUserOperator,
			Rules: "resetchannels resetkeys -@all +cluster +config|get +config|set +info +client|setname +client|setinfo +replicaof +wait +ping",
		},
	}
	if exporterEnabled {
		users = append(users, SystemUser{
			Name:  SystemUserExporter,
			Rules: "resetchannels resetkeys -@all +info +cluster|info +latency +ping",
		})
	}
	users = append(users, SystemUser{
		Name:  SystemUserBackup,
		Rules: "resetchannels resetkeys -@all +bgsave +lastsave +save +info +wait +ping",
	})
	return users
}

// System user names (the reserved _-prefixed ACL users, mirrored from
// pkg/naming so pkg/valkey need not import it for the ACL render).
const (
	// SystemUserOperator is the cluster-orchestration ACL user.
	SystemUserOperator = "_operator"
	// SystemUserExporter is the read-only metrics-scraper ACL user.
	SystemUserExporter = "_exporter"
	// SystemUserBackup is the server-side snapshot ACL user.
	SystemUserBackup = "_backup"
)

// HashACLPassword returns the bare `#<sha256-hex>` ACL rule for a cleartext
// password (64 lowercase hex chars), per the Valkey 9 ACL grammar (07 §4.3).
func HashACLPassword(password string) string {
	sum := sha256.Sum256([]byte(password))
	return "#" + hex.EncodeToString(sum[:])
}

// renderSystemUserLine renders one canonical system-user line:
//
//	user <name> on #<sha256-of-password> <rules>
//
// matching the verbatim form in 07 §4.3. The password hash is inserted at the
// documented position; cleartext is never written.
func renderSystemUserLine(u SystemUser, password string) string {
	return fmt.Sprintf("user %s on %s %s", u.Name, HashACLPassword(password), u.Rules)
}

// RenderUsersACL renders the deterministic users.acl file content: the canonical
// system users (07 §4.3) in fixed order, each with its password sourced from
// passwords[name], followed by the user-defined lines (already-rendered, sorted)
// passed in userLines. The output is byte-stable (sorted user lines, fixed
// system-user order) so its hash triggers a roll only on a real ACL change
// (04 §3). M3 renders only the system users + a passthrough of spec.users; the
// full user-defined ACL grammar (keys/channels/commands) is M5.
func RenderUsersACL(exporterEnabled bool, passwords map[string]string, userLines []string) string {
	var b strings.Builder
	for _, u := range SystemUsers(exporterEnabled) {
		b.WriteString(renderSystemUserLine(u, passwords[u.Name]))
		b.WriteByte('\n')
	}
	sorted := append([]string(nil), userLines...)
	slices.Sort(sorted)
	for _, line := range sorted {
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
