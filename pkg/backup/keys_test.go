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

package backup_test

import (
	"testing"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

func TestKeyHelpers(t *testing.T) {
	const cluster, name = "prod", "prod-20260622-020000"
	tests := []struct {
		desc string
		got  string
		want string
	}{
		{"set prefix", backup.SetPrefix(cluster, name), "prod/prod-20260622-020000"},
		{"manifest key", backup.ManifestKey(cluster, name), "prod/prod-20260622-020000/manifest.json"},
		{"shard prefix", backup.ShardPrefix(cluster, name, 1), "prod/prod-20260622-020000/shard-1"},
		{"shard rdb key", backup.ShardRDBKey(cluster, name, 2), "prod/prod-20260622-020000/shard-2/dump.rdb"},
		{"shard rel key", backup.ShardRDBRelKey(0), "shard-0/dump.rdb"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q want %q", tc.got, tc.want)
			}
		})
	}
}

func TestManifestKeyIsUnderSetPrefix(t *testing.T) {
	// The manifest and every shard RDB must share the set prefix so a single
	// recursive Delete reclaims the whole set (06 §6.1).
	prefix := backup.SetPrefix("c", "b")
	mk := backup.ManifestKey("c", "b")
	rk := backup.ShardRDBKey("c", "b", 0)
	if len(mk) <= len(prefix) || mk[:len(prefix)] != prefix {
		t.Errorf("manifest key %q not under set prefix %q", mk, prefix)
	}
	if len(rk) <= len(prefix) || rk[:len(prefix)] != prefix {
		t.Errorf("shard rdb key %q not under set prefix %q", rk, prefix)
	}
}
