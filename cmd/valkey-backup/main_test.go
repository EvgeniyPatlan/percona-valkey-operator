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

package main

import (
	"testing"

	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

func TestParseFlags(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantErr  bool
		download bool
		cleanup  bool
		shard    int
	}{
		{name: "default backup", args: nil},
		{name: "cleanup", args: []string{"--cleanup"}, cleanup: true, shard: -1},
		{name: "download with shard", args: []string{"--download", "--shard=2"}, download: true, shard: 2},
		{name: "download without shard", args: []string{"--download"}, wantErr: true},
		{name: "bad flag", args: []string{"--nope"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseFlags(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseFlags(%v) err = %v, wantErr %v", tc.args, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if f.download != tc.download || f.cleanup != tc.cleanup {
				t.Fatalf("parseFlags(%v) = %+v", tc.args, f)
			}
		})
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("VALKEY_BACKUP_TEST_KEY", "")
	if got := envOrDefault("VALKEY_BACKUP_TEST_KEY", "fallback"); got != "fallback" {
		t.Fatalf("envOrDefault(empty) = %q, want fallback", got)
	}
	t.Setenv("VALKEY_BACKUP_TEST_KEY", "set")
	if got := envOrDefault("VALKEY_BACKUP_TEST_KEY", "fallback"); got != "set" {
		t.Fatalf("envOrDefault(set) = %q, want set", got)
	}
}

func TestConnSecurityFromEnvDefaults(t *testing.T) {
	t.Setenv(envTLSEnabled, "")
	t.Setenv(envAuthUser, "")
	t.Setenv(envAuthPassword, "pw")
	auth, tlsConfig, err := connSecurityFromEnv()
	if err != nil {
		t.Fatalf("connSecurityFromEnv: %v", err)
	}
	if auth.username != naming.SystemUserOperator {
		t.Fatalf("auth.username = %q, want %q", auth.username, naming.SystemUserOperator)
	}
	if auth.password != "pw" {
		t.Fatalf("auth.password not carried")
	}
	if tlsConfig != nil {
		t.Fatalf("tlsConfig = %v, want nil when TLS disabled", tlsConfig)
	}
}

func TestConnSecurityFromEnvTLSMissingCA(t *testing.T) {
	t.Setenv(envTLSEnabled, "true")
	t.Setenv(envTLSCAFile, "/nonexistent/ca.crt")
	if _, _, err := connSecurityFromEnv(); err == nil {
		t.Fatalf("connSecurityFromEnv(missing CA) = nil error, want failure")
	}
}

func TestRunUnknownStorageFails(t *testing.T) {
	t.Setenv(envStorageType, "")
	// Default (backup) mode with no storage configured must fail loudly, not panic.
	if err := run([]string{}); err == nil {
		t.Fatalf("run(no storage) = nil error, want failure")
	}
}
