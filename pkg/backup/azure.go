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
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// Azure credential env-var keys (06 §3.4). The Job mounts the credentialsSecret
// as env under these names; StorageConfig.Credentials carries the VALUES.
const (
	envAzureStorageAccount = "AZURE_STORAGE_ACCOUNT"
	envAzureStorageKey     = "AZURE_STORAGE_KEY"
)

// init wires the Azure Blob backend into the constructor seam (config.go).
func init() {
	RegisterBackend(valkeyv1alpha1.BackupStorageAzure, newAzureStore)
}

// azureServiceURL is overridable in tests (a func, so leaving it as the default
// derivation requires no test plumbing in production). It derives the blob
// service URL from the account name; the live Azure CRUD path is otherwise
// exercised by e2e against a real account / Azurite emulator (GO-4.3 test plan).
var azureServiceURL = func(account string) string {
	return fmt.Sprintf("https://%s.blob.core.windows.net/", account)
}

// azureStore is the Azure-Blob ArtifactStore. It addresses a single container,
// streaming uploads via UploadStream (block-blob staging, bounded buffers) and
// downloads via DownloadStream's response body, so memory stays independent of
// RDB size (06 §4.8). Authentication uses the storage-account shared key from
// the mounted Secret.
type azureStore struct {
	client    *azblob.Client
	container string
	prefix    string
}

// compile-time assertion that azureStore satisfies the interface.
var _ ArtifactStore = (*azureStore)(nil)

// newAzureStore builds an azureStore from the resolved StorageConfig. It requires
// AZURE_STORAGE_ACCOUNT and AZURE_STORAGE_KEY in cfg.Credentials (the mounted
// Secret values) and derives the service URL from the account name (honouring an
// explicit endpointUrl is not part of the v1alpha1 Azure spec, 06 §3.4).
func newAzureStore(_ context.Context, cfg StorageConfig) (ArtifactStore, error) {
	if cfg.Azure == nil {
		return nil, fmt.Errorf("azure backend: nil Azure spec")
	}
	if cfg.Azure.Container == "" {
		return nil, fmt.Errorf("azure backend: empty container")
	}
	account := cfg.Credentials[envAzureStorageAccount]
	key := cfg.Credentials[envAzureStorageKey]
	if account == "" || key == "" {
		return nil, fmt.Errorf("azure backend: missing %s/%s credentials", envAzureStorageAccount, envAzureStorageKey)
	}

	cred, err := azblob.NewSharedKeyCredential(account, key)
	if err != nil {
		return nil, fmt.Errorf("azure backend: shared-key credential: %w", err)
	}
	client, err := azblob.NewClientWithSharedKeyCredential(azureServiceURL(account), cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azure backend: new client: %w", err)
	}
	return &azureStore{client: client, container: cfg.Azure.Container, prefix: cfg.Azure.Prefix}, nil
}

// fullKey roots a store-relative key under the backend's configured prefix.
func (s *azureStore) fullKey(key string) string {
	return joinKey(s.prefix, key)
}

// Put streams r to the blob at key via UploadStream (block staging, bounded
// memory). A failure leaves no committed blob (06 §9.3).
func (s *azureStore) Put(ctx context.Context, key string, r io.Reader, _ int64) error {
	_, err := s.client.UploadStream(ctx, s.container, s.fullKey(key), r, nil)
	if err != nil {
		return fmt.Errorf("azure backend: put %q: %w", key, err)
	}
	return nil
}

// Get opens the blob at key for streaming reads via DownloadStream; the response
// Body is the streaming ReadCloser the caller must Close. A missing blob maps to
// a wrapped ErrNotExist.
func (s *azureStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.client.DownloadStream(ctx, s.container, s.fullKey(key), nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, fmt.Errorf("azure backend: get %q: %w", key, ErrNotExist)
		}
		return nil, fmt.Errorf("azure backend: get %q: %w", key, err)
	}
	return resp.Body, nil
}

// List returns the store-relative keys under prefix via the flat blob pager,
// stripping the backend's configured prefix so results are store-relative.
func (s *azureStore) List(ctx context.Context, prefix string) ([]string, error) {
	listPrefix := s.fullKey(prefix)
	pager := s.client.NewListBlobsFlatPager(s.container, &container.ListBlobsFlatOptions{
		Prefix: &listPrefix,
	})
	var keys []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure backend: list %q: %w", prefix, err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil {
				continue
			}
			keys = append(keys, s.stripPrefix(*item.Name))
		}
	}
	return keys, nil
}

// Delete removes the blob at key. An already-gone blob is a no-op success
// (idempotent), as the manifest-first teardown requires (06 §6.1).
func (s *azureStore) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteBlob(ctx, s.container, s.fullKey(key), nil)
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return fmt.Errorf("azure backend: delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether key is present. It reuses DownloadStream and closes the
// body immediately; a missing blob is (false, nil), any other failure errors.
func (s *azureStore) Exists(ctx context.Context, key string) (bool, error) {
	resp, err := s.client.DownloadStream(ctx, s.container, s.fullKey(key), nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("azure backend: exists %q: %w", key, err)
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	return true, nil
}

// stripPrefix removes the backend's configured prefix from a full blob name,
// returning the store-relative key.
func (s *azureStore) stripPrefix(fullName string) string {
	if s.prefix == "" {
		return fullName
	}
	trimmed := strings.Trim(s.prefix, "/") + "/"
	return strings.TrimPrefix(fullName, trimmed)
}
