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

// configYes is the canonical "yes" config value.
const configYes = "yes"

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
	if in.TLS {
		cfg["tls-port"] = clientPortStr
		cfg["port"] = tlsDisabledPortZero
		cfg["tls-cluster"] = configYes
		cfg["tls-replication"] = configYes
		cfg["tls-cert-file"] = tlsCertMountPath + "/" + tlsSecretFileCert
		cfg["tls-key-file"] = tlsCertMountPath + "/" + tlsSecretFileKey
		cfg["tls-ca-cert-file"] = tlsCertMountPath + "/" + tlsSecretFileCA
		cfg["tls-auth-clients"] = "optional"
	}
	return cfg
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
// in sorted key order for byte-stability (no phantom rolls, 04 §11).
func RenderServerConfig(in ConfigInput) string {
	return renderConfigMap(mergedConfig(in))
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

// ServerConfigRollHash is the SHA-256 (hex) over the rendered config EXCLUDING
// the live-settable keys, with sorted key serialization (04 §11). It is computed
// from spec (the ConfigInput), never read back from the live ConfigMap, so the
// stamped ValkeyNode.spec.serverConfigHash can never silently lag desired
// config. Changing only a live-settable key leaves the hash unchanged (no roll).
func ServerConfigRollHash(in ConfigInput) string {
	merged := mergedConfig(in)
	for _, k := range liveSettableKeys {
		delete(merged, k)
	}
	sum := sha256.Sum256([]byte(renderConfigMap(merged)))
	return hex.EncodeToString(sum[:])
}
