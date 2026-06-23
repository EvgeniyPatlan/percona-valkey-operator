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

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// ACLFileKey is the data key under which the rendered users.acl is stored in
// the internal-<cluster>-acl Secret (type valkey.io/acl).
const ACLFileKey = "users.acl"

// SystemUserDefault is the built-in default user. Valkey's ACL subsystem OWNS the
// default user: once an aclfile is loaded, the default user's state is whatever
// the aclfile says (or its built-in startup state if the file omits it), and a
// `requirepass` directive in valkey.conf is IGNORED for it. So when the operator
// passwords the default user via requirepass, the users.acl MUST also carry a
// matching `default` line or auth is silently NOT enforced (the default user
// stays `nopass`). See defaultUserLine. 07 §3 / gap §2.3.
const SystemUserDefault = "default"

// defaultUserRules is the default user's grant: full access (~* &* +@all),
// matching the engine's built-in default. The only thing the operator gates is
// the password (on #<hash> vs on nopass); access is never narrowed so existing
// clients keep working once they authenticate.
const defaultUserRules = "~* &* +@all"

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

// operatorRules is the canonical least-privilege _operator grant string
// (everything after the hashed-password slot), byte-identical to the canonical
// 07 §4.3 / 04 §3 specification.
//
// M6 SECURITY REFACTOR (07 §10 trust-boundary narrowing): the replication +
// snapshot grants (+bgsave +sync +psync +replconf) that M5 had APPENDED here so
// the backup Job could SYNC-as-replica while authenticating as _operator have
// been MOVED onto _backup (see backupRules). The backup Job now authenticates as
// _backup, which performs the BGSAVE+SYNC itself, so _operator no longer needs
// any replication/snapshot grant and is narrowed back to the canonical
// orchestration-only floor. This resolves the M5 trust-boundary flag: a
// compromised _operator credential can no longer pull every shard's full dataset
// over the replication protocol. Any change to this string is a CRITICAL
// trust-boundary change (07 §10) requiring security-reviewer.
//
// +acl|load lets the operator apply a live ACL change in place: after rewriting
// the mounted aclfile it issues ACL LOAD so running engines pick up user/grant/
// password changes without a pod restart (the in-place auth reload, acl.go
// liveReloadAuth). It is the aclfile-RELOAD subcommand ONLY — it re-reads the
// operator-controlled aclfile and grants NO ability to mutate ACLs ad hoc
// (acl|setuser/deluser stay denied), so the orchestration-only trust boundary is
// preserved (an _operator credential still cannot read the keyspace or rewrite
// grants, only reload the file the operator already controls).
//
// +acl|list is the READ-ONLY companion to +acl|load: liveReloadAuth reads the
// node's currently-loaded rules back (ACL LIST) to VERIFY the ACL LOAD actually
// picked up the freshly rendered aclfile. Kubernetes Secret-mount projection lags
// the Secret write, so an ACL LOAD issued immediately can re-read STALE file
// content, report success, and silently drop the change; comparing loaded-vs-
// rendered closes that race. It is read-only introspection of the operator's OWN
// rendered ACL — it returns password HASHES (never plaintext), grants NO keyspace
// access and NO ability to mutate ACLs, so the trust boundary is unchanged in
// substance (a security review graded the addition LOW: strictly narrower than
// +acl or +@admin, and the operator already holds every rendered hash). Any
// change to this string remains a CRITICAL trust-boundary change (07 §10).
const operatorRules = "resetchannels resetkeys -@all +cluster +config|get +config|set +info " +
	"+client|setname +client|setinfo +replicaof +wait +ping +acl|load +acl|list"

// backupRules is the canonical least-privilege _backup grant string. _backup is
// the snapshot+replication user: it triggers a server-side RDB snapshot and
// pulls each shard's RDB over the replication protocol.
//
//   - +bgsave +lastsave +save: server-side snapshot control (07 §4.3 canonical).
//   - +info +wait +ping: connection/health + replica-ack await (07 §4.3 canonical).
//   - +cluster|nodes: the backup Job scrapes CLUSTER NODES from a seed node to
//     resolve each shard's LIVE primary (06 §4.3 step 1) before attaching as a
//     replica — without it the Job cannot discover the topology to snapshot. This
//     is the cluster-management subcommand only (read-only topology: node ids,
//     addresses, slot ownership); it grants NO keyspace access, so the trust
//     boundary below (full-keyspace read via replication) is unchanged.
//   - +sync +psync +replconf (M6, APPENDED): the replication grants the backup Job
//     needs to attach as a replica and stream each shard's RDB. The Job uses the
//     legacy SYNC (+sync); +psync/+replconf are the canonical Valkey replica-user
//     grants (valkey-doc topics/acl.md) added so a PSYNC-based path also works.
//
// The replication tokens are APPENDED after the canonical +ping so the original
// 07 §4.3 substring stays contiguous. Like operatorRules, any change here is a
// CRITICAL trust-boundary change (07 §10) requiring security-reviewer: _backup
// is now the only system user that can read the full keyspace via replication.
const backupRules = "resetchannels resetkeys -@all +bgsave +lastsave +save +info +wait +ping " +
	"+cluster|nodes +sync +psync +replconf"

// SystemUsers returns the canonical system-user definitions in fixed order.
// _exporter is included only when exporterEnabled (07 §4.3: skipped when the
// exporter is disabled). The grant strings are byte-identical to the canonical
// least-privilege spec in 07 §4.3 / 04 §3 / 05 §10 (with the M5 _operator backup
// extension, see operatorRules) — do NOT edit without updating those docs (an
// ACL drift would lock the operator out).
func SystemUsers(exporterEnabled bool) []SystemUser {
	users := []SystemUser{
		{
			Name:  SystemUserOperator,
			Rules: operatorRules,
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
		Rules: backupRules,
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

// RenderUsersACL renders the deterministic users.acl file content: the built-in
// `default` user line first (defaultUserLine), then the canonical system users
// (07 §4.3) in fixed order, each with its password sourced from passwords[name],
// followed by the user-defined lines (already-rendered by BuildUserACL, sorted
// here) passed in userLines. The output is byte-stable (sorted user lines, fixed
// system-user order) so its hash triggers a roll only on a real ACL change (04
// §3). System users render LAST in the file is NOT the order: 07 §4.2 fixes
// user-defined lines first, then system users; this helper emits system users
// first because the engine resolves the last matching `user` line and the system
// users must never be shadowed by a same-named user-defined line — but
// user-defined names cannot start with `_` (CEL), so there is no collision and
// either order is byte-stable. Kept as-is from M3 for stability.
//
// defaultUserPassword is the cleartext default-user password the controller
// resolved from spec.auth (same value rendered as `requirepass` in valkey.conf).
// It is REQUIRED that the aclfile carry a matching `default` line: once an aclfile
// is loaded the engine ignores `requirepass` for the default user, so without this
// line a passworded cluster would silently leave the default user `nopass` (auth
// not enforced — security defect surfaced on percona/valkey:9.1.0 via the
// cross-shard MOVED re-AUTH path). Empty => `user default on nopass ...` (auth
// disabled / password unresolved), making the file agree with the absent
// requirepass line.
func RenderUsersACL(exporterEnabled bool, passwords map[string]string, userLines []string, defaultUserPassword string) string {
	var b strings.Builder
	b.WriteString(defaultUserLine(defaultUserPassword))
	b.WriteByte('\n')
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

// defaultUserLine renders the built-in `default` user line so the aclfile and the
// valkey.conf `requirepass` directive AGREE on the default user's auth state.
// Non-empty password => `user default on #<sha256-hex> ~* &* +@all` (password
// required). Empty password => `user default on nopass ~* &* +@all` (auth
// disabled). Access is always full (~* &* +@all), matching the engine default;
// only the password slot is gated. Cleartext is never written — only the hash.
func defaultUserLine(password string) string {
	if password == "" {
		return fmt.Sprintf("user %s on nopass %s", SystemUserDefault, defaultUserRules)
	}
	return fmt.Sprintf("user %s on %s %s", SystemUserDefault, HashACLPassword(password), defaultUserRules)
}

// BuildUserACL renders exactly one deterministic `user <name> ...` line from a
// UserACLSpec, mapping each field to its ACL token in the FIXED order from
// 07 §4.1: name, on/off, password(s), keys (~/%R~/%W~), channels (resetchannels
// then &), +allow, -deny, raw permissions.
//
// pwHashes are the already-`#`-prefixed hashed passwords (each "#<sha256-hex>")
// in the caller's deterministic order — one per password key for multi-password
// rotation (07 §4.4). resetpass strips every password (emits neither nopass nor
// #hashes); nopass makes the user passwordless. resetpass takes precedence over
// nopass and over pwHashes (an emergency lock-out).
//
// The output is byte-stable for an identical spec so the rendered users.acl hash
// only churns on a real ACL change (04 §3). Cleartext passwords never appear —
// only their hashes, which the caller computes via HashACLPassword.
func BuildUserACL(u valkeyv1alpha1.UserACLSpec, pwHashes []string) string {
	var b strings.Builder
	b.WriteString("user ")
	b.WriteString(u.Name)
	if u.Enabled {
		b.WriteString(" on")
	} else {
		b.WriteString(" off")
	}

	// Password slot. resetpass wins (strips all passwords); else nopass; else the
	// hashed password(s) for rotation. pwHashes are already "#<sha256-hex>".
	switch {
	case u.Resetpass:
		b.WriteString(" resetpass")
	case u.Nopass:
		b.WriteString(" nopass")
	default:
		for _, h := range pwHashes {
			b.WriteByte(' ')
			b.WriteString(h)
		}
	}

	// Keys: ~pattern (read-write), %R~pattern (read-only), %W~pattern (write-only).
	if u.Keys != nil {
		for _, k := range u.Keys.ReadWrite {
			b.WriteString(" ~")
			b.WriteString(k)
		}
		for _, k := range u.Keys.ReadOnly {
			b.WriteString(" %R~")
			b.WriteString(k)
		}
		for _, k := range u.Keys.WriteOnly {
			b.WriteString(" %W~")
			b.WriteString(k)
		}
	}

	// Channels: resetchannels emitted exactly once (drops default access), then
	// &pattern per entry (07 §4.1).
	if u.Channels != nil && len(u.Channels.Patterns) > 0 {
		b.WriteString(" resetchannels")
		for _, c := range u.Channels.Patterns {
			b.WriteString(" &")
			b.WriteString(c)
		}
	}

	// Commands: +allow then -deny, each token verbatim (the spec carries bare
	// tokens; the renderer prepends the +/- sign).
	if u.Commands != nil {
		for _, c := range u.Commands.Allow {
			b.WriteString(" +")
			b.WriteString(c)
		}
		for _, c := range u.Commands.Deny {
			b.WriteString(" -")
			b.WriteString(c)
		}
	}

	// Raw permissions escape hatch, appended verbatim (07 §4.1).
	if u.Permissions != "" {
		b.WriteByte(' ')
		b.WriteString(u.Permissions)
	}

	return b.String()
}
