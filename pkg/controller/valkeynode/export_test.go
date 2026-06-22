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

package valkeynode

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// NewReconcilerForTest builds a Reconciler wired with an injected (fake)
// ClientFactory and a no-op recorder, so envtest can drive Reconcile directly
// without a kubelet/pod. This is the mockable-seam entry point referenced by
// 04 §3 (the controller's connect-to-pod path is behind ClientFactory). It lives
// in an _test.go file so it ships only with tests, not the production binary.
func NewReconcilerForTest(c client.Client, scheme *runtime.Scheme, factory valkey.ClientFactory) *Reconciler {
	return &Reconciler{
		Client:             c,
		scheme:             scheme,
		recorder:           events.NewFakeRecorder(100),
		clientFactory:      factory,
		skipNameValidation: true,
	}
}

// RecorderForTest swaps the reconciler's event recorder so a test can assert on
// emitted events.
func (r *Reconciler) RecorderForTest(rec events.EventRecorder) {
	r.recorder = rec
}
