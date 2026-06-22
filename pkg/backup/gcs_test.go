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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// fakeSAJSON builds a syntactically valid, INERT service-account JSON with a
// freshly generated RSA key (never a hardcoded secret) so the GCS client
// constructor — which parses and validates the private key — succeeds without
// authenticating to anything real.
func fakeSAJSON(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa := map[string]string{
		"type":         "service_account",
		"project_id":   "p",
		"private_key":  string(pemKey),
		"client_email": "x@p.iam.gserviceaccount.com",
		"token_uri":    "https://oauth2.googleapis.com/token",
	}
	data, err := json.Marshal(sa)
	if err != nil {
		t.Fatalf("marshal sa: %v", err)
	}
	return string(data)
}

func TestNewGCSStoreConstructionInlineJSON(t *testing.T) {
	ctx := context.Background()
	cfg := StorageConfig{
		Type: valkeyv1alpha1.BackupStorageGCS,
		GCS:  &valkeyv1alpha1.BackupStorageGCSSpec{Bucket: "valkey-backups", Prefix: "prod/"},
		Credentials: map[string]string{
			envGoogleAppCredsJSON: fakeSAJSON(t),
		},
	}
	store, err := newGCSStore(ctx, cfg)
	if err != nil {
		t.Fatalf("newGCSStore(inline JSON): %v", err)
	}
	g, ok := store.(*gcsStore)
	if !ok {
		t.Fatalf("expected *gcsStore")
	}
	if g.bucket != "valkey-backups" || g.prefix != "prod/" || g.client == nil {
		t.Fatalf("gcsStore fields = %q/%q client=%v", g.bucket, g.prefix, g.client)
	}
}

func TestNewGCSStoreConstructionCredsFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(path, []byte(fakeSAJSON(t)), 0o600); err != nil {
		t.Fatalf("write sa.json: %v", err)
	}
	cfg := StorageConfig{
		Type:        valkeyv1alpha1.BackupStorageGCS,
		GCS:         &valkeyv1alpha1.BackupStorageGCSSpec{Bucket: "b"},
		Credentials: map[string]string{envGoogleAppCreds: path},
	}
	if _, err := newGCSStore(ctx, cfg); err != nil {
		t.Fatalf("newGCSStore(creds file): %v", err)
	}
}

func TestNewGCSStoreErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		cfg  StorageConfig
	}{
		{"nil spec", StorageConfig{Type: valkeyv1alpha1.BackupStorageGCS}},
		{"empty bucket", StorageConfig{Type: valkeyv1alpha1.BackupStorageGCS, GCS: &valkeyv1alpha1.BackupStorageGCSSpec{}}},
		{
			"unreadable creds file",
			StorageConfig{
				Type:        valkeyv1alpha1.BackupStorageGCS,
				GCS:         &valkeyv1alpha1.BackupStorageGCSSpec{Bucket: "b"},
				Credentials: map[string]string{envGoogleAppCreds: "/nonexistent/sa.json"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newGCSStore(ctx, tc.cfg); err == nil {
				t.Fatalf("newGCSStore(%s) = nil error, want failure", tc.name)
			}
		})
	}
}

func TestGCSClientOptions(t *testing.T) {
	// Empty creds -> no explicit option (defer to ADC).
	opts, err := gcsClientOptions(nil)
	if err != nil {
		t.Fatalf("gcsClientOptions(nil): %v", err)
	}
	if len(opts) != 0 {
		t.Fatalf("gcsClientOptions(nil) = %d opts, want 0", len(opts))
	}
	// Inline JSON -> one option.
	opts, err = gcsClientOptions(map[string]string{envGoogleAppCredsJSON: fakeSAJSON(t)})
	if err != nil {
		t.Fatalf("gcsClientOptions(json): %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("gcsClientOptions(json) = %d opts, want 1", len(opts))
	}
	// Missing file path -> error.
	if _, err = gcsClientOptions(map[string]string{envGoogleAppCreds: "/nope.json"}); err == nil {
		t.Fatalf("gcsClientOptions(missing file) = nil error, want failure")
	}
}

func TestGCSStoreFullKeyAndStripPrefix(t *testing.T) {
	g := &gcsStore{prefix: "prod/"}
	if got := g.fullKey("c/b/manifest.json"); got != "prod/c/b/manifest.json" {
		t.Fatalf("fullKey = %q", got)
	}
	if got := g.stripPrefix("prod/c/b/manifest.json"); got != "c/b/manifest.json" {
		t.Fatalf("stripPrefix = %q", got)
	}
	bare := &gcsStore{}
	if got := bare.stripPrefix("c/b/x"); got != "c/b/x" {
		t.Fatalf("bare stripPrefix = %q", got)
	}
}
