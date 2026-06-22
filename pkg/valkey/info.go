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

import "strings"

// Engine replication-role tokens as reported by `INFO replication` (05 §10).
const (
	// InfoRoleMaster is the engine's token for a primary node.
	InfoRoleMaster = "master"
	// InfoRoleSlave is the engine's token for a replica node.
	InfoRoleSlave = "slave"
	// InfoKeyRole is the INFO replication field carrying the role.
	InfoKeyRole = "role"
	// InfoKeyMasterLinkStatus is the replica→primary link state field.
	InfoKeyMasterLinkStatus = "master_link_status"
)

// ParseInfoReplication parses the textual `INFO replication` reply into a map.
// Each non-empty, non-comment line of the form "key:value" becomes one entry;
// the leading "# Replication" section header and blank lines are skipped. Carriage
// returns (the engine emits CRLF) are trimmed. Malformed lines without a colon
// are ignored rather than erroring, so a best-effort role read never fails on a
// stray line (05 §10).
func ParseInfoReplication(info string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}
