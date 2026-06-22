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

package backup

import (
	"context"
	"encoding/base64"
	"testing"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// fakeAzureKey is a base64-encoded inert account key (the SDK shared-key
// credential requires valid base64). It authenticates to nothing.
var fakeAzureKey = base64.StdEncoding.EncodeToString([]byte("inert-account-key-bytes"))

func TestNewAzureStoreConstruction(t *testing.T) {
	ctx := context.Background()
	cfg := StorageConfig{
		Type:  valkeyv1alpha1.BackupStorageAzure,
		Azure: &valkeyv1alpha1.BackupStorageAzureSpec{Container: "backups", Prefix: "prod/"},
		Credentials: map[string]string{
			envAzureStorageAccount: "myaccount",
			envAzureStorageKey:     fakeAzureKey,
		},
	}
	store, err := newAzureStore(ctx, cfg)
	if err != nil {
		t.Fatalf("newAzureStore: %v", err)
	}
	a, ok := store.(*azureStore)
	if !ok {
		t.Fatalf("expected *azureStore")
	}
	if a.container != "backups" || a.prefix != "prod/" || a.client == nil {
		t.Fatalf("azureStore fields = %q/%q client=%v", a.container, a.prefix, a.client)
	}
}

func TestNewAzureStoreErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		cfg  StorageConfig
	}{
		{"nil spec", StorageConfig{Type: valkeyv1alpha1.BackupStorageAzure}},
		{"empty container", StorageConfig{Type: valkeyv1alpha1.BackupStorageAzure, Azure: &valkeyv1alpha1.BackupStorageAzureSpec{}}},
		{
			"missing creds",
			StorageConfig{
				Type:  valkeyv1alpha1.BackupStorageAzure,
				Azure: &valkeyv1alpha1.BackupStorageAzureSpec{Container: "c"},
			},
		},
		{
			"non-base64 key",
			StorageConfig{
				Type:  valkeyv1alpha1.BackupStorageAzure,
				Azure: &valkeyv1alpha1.BackupStorageAzureSpec{Container: "c"},
				Credentials: map[string]string{
					envAzureStorageAccount: "acct",
					envAzureStorageKey:     "not-valid-base64!!!",
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newAzureStore(ctx, tc.cfg); err == nil {
				t.Fatalf("newAzureStore(%s) = nil error, want failure", tc.name)
			}
		})
	}
}

func TestAzureStoreFullKeyAndStripPrefix(t *testing.T) {
	a := &azureStore{prefix: "prod/"}
	if got := a.fullKey("c/b/manifest.json"); got != "prod/c/b/manifest.json" {
		t.Fatalf("fullKey = %q", got)
	}
	if got := a.stripPrefix("prod/c/b/manifest.json"); got != "c/b/manifest.json" {
		t.Fatalf("stripPrefix = %q", got)
	}
	bare := &azureStore{}
	if got := bare.stripPrefix("c/b/x"); got != "c/b/x" {
		t.Fatalf("bare stripPrefix = %q", got)
	}
}
