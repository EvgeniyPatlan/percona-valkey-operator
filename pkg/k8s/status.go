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

package k8s

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WriteStatus persists obj's status subresource using a server-side merge patch
// from a fresh read of the object, honouring the 04 §9 re-fetch-before-update
// rule: it re-Gets the object, copies the in-memory status onto the freshly read
// copy, and patches only the status subresource. This avoids clobbering spec
// changes that landed since the reconcile started and avoids stale-resourceVersion
// conflicts. statusOf must return a pointer to obj's status field for the copy.
func WriteStatus[T client.Object, S any](
	ctx context.Context,
	c client.Client,
	obj T,
	statusOf func(T) *S,
) error {
	fresh, ok := obj.DeepCopyObject().(T)
	if !ok {
		return fmt.Errorf("status writeback: %T does not deep-copy to its own type", obj)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
		return fmt.Errorf("status writeback re-fetch %s: %w", client.ObjectKeyFromObject(obj), err)
	}
	base := fresh.DeepCopyObject().(T)
	*statusOf(fresh) = *statusOf(obj)
	if err := c.Status().Patch(ctx, fresh, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("status patch %s: %w", client.ObjectKeyFromObject(obj), err)
	}
	return nil
}
