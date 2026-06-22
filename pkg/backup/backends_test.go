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
	"testing"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// TestAllBackendsRegistered proves the Leg-A init() functions wired every
// storage type into the constructor seam so BackendRegistered (used by the
// controller's fail-fast CheckNSetDefaults) reports them all.
func TestAllBackendsRegistered(t *testing.T) {
	for _, typ := range []valkeyv1alpha1.BackupStorageType{
		valkeyv1alpha1.BackupStorageS3,
		valkeyv1alpha1.BackupStorageGCS,
		valkeyv1alpha1.BackupStorageAzure,
		valkeyv1alpha1.BackupStorageFilesystem,
	} {
		if !BackendRegistered(typ) {
			t.Errorf("BackendRegistered(%q) = false, want true", typ)
		}
	}
}

func TestNewStoreUnknownType(t *testing.T) {
	if _, err := NewStore(context.Background(), StorageConfig{Type: "nonsense"}); err == nil {
		t.Fatalf("NewStore(unknown type) = nil error, want failure")
	}
}

// TestNewStoreDispatchesByType confirms NewStore routes to the correct concrete
// backend per StorageConfig.Type.
func TestNewStoreDispatchesByType(t *testing.T) {
	ctx := context.Background()

	fs, err := NewStore(ctx, StorageConfig{
		Type:           valkeyv1alpha1.BackupStorageFilesystem,
		FilesystemRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore(filesystem): %v", err)
	}
	if _, ok := fs.(*fsStore); !ok {
		t.Fatalf("filesystem dispatched to %T, want *fsStore", fs)
	}

	s3, err := NewStore(ctx, StorageConfig{
		Type:        valkeyv1alpha1.BackupStorageS3,
		S3:          &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "b", Region: "us-east-1"},
		Credentials: map[string]string{envAWSAccessKeyID: "id", envAWSSecretAccessKey: "s"},
	})
	if err != nil {
		t.Fatalf("NewStore(s3): %v", err)
	}
	if _, ok := s3.(*s3Store); !ok {
		t.Fatalf("s3 dispatched to %T, want *s3Store", s3)
	}
}
