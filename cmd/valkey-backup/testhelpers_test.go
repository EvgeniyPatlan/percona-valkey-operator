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
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// seedFilesystemSetWithSHA seeds a filesystem-backed set whose manifest records
// the correct SHA-256 per shard, so the download verification path passes.
func seedFilesystemSetWithSHA(t *testing.T, root, cluster, name string, payloads map[int][]byte) {
	t.Helper()
	ctx := context.Background()
	store, err := backup.NewStore(ctx, backup.StorageConfig{Type: "filesystem", FilesystemRoot: root})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	man := backup.Manifest{Cluster: cluster, BackupName: name, Mode: "cluster", SlotCoverage: "complete"}
	for idx, data := range payloads {
		if putErr := store.Put(ctx, backup.ShardRDBKey(cluster, name, idx), strings.NewReader(string(data)), int64(len(data))); putErr != nil {
			t.Fatalf("seed shard %d: %v", idx, putErr)
		}
		sum := sha256.Sum256(data)
		man.Shards = append(man.Shards, backup.ShardManifest{
			Index:  idx,
			RDBKey: backup.ShardRDBRelKey(idx),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	if wErr := backup.WriteManifest(ctx, store, backup.ManifestKey(cluster, name), man); wErr != nil {
		t.Fatalf("seed manifest: %v", wErr)
	}
}

// testCAPEM generates a self-signed CA certificate PEM at test time (never a
// hardcoded cert) so the TLS-CA loading path can be exercised hermetically.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "valkey-backup-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
