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
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// startFakeS3 spins up a TLS httptest server speaking the minimal S3 verbs and
// installs the test-only HTTP client (trusting the server's self-signed cert) so
// the real AWS SDK signs/streams against it over TLS — the same transport shape
// as production S3/MinIO (the SDK's unseekable-stream path needs TLS). Production
// leaves s3TestHTTPClient nil.
func startFakeS3(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(newFakeS3Server())
	prev := s3TestHTTPClient
	s3TestHTTPClient = &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	t.Cleanup(func() {
		s3TestHTTPClient = prev
		srv.Close()
	})
	return srv
}

// startFakeS3Failing is like startFakeS3 but the server returns HTTP 500 for
// every request, so the error-surfacing paths (a real backend failure that is
// NOT a not-found) can be asserted.
func startFakeS3Failing(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>InternalError</Code></Error>`)
	}))
	prev := s3TestHTTPClient
	s3TestHTTPClient = &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	t.Cleanup(func() {
		s3TestHTTPClient = prev
		srv.Close()
	})
	return srv
}

// fakeS3Server is a minimal in-memory, path-style S3 endpoint: enough of
// PutObject / GetObject / HeadObject / DeleteObject / ListObjectsV2 for the real
// AWS SDK client to round-trip against, so s3Store's CRUD methods are exercised
// hermetically (the live AWS path is otherwise e2e-only). It is NOT a faithful
// S3 — just the verbs and the XML shapes the SDK parses.
type fakeS3Server struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3Server() *fakeS3Server { return &fakeS3Server{objects: map[string][]byte{}} }

func (f *fakeS3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Path-style: /<bucket>/<key...>; ListObjectsV2 is GET /<bucket>?list-type=2.
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	_ = bucket

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			f.writeListXML(w, r.URL.Query().Get("prefix"))
			return
		}
		data, ok := f.objects[key]
		if !ok {
			f.writeNotFound(w)
			return
		}
		_, _ = w.Write(data)
	case http.MethodHead:
		if _, ok := f.objects[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3Server) writeNotFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>not found</Message></Error>`)
}

func (f *fakeS3Server) writeListXML(w http.ResponseWriter, prefix string) {
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><ListBucketResult>`)
	for _, k := range keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`, k, len(f.objects[k]))
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}

// newS3StoreAgainst builds an s3Store pointed at the fake server (path-style).
func newS3StoreAgainst(t *testing.T, endpoint, prefix string) ArtifactStore {
	t.Helper()
	store, err := newS3Store(context.Background(), StorageConfig{
		Type: valkeyv1alpha1.BackupStorageS3,
		S3: &valkeyv1alpha1.BackupStorageS3Spec{
			Bucket:      "test-bucket",
			Prefix:      prefix,
			Region:      "us-east-1",
			EndpointURL: endpoint,
		},
		Credentials: map[string]string{
			envAWSAccessKeyID:     "test",
			envAWSSecretAccessKey: "test",
		},
	})
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}
	return store
}

func TestS3StoreRoundTripAgainstFakeServer(t *testing.T) {
	ctx := context.Background()
	srv := startFakeS3(t)
	store := newS3StoreAgainst(t, srv.URL, "prod/")

	// Exists on absent key.
	if ok, err := store.Exists(ctx, "c/b/missing"); err != nil || ok {
		t.Fatalf("Exists(absent) = %v,%v want false,nil", ok, err)
	}
	// Get on absent key -> ErrNotExist.
	if _, err := store.Get(ctx, "c/b/missing"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get(absent) err = %v, want ErrNotExist", err)
	}

	// Put + Get round-trip.
	key := ShardRDBKey("c", "b", 0)
	want := []byte("rdb-bytes-via-s3-sdk")
	if err := store.Put(ctx, key, strings.NewReader(string(want)), int64(len(want))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != string(want) {
		t.Fatalf("Get = %q, want %q", got, want)
	}

	// Exists on present key.
	if ok, err := store.Exists(ctx, key); err != nil || !ok {
		t.Fatalf("Exists(present) = %v,%v want true,nil", ok, err)
	}

	// Manifest + List (store-relative keys, prefix stripped).
	if err = WriteManifest(ctx, store, ManifestKey("c", "b"), Manifest{Cluster: "c", BackupName: "b", SlotCoverage: "complete"}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	keys, err := store.List(ctx, SetPrefix("c", "b"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	slices.Sort(keys)
	wantKeys := []string{ManifestKey("c", "b"), ShardRDBKey("c", "b", 0)}
	slices.Sort(wantKeys)
	if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("List = %v, want %v (store-relative, prefix-stripped)", keys, wantKeys)
	}

	// Delete (idempotent on re-delete).
	if err = store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err = store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete(idempotent): %v", err)
	}
	if ok, _ := store.Exists(ctx, key); ok {
		t.Fatalf("Exists after delete = true, want false")
	}
}

func TestS3StoreListNoPrefixAgainstFakeServer(t *testing.T) {
	ctx := context.Background()
	srv := startFakeS3(t)
	store := newS3StoreAgainst(t, srv.URL, "")

	for i := range 3 {
		if err := store.Put(ctx, ShardRDBKey("c", "b", i), strings.NewReader("x"), 1); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	keys, err := store.List(ctx, "c/b")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("List = %v, want 3 keys", keys)
	}
}
