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
	"reflect"
	"strings"
	"testing"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

func sampleManifest() backup.Manifest {
	return backup.Manifest{
		SchemaVersion: backup.ManifestSchemaVersion,
		APIVersion:    backup.ManifestAPIVersion,
		Cluster:       "prod",
		BackupName:    "prod-20260622-020000",
		Mode:          "cluster",
		CRVersion:     "1.0",
		EngineVersion: "9.0.0",
		CreatedAt:     "2026-06-22T02:00:31Z",
		Consistency:   "strict",
		SlotCoverage:  "complete",
		Shards: []backup.ShardManifest{
			{Index: 0, PrimaryNodeID: "n0", SlotRanges: "0-5460", RDBKey: "shard-0/dump.rdb", SizeBytes: 734003200, SHA256: "aa", MasterReplOffset: 91823771},
			{Index: 1, PrimaryNodeID: "n1", SlotRanges: "5461-10922", RDBKey: "shard-1/dump.rdb", SizeBytes: 12, SHA256: "bb"},
			{Index: 2, PrimaryNodeID: "n2", SlotRanges: "10923-16383", RDBKey: "shard-2/dump.rdb", SizeBytes: 34, SHA256: "cc"},
		},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	in := sampleManifest()
	data, err := backup.MarshalManifest(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := backup.UnmarshalManifest(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestMarshalManifestStampsDefaults(t *testing.T) {
	in := sampleManifest()
	in.SchemaVersion = 0
	in.APIVersion = ""
	data, err := backup.MarshalManifest(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := backup.UnmarshalManifest(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.SchemaVersion != backup.ManifestSchemaVersion {
		t.Errorf("schemaVersion not stamped: got %d", out.SchemaVersion)
	}
	if out.APIVersion != backup.ManifestAPIVersion {
		t.Errorf("apiVersion not stamped: got %q", out.APIVersion)
	}
}

func TestManifestJSONFieldNames(t *testing.T) {
	data, err := backup.MarshalManifest(sampleManifest())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)
	for _, want := range []string{
		`"schemaVersion"`, `"clusterName"`, `"backupName"`, `"engineVersion"`,
		`"createdAt"`, `"slotCoverage"`, `"shards"`, `"index"`, `"slotRanges"`,
		`"rdbKey"`, `"sizeBytes"`, `"sha256"`, `"primaryNodeID"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("manifest JSON missing field %s\n%s", want, js)
		}
	}
}

func TestUnmarshalManifestRejectsNewerSchema(t *testing.T) {
	in := sampleManifest()
	in.SchemaVersion = backup.ManifestSchemaVersion + 1
	data, err := backup.MarshalManifest(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := backup.UnmarshalManifest(data); err == nil {
		t.Fatal("expected error on newer schemaVersion, got nil")
	}
}

func TestUnmarshalManifestRejectsGarbage(t *testing.T) {
	if _, err := backup.UnmarshalManifest([]byte("not json")); err == nil {
		t.Fatal("expected error on garbage, got nil")
	}
}
