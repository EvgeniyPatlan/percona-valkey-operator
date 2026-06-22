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

package perconavalkeyrestore

import (
	"context"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// newReconcilerForTest builds a Reconciler wired with an injected storeFactory (a
// FakeStore seam) and a buffered fake recorder so envtest drives the restore phase
// machine against an in-memory backup-set without a real object store.
func newReconcilerForTest(c client.Client, scheme *runtime.Scheme, factory StoreFactory) *Reconciler {
	return &Reconciler{
		Client:             c,
		scheme:             scheme,
		recorder:           events.NewFakeRecorder(500),
		storeFactory:       factory,
		skipNameValidation: true,
	}
}

// fixedStoreFactory returns a StoreFactory that always yields the given store,
// ignoring the StorageConfig — the simplest test seam for a FakeStore-backed set.
func fixedStoreFactory(store backup.ArtifactStore) StoreFactory {
	return func(_ context.Context, _ backup.StorageConfig) (backup.ArtifactStore, error) {
		return store, nil
	}
}

// clusterTemplateAnnotation builds the cluster-template annotation blob carrying a
// shards-only embedded spec (used to drive shard-count validation).
func clusterTemplateAnnotation(shards int) string {
	return `{"shards":` + strconv.Itoa(shards) + `}`
}
