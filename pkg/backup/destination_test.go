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

func TestParseDestination(t *testing.T) {
	tests := []struct {
		in         string
		wantErr    bool
		wantScheme string
		wantBucket string
		wantPath   string
	}{
		{in: "s3://bkt/prod/prod-1", wantScheme: backup.SchemeS3, wantBucket: "bkt", wantPath: "prod/prod-1"},
		{in: "gs://gbkt/prefix/c/b", wantScheme: backup.SchemeGCS, wantBucket: "gbkt", wantPath: "prefix/c/b"},
		{in: "azure://cont/c/b", wantScheme: backup.SchemeAzure, wantBucket: "cont", wantPath: "c/b"},
		{in: "pvc/data/c/b", wantScheme: backup.SchemePVC, wantBucket: "", wantPath: "data/c/b"},
		{in: "s3://bkt", wantScheme: backup.SchemeS3, wantBucket: "bkt", wantPath: ""},
		{in: "ftp://nope", wantErr: true},
		{in: "", wantErr: true},
		{in: "s3://", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			d, err := backup.ParseDestination(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Scheme != tc.wantScheme || d.Bucket != tc.wantBucket || d.Path != tc.wantPath {
				t.Errorf("got %+v want scheme=%q bucket=%q path=%q", d, tc.wantScheme, tc.wantBucket, tc.wantPath)
			}
		})
	}
}

func TestDestinationRoundTrip(t *testing.T) {
	for _, in := range []string{
		"s3://bkt/prod/prod-1",
		"gs://gbkt/prefix/c/b",
		"azure://cont/c/b",
		"pvc/data/c/b",
	} {
		d, err := backup.ParseDestination(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got := d.String(); got != in {
			t.Errorf("round-trip %q -> %q", in, got)
		}
	}
}

func TestFormatDestination(t *testing.T) {
	tests := []struct {
		desc   string
		scheme string
		bucket string
		prefix string
		want   string
	}{
		{"s3 with prefix", backup.SchemeS3, "bkt", "team/", "s3://bkt/team/prod/prod-1"},
		{"s3 no prefix", backup.SchemeS3, "bkt", "", "s3://bkt/prod/prod-1"},
		{"gcs", backup.SchemeGCS, "gbkt", "p", "gs://gbkt/p/prod/prod-1"},
		{"azure", backup.SchemeAzure, "cont", "", "azure://cont/prod/prod-1"},
		{"pvc", backup.SchemePVC, "data", "", "pvc/data/prod/prod-1"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := backup.FormatDestination(tc.scheme, tc.bucket, tc.prefix, "prod", "prod-1")
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
			// Every formatted destination must parse back cleanly.
			if _, err := backup.ParseDestination(got); err != nil {
				t.Errorf("formatted destination %q does not parse: %v", got, err)
			}
		})
	}
}
