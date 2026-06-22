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

package perconavalkeybackup

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// NewReconcilerForTest builds a backup Reconciler wired with an injected
// ArtifactStore factory (a FakeStore), a fake event recorder, and a fixed clock,
// so envtest drives the phase machine hermetically without a real object store or
// wall-clock waits. It lives in an _test.go file so it ships only with tests.
func NewReconcilerForTest(c client.Client, scheme *runtime.Scheme, factory StoreFactory, clk func() time.Time) *Reconciler {
	return &Reconciler{
		Client:             c,
		scheme:             scheme,
		recorder:           events.NewFakeRecorder(200),
		storeFactory:       factory,
		clock:              clk,
		skipNameValidation: true,
	}
}

// RecorderForTest swaps the reconciler's event recorder so a test can assert on
// emitted events.
func (r *Reconciler) RecorderForTest(rec events.EventRecorder) {
	r.recorder = rec
}

// ReconcileForTest exposes Reconcile for direct-call envtest specs.
func (r *Reconciler) ReconcileForTest(ctx context.Context, name, namespace string) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: name, Namespace: namespace}})
}

// FakeStoreFactory returns a StoreFactory that always yields the supplied
// FakeStore, ignoring the StorageConfig (the operator process never carries
// credential values, 06 §8.2).
func FakeStoreFactory(fs *backup.FakeStore) StoreFactory {
	return func(_ context.Context, _ backup.StorageConfig) (backup.ArtifactStore, error) {
		return fs, nil
	}
}
