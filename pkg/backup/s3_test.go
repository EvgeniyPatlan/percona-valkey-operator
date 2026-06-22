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
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

func TestNewS3StoreConstruction(t *testing.T) {
	ctx := context.Background()
	cfg := StorageConfig{
		Type: valkeyv1alpha1.BackupStorageS3,
		S3: &valkeyv1alpha1.BackupStorageS3Spec{
			Bucket:      "percona-valkey-backups",
			Prefix:      "prod/",
			Region:      "eu-central-1",
			EndpointURL: "https://minio.local:9000",
		},
		Credentials: map[string]string{
			envAWSAccessKeyID:     "AKIA",
			envAWSSecretAccessKey: "secret",
		},
	}
	store, err := newS3Store(ctx, cfg)
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}
	s3s, ok := store.(*s3Store)
	if !ok {
		t.Fatalf("expected *s3Store")
	}
	if s3s.bucket != "percona-valkey-backups" || s3s.prefix != "prod/" {
		t.Fatalf("s3Store fields = %q/%q", s3s.bucket, s3s.prefix)
	}
	if s3s.client == nil {
		t.Fatalf("s3Store client not initialised")
	}
}

func TestNewS3StoreErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		cfg  StorageConfig
	}{
		{"nil spec", StorageConfig{Type: valkeyv1alpha1.BackupStorageS3}},
		{"empty bucket", StorageConfig{Type: valkeyv1alpha1.BackupStorageS3, S3: &valkeyv1alpha1.BackupStorageS3Spec{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newS3Store(ctx, tc.cfg); err == nil {
				t.Fatalf("newS3Store(%s) = nil error, want failure", tc.name)
			}
		})
	}
}

func TestStaticAWSCreds(t *testing.T) {
	if _, ok := staticAWSCreds(nil); ok {
		t.Fatalf("staticAWSCreds(nil) ok = true, want false")
	}
	if _, ok := staticAWSCreds(map[string]string{envAWSAccessKeyID: "only-id"}); ok {
		t.Fatalf("staticAWSCreds(partial) ok = true, want false")
	}
	prov, ok := staticAWSCreds(map[string]string{
		envAWSAccessKeyID:     "id",
		envAWSSecretAccessKey: "secret",
		envAWSSessionToken:    "tok",
	})
	if !ok || prov == nil {
		t.Fatalf("staticAWSCreds(full) = %v,%v want provider,true", prov, ok)
	}
	creds, err := prov.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != "id" || creds.SecretAccessKey != "secret" || creds.SessionToken != "tok" {
		t.Fatalf("creds = %+v", creds)
	}
}

func TestS3StoreFullKeyAndStripPrefix(t *testing.T) {
	s := &s3Store{prefix: "prod/"}
	if got := s.fullKey("c/b/manifest.json"); got != "prod/c/b/manifest.json" {
		t.Fatalf("fullKey = %q", got)
	}
	if got := s.stripPrefix("prod/c/b/manifest.json"); got != "c/b/manifest.json" {
		t.Fatalf("stripPrefix = %q", got)
	}
	// No prefix configured: fullKey/stripPrefix are identity.
	bare := &s3Store{}
	if got := bare.fullKey("c/b/x"); got != "c/b/x" {
		t.Fatalf("bare fullKey = %q", got)
	}
	if got := bare.stripPrefix("c/b/x"); got != "c/b/x" {
		t.Fatalf("bare stripPrefix = %q", got)
	}
}

func TestIsS3NotFound(t *testing.T) {
	if isS3NotFound(errors.New("transient")) {
		t.Fatalf("isS3NotFound(generic) = true, want false")
	}
	if !isS3NotFound(&s3types.NoSuchKey{}) {
		t.Fatalf("isS3NotFound(NoSuchKey) = false, want true")
	}
	if !isS3NotFound(&s3types.NotFound{}) {
		t.Fatalf("isS3NotFound(NotFound) = false, want true")
	}
	if !isS3NotFound(&stubAPIError{code: "404"}) {
		t.Fatalf("isS3NotFound(404 APIError) = false, want true")
	}
	if isS3NotFound(&stubAPIError{code: "AccessDenied"}) {
		t.Fatalf("isS3NotFound(AccessDenied) = true, want false")
	}
}

// stubAPIError is a minimal smithy.APIError for the 404-code path of
// isS3NotFound (HeadObject returns a bare APIError with no typed shape).
type stubAPIError struct{ code string }

func (e *stubAPIError) Error() string                 { return e.code }
func (e *stubAPIError) ErrorCode() string             { return e.code }
func (e *stubAPIError) ErrorMessage() string          { return e.code }
func (e *stubAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }
