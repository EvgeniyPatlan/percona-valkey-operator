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
	"maps"
	"slices"
	"strings"
)

// ConfigFileKey is the data key under which the rendered valkey.conf is stored
// in the cluster ConfigMap.
const ConfigFileKey = "valkey.conf"

// Operator-managed config paths (mount points the operator owns, 04 §2.1 step4).
const (
	aclFilePath       = "/config/users/users.acl"
	dataDir           = "/data"
	clusterConfigFile = "/data/nodes.conf"
	// tlsCertMountPath is the read-only TLS cert mount point. FROZEN M5 contract
	// (= naming.TLSMountPath): the rendered tls-cert-file/tls-key-file/
	// tls-ca-cert-file directives and the cert VolumeMount in the node resources
	// builder share this single path so the mounted files and rendered paths agree
	// (07 §3.1). pkg/valkey is a leaf and does not import pkg/naming, so the literal
	// is duplicated here with this contract note rather than referenced.
	tlsCertMountPath    = "/etc/valkey/tls"
	tlsSecretFileCA     = "ca.crt"
	tlsSecretFileCert   = "tls.crt"
	tlsSecretFileKey    = "tls.key"
	clientPortStr       = "6379"
	tlsDisabledPortZero = "0"
)

// configYes / configNo / configOptional are the canonical tls-auth-clients
// values mapped from the spec.tls.authClients enum (07 §3.2):
//
//	off      -> tls-auth-clients no
//	optional -> tls-auth-clients optional (default)
//	require  -> tls-auth-clients yes
const (
	configYes      = "yes"
	configNo       = "no"
	configOptional = "optional"
)

// authClientsOff / authClientsOptional / authClientsRequire are the spec.tls.
// authClients enum INPUT values (mirrored from the API TLSAuthClients consts;
// pkg/valkey is a leaf and cannot import the API package, so they are duplicated
// here). tlsAuthClientsValue maps them to the engine directive values above.
const (
	authClientsOff      = "off"
	authClientsOptional = "optional"
	authClientsRequire  = "require"
)

// keyMaxmemory is the maxmemory live-settable config key.
const keyMaxmemory = "maxmemory"

// liveSettableKeys are the user-config keys applied hot via CONFIG SET (05 §11)
// and therefore EXCLUDED from the config-roll hash so changing only these never
// rolls a pod (04 §11). The Node controller's live-config allowlist mirrors this.
var liveSettableKeys = []string{keyMaxmemory, "maxmemory-policy", "maxclients"}

// ConfigInput is the minimal slice of cluster spec the config renderer needs,
// so pkg/valkey stays decoupled from the API types' exact shape (the controller
// adapts spec -> ConfigInput). UserConfig is spec.config (verbatim); the three
// flags gate the operator-managed base directives (04 §2.1 step4 / 05 §2).
type ConfigInput struct {
	// UserConfig is spec.config — user-supplied directives applied first so the
	// user wins where allowed; the operator base overrides them where it must.
	UserConfig map[string]string
	// Persistence is true when spec.persistence is set (adds dir/cluster-config-file).
	Persistence bool
	// TLS is true when spec.tls is set (adds the tls-* directive block).
	TLS bool
	// ACL is true when an ACL file is mounted (adds aclfile).
	ACL bool

	// Requirepass is the cleartext default-user password (spec.auth) the controller
	// resolved from the auth Secret. When non-empty the renderer emits a
	// `requirepass <password>` directive (operator-managed, override-proof) so the
	// built-in default user requires it. Empty => no requirepass line (auth disabled
	// or password not yet resolved). It is rendered into valkey.conf but EXCLUDED
	// from the config-roll hash: a default-user password rotation is applied live
	// (CONFIG SET requirepass) so it must never roll the pods (07 §3, ADR-008).
	Requirepass string

	// DisableCommands lists dangerous commands to neutralize via
	// `rename-command <CMD> ""` (spec.disableCommands; defaults to FLUSHALL/FLUSHDB
	// upstream of the renderer). These are operator-managed and MUST win over user
	// config, so they are emitted as dedicated rename-command lines after the
	// key/value config and cannot be re-enabled through spec.config.
	DisableCommands []string

	// TLSAuthClients is the spec.tls.authClients enum value (off|optional|require);
	// empty defaults to optional. Maps to tls-auth-clients no|optional|yes.
	TLSAuthClients string
	// TLSCiphers restricts the TLSv1.2-and-below cipher list (tls-ciphers). Empty =>
	// engine default (directive omitted).
	TLSCiphers string
	// TLSCipherSuites restricts the TLSv1.3 cipher suites (tls-ciphersuites). Empty
	// => engine default (directive omitted).
	TLSCipherSuites string
	// TLSDHParamsFile is the absolute path of the mounted Diffie-Hellman params file
	// wired to tls-dh-params-file. Empty => engine default (directive omitted).
	TLSDHParamsFile string
}

// baseConfig returns the operator-managed directives written LAST so they are
// override-proof (04 §2.1 step4). cluster-node-timeout is deliberately NOT here:
// it is user-tunable via spec.config and defaults to the engine default
// (15000ms) so a too-aggressive value cannot trigger spurious failovers (04
// §2.1 step4 note).
func baseConfig(in ConfigInput) map[string]string {
	cfg := map[string]string{
		"cluster-enabled":                 configYes,
		"protected-mode":                  "no",
		"dir":                             dataDir,
		"cluster-config-file":             clusterConfigFile,
		"cluster-require-full-coverage":   configYes,
		"cluster-allow-replica-migration": "no",
		"cluster-replica-validity-factor": "0",
	}
	if in.ACL {
		cfg["aclfile"] = aclFilePath
	}
	// requirepass sets the built-in default user's password. It is operator-managed
	// (rendered in the base block) so a user cannot blank it via spec.config. The
	// default user is otherwise governed by the mounted aclfile; requirepass is the
	// chart's primary auth knob and is the canonical way to password the default
	// user (07 §3 / gap §2.3). Empty => no line (auth disabled / password unresolved).
	//
	// masterauth is the COMPANION directive: when the default user requires a
	// password, a replica's link to its primary authenticates as the default user
	// (no masteruser is set, so the replication client uses default), so without
	// masterauth the replica gets NOAUTH and master_link_status stays `down` —
	// every shard then fails the operator's replica-link health check and the
	// cluster never reaches Ready. So masterauth MUST mirror requirepass. Like
	// requirepass it is operator-managed and EXCLUDED from the config-roll hash
	// (rotated live alongside requirepass; see ServerConfigRollHash).
	if in.Requirepass != "" {
		cfg["requirepass"] = in.Requirepass
		cfg["masterauth"] = in.Requirepass
	}
	if in.TLS {
		cfg["tls-port"] = clientPortStr
		cfg["port"] = tlsDisabledPortZero
		cfg["tls-cluster"] = configYes
		cfg["tls-replication"] = configYes
		cfg["tls-cert-file"] = tlsCertMountPath + "/" + tlsSecretFileCert
		cfg["tls-key-file"] = tlsCertMountPath + "/" + tlsSecretFileKey
		cfg["tls-ca-cert-file"] = tlsCertMountPath + "/" + tlsSecretFileCA
		cfg["tls-auth-clients"] = tlsAuthClientsValue(in.TLSAuthClients)
		if in.TLSCiphers != "" {
			cfg["tls-ciphers"] = in.TLSCiphers
		}
		if in.TLSCipherSuites != "" {
			cfg["tls-ciphersuites"] = in.TLSCipherSuites
		}
		if in.TLSDHParamsFile != "" {
			cfg["tls-dh-params-file"] = in.TLSDHParamsFile
		}
	}
	return cfg
}

// tlsAuthClientsValue maps the spec.tls.authClients enum (off|optional|require)
// to the Valkey tls-auth-clients directive value (no|optional|yes). An empty or
// unrecognized value defaults to optional (the operator/upstream default, 07
// §3.2) so a malformed value can never silently weaken to "no".
func tlsAuthClientsValue(authClients string) string {
	switch authClients {
	case authClientsOff:
		return configNo
	case authClientsRequire:
		return configYes
	default: // authClientsOptional and any unexpected value.
		return configOptional
	}
}

// mergedConfig layers user config first, operator base last (base wins on
// conflict). The returned map is fresh; inputs are not mutated.
func mergedConfig(in ConfigInput) map[string]string {
	merged := map[string]string{}
	maps.Copy(merged, in.UserConfig)
	maps.Copy(merged, baseConfig(in)) // base overrides user where keys collide.
	return merged
}

// RenderServerConfig renders the full valkey.conf: user directives first (so the
// user wins where allowed) then the operator base (override-proof), serialized
// in sorted key order for byte-stability (no phantom rolls, 04 §11). The
// dangerous-command rename-command lines are appended LAST so they always win
// over any user attempt to re-enable a disabled command via spec.config.
func RenderServerConfig(in ConfigInput) string {
	return renderConfigMap(mergedConfig(in)) + renderDisableCommands(in.DisableCommands)
}

// renderConfigMap serializes a config map as sorted "key value\n" lines.
func renderConfigMap(cfg map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		b.WriteString(k)
		b.WriteByte(' ')
		b.WriteString(cfg[k])
		b.WriteByte('\n')
	}
	return b.String()
}

// renderDisableCommands emits one `rename-command <CMD> ""` line per entry, in
// sorted order for byte-stability. Renaming a command to the empty string is the
// canonical Valkey idiom for disabling it. These lines are emitted AFTER the
// key/value config so they are override-proof (a user cannot re-enable a disabled
// command via spec.config). Empty/blank entries are skipped. Returns "" when the
// list is empty so the M1 minimal cluster (no disabled commands) is unaffected.
func renderDisableCommands(commands []string) string {
	if len(commands) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(commands))
	for _, c := range commands {
		if strings.TrimSpace(c) == "" {
			continue
		}
		sorted = append(sorted, c)
	}
	slices.Sort(sorted)
	var b strings.Builder
	for _, c := range sorted {
		b.WriteString("rename-command ")
		b.WriteString(c)
		b.WriteString(` ""`)
		b.WriteByte('\n')
	}
	return b.String()
}

// ServerConfigRollHash is the SHA-256 (hex) over the rendered config EXCLUDING
// the live-settable keys, with sorted key serialization (04 §11). It is computed
// from spec (the ConfigInput), never read back from the live ConfigMap, so the
// stamped ValkeyNode.spec.serverConfigHash can never silently lag desired
// config. Changing only a live-settable key leaves the hash unchanged (no roll).
//
// requirepass is excluded too: the default-user password is rotated live via
// CONFIG SET requirepass (07 §3, ADR-008), so a password-only change must never
// roll the pods. The rename-command (disableCommands) lines ARE hashed — adding
// or removing a disabled command is a real, roll-worthy config change.
func ServerConfigRollHash(in ConfigInput) string {
	merged := mergedConfig(in)
	for _, k := range liveSettableKeys {
		delete(merged, k)
	}
	delete(merged, "requirepass")
	// masterauth mirrors requirepass and is rotated live alongside it (07 §3,
	// ADR-008); excluded from the roll hash so a password rotation never rolls pods.
	delete(merged, "masterauth")
	body := renderConfigMap(merged) + renderDisableCommands(in.DisableCommands)
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
