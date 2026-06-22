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
	"strings"

	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// S3 credential env-var keys (06 §3.4). The Job mounts the credentialsSecret as
// env under these names; StorageConfig.Credentials carries the VALUES.
const (
	envAWSAccessKeyID     = "AWS_ACCESS_KEY_ID"
	envAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	envAWSSessionToken    = "AWS_SESSION_TOKEN"
)

// init wires the S3 backend into the constructor seam (config.go).
func init() {
	RegisterBackend(valkeyv1alpha1.BackupStorageS3, newS3Store)
}

// s3TestHTTPClient is a test-only hook: when non-nil it overrides the S3
// client's HTTP transport so the round-trip suite can point the real AWS SDK at
// a local TLS httptest server (whose self-signed cert the default transport
// would reject). It is nil in production, so the SDK uses its default client.
var s3TestHTTPClient *http.Client

// s3Store is the AWS-SDK-v2 ArtifactStore. It honours endpointUrl so MinIO /
// Ceph / other S3-compatible stores work, constructing the endpoint EXPLICITLY
// (BaseEndpoint + path-style) rather than relying on regional auto-routing — the
// documented Percona custom-endpoint trap (06 §8.2). PutObject streams the body
// reader directly with an explicit ContentLength (the SYNC RDB length is always
// known), so the request body is sent without buffering the whole RDB in memory
// (06 §4.8); downloads stream straight off GetObject's response body.
type s3Store struct {
	client *s3.Client
	bucket string
	// prefix is the configured in-bucket key prefix; every store-relative key is
	// rooted under it so a backend owns a sub-tree of the bucket.
	prefix string
}

// compile-time assertion that s3Store satisfies the interface.
var _ ArtifactStore = (*s3Store)(nil)

// newS3Store builds an s3Store from the resolved StorageConfig. Credentials come
// from cfg.Credentials (the Job's mounted Secret values) when present; otherwise
// it falls back to the SDK default chain (instance/role credentials). A custom
// endpointUrl forces path-style addressing so bucket-in-host routing (which
// breaks on MinIO) is bypassed.
func newS3Store(ctx context.Context, cfg StorageConfig) (ArtifactStore, error) {
	if cfg.S3 == nil {
		return nil, fmt.Errorf("s3 backend: nil S3 spec")
	}
	if cfg.S3.Bucket == "" {
		return nil, fmt.Errorf("s3 backend: empty bucket")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if cfg.S3.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.S3.Region))
	}
	if creds, ok := staticAWSCreds(cfg.Credentials); ok {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(creds))
	}
	if s3TestHTTPClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(s3TestHTTPClient))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3 backend: load config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		// The RDB body streamed from SYNC is UNSEEKABLE; the SDK default
		// (WhenSupported) forces a CRC32 request checksum that needs a seekable
		// body or a TLS trailing-checksum, which an unseekable plain-HTTP stream
		// cannot satisfy. We compute and verify our own SHA-256 over the stream
		// (the manifest integrity check), so only emit a request checksum when the
		// operation actually requires one.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		if cfg.S3.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.S3.EndpointURL)
			// Custom endpoints (MinIO/Ceph) almost always need path-style
			// addressing; virtual-host style assumes bucket.<endpoint> DNS.
			o.UsePathStyle = true
		}
	})

	return &s3Store{
		client: client,
		bucket: cfg.S3.Bucket,
		prefix: cfg.S3.Prefix,
	}, nil
}

// staticAWSCreds builds a static credentials provider from mounted Secret values
// when an access key is present. Returns ok=false to defer to the SDK default
// credential chain (so IRSA / instance roles still work with no Secret).
func staticAWSCreds(env map[string]string) (aws.CredentialsProvider, bool) {
	id := env[envAWSAccessKeyID]
	secret := env[envAWSSecretAccessKey]
	if id == "" || secret == "" {
		return nil, false
	}
	return credentials.NewStaticCredentialsProvider(id, secret, env[envAWSSessionToken]), true
}

// fullKey roots a store-relative key under the backend's configured prefix.
func (s *s3Store) fullKey(key string) string {
	return joinKey(s.prefix, key)
}

// Put streams r to the object at key via PutObject. When size >= 0 it is set as
// the ContentLength so the SDK streams the body without buffering it to compute a
// length (memory independent of RDB size, 06 §4.8); a negative size falls back to
// the SDK's own handling. A failed PutObject leaves no committed object (06 §9.3).
func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
		Body:   r,
	}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}
	if _, err := s.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3 backend: put %q: %w", key, err)
	}
	return nil
}

// Get opens the object at key for streaming reads via GetObject; the response
// Body is the streaming ReadCloser the caller must Close. A missing key maps to
// a wrapped ErrNotExist.
func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("s3 backend: get %q: %w", key, ErrNotExist)
		}
		return nil, fmt.Errorf("s3 backend: get %q: %w", key, err)
	}
	return out.Body, nil
}

// List returns the store-relative keys under prefix via paginated ListObjectsV2.
// The backend's own configured prefix is stripped from each returned key so the
// result is store-relative (the same key-space the helpers in keys.go produce).
func (s *s3Store) List(ctx context.Context, prefix string) ([]string, error) {
	listPrefix := s.fullKey(prefix)
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(listPrefix),
	})
	var keys []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 backend: list %q: %w", prefix, err)
		}
		for i := range page.Contents {
			if page.Contents[i].Key == nil {
				continue
			}
			keys = append(keys, s.stripPrefix(*page.Contents[i].Key))
		}
	}
	return keys, nil
}

// Delete removes the object at key. An already-gone key is a no-op success
// (idempotent), as the manifest-first teardown requires (06 §6.1). S3 DeleteObject
// is itself idempotent, so a missing key returns no error.
func (s *s3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("s3 backend: delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether key is present via HeadObject. A missing key is
// (false, nil); only a real failure returns a non-nil error.
func (s *s3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3 backend: exists %q: %w", key, err)
	}
	return true, nil
}

// stripPrefix removes the backend's configured prefix from a full object key,
// returning the store-relative key.
func (s *s3Store) stripPrefix(fullKey string) string {
	if s.prefix == "" {
		return fullKey
	}
	trimmed := strings.TrimPrefix(strings.Trim(s.prefix, "/")+"/", "/")
	return strings.TrimPrefix(fullKey, trimmed)
}

// isS3NotFound reports whether err is an S3 "key/bucket not found" condition,
// covering both the typed NoSuchKey/NotFound errors and a bare 404 API error
// (HeadObject returns the latter with no typed shape).
func isS3NotFound(err error) bool {
	var noKey *s3types.NoSuchKey
	var notFound *s3types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
