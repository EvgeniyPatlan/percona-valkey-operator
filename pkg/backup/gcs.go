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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// GCS credential env-var keys (06 §3.4). The service-account JSON is mounted as a
// file at GOOGLE_APPLICATION_CREDENTIALS, or its contents passed inline via
// GOOGLE_APPLICATION_CREDENTIALS_JSON (the StorageConfig.Credentials convention).
const (
	envGoogleAppCreds     = "GOOGLE_APPLICATION_CREDENTIALS"
	envGoogleAppCredsJSON = "GOOGLE_APPLICATION_CREDENTIALS_JSON"
)

// init wires the GCS backend into the constructor seam (config.go).
func init() {
	RegisterBackend(valkeyv1alpha1.BackupStorageGCS, newGCSStore)
}

// gcsStore is the Google Cloud Storage ArtifactStore. The GCS SDK's object
// Reader/Writer are themselves streaming, so Put/Get pipe data through without
// buffering the whole RDB (06 §4.8). Authentication uses the service-account
// JSON from the mounted Secret (file path or inline), falling back to ADC.
type gcsStore struct {
	client *gcs.Client
	bucket string
	prefix string
}

// compile-time assertion that gcsStore satisfies the interface.
var _ ArtifactStore = (*gcsStore)(nil)

// newGCSStore builds a gcsStore from the resolved StorageConfig, resolving the
// service-account credentials from the mounted Secret (inline JSON value or a
// file path) and deferring to Application Default Credentials when neither is
// set.
func newGCSStore(ctx context.Context, cfg StorageConfig) (ArtifactStore, error) {
	if cfg.GCS == nil {
		return nil, fmt.Errorf("gcs backend: nil GCS spec")
	}
	if cfg.GCS.Bucket == "" {
		return nil, fmt.Errorf("gcs backend: empty bucket")
	}

	opts, err := gcsClientOptions(cfg.Credentials)
	if err != nil {
		return nil, err
	}
	client, err := gcs.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gcs backend: new client: %w", err)
	}
	return &gcsStore{client: client, bucket: cfg.GCS.Bucket, prefix: cfg.GCS.Prefix}, nil
}

// gcsClientOptions turns mounted Secret values into GCS client options. Inline
// JSON wins over a file path; an empty map yields no explicit option so the SDK
// uses ADC. The service-account credential type is passed explicitly (the
// non-deprecated WithAuthCredentials* form) so the inline-JSON path is not the
// deprecated WithCredentialsJSON.
func gcsClientOptions(env map[string]string) ([]option.ClientOption, error) {
	if jsonVal := env[envGoogleAppCredsJSON]; jsonVal != "" {
		return []option.ClientOption{option.WithAuthCredentialsJSON(option.ServiceAccount, []byte(jsonVal))}, nil
	}
	if path := env[envGoogleAppCreds]; path != "" {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("gcs backend: credentials file %q: %w", path, err)
		}
		return []option.ClientOption{option.WithAuthCredentialsFile(option.ServiceAccount, path)}, nil
	}
	return nil, nil
}

// fullKey roots a store-relative key under the backend's configured prefix.
func (s *gcsStore) fullKey(key string) string {
	return joinKey(s.prefix, key)
}

// object returns the ObjectHandle for a store-relative key.
func (s *gcsStore) object(key string) *gcs.ObjectHandle {
	return s.client.Bucket(s.bucket).Object(s.fullKey(key))
}

// Put streams r into a GCS object Writer (chunked uploads, bounded memory). The
// Writer must Close to flush; a Copy or Close failure aborts the upload, leaving
// no finalized object (06 §9.3).
func (s *gcsStore) Put(ctx context.Context, key string, r io.Reader, _ int64) error {
	w := s.object(key).NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return fmt.Errorf("gcs backend: put %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs backend: finalize %q: %w", key, err)
	}
	return nil
}

// Get opens the object at key for streaming reads. A missing object maps to a
// wrapped ErrNotExist.
func (s *gcsStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.object(key).NewReader(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return nil, fmt.Errorf("gcs backend: get %q: %w", key, ErrNotExist)
		}
		return nil, fmt.Errorf("gcs backend: get %q: %w", key, err)
	}
	return rc, nil
}

// List returns the store-relative keys under prefix via the object iterator,
// stripping the backend's configured prefix so results are store-relative.
func (s *gcsStore) List(ctx context.Context, prefix string) ([]string, error) {
	it := s.client.Bucket(s.bucket).Objects(ctx, &gcs.Query{Prefix: s.fullKey(prefix)})
	var keys []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs backend: list %q: %w", prefix, err)
		}
		keys = append(keys, s.stripPrefix(attrs.Name))
	}
	return keys, nil
}

// Delete removes the object at key. An already-gone object is a no-op success
// (idempotent), as the manifest-first teardown requires (06 §6.1).
func (s *gcsStore) Delete(ctx context.Context, key string) error {
	err := s.object(key).Delete(ctx)
	if err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
		return fmt.Errorf("gcs backend: delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether key is present via Attrs. A missing object is
// (false, nil); only a real failure returns a non-nil error.
func (s *gcsStore) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.object(key).Attrs(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("gcs backend: exists %q: %w", key, err)
	}
	return true, nil
}

// stripPrefix removes the backend's configured prefix from a full object name,
// returning the store-relative key.
func (s *gcsStore) stripPrefix(fullName string) string {
	if s.prefix == "" {
		return fullName
	}
	trimmed := strings.Trim(s.prefix, "/") + "/"
	return strings.TrimPrefix(fullName, trimmed)
}
