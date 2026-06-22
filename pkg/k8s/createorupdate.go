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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Owner is an object that can own children via controller owner references: a
// namespaced object that also exposes its scheme runtime.Object identity.
type Owner interface {
	client.Object
	runtime.Object
}

// CreateOrUpdate creates obj if it does not exist or updates it to the mutated
// state otherwise, setting a controller owner reference to owner inside the
// mutation so the child is GC'd with its parent and an owner change re-enqueues
// the parent. It wraps controllerutil.CreateOrUpdate (which re-fetches and
// retries on conflict per the 04 §9 re-fetch-before-update rule). The mutate
// closure must set the desired spec/labels on obj; SetControllerReference is
// applied automatically after mutate runs, so callers must not clear ownerRefs.
func CreateOrUpdate(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner Owner,
	obj client.Object,
	mutate func() error,
) (controllerutil.OperationResult, error) {
	res, err := controllerutil.CreateOrUpdate(ctx, c, obj, func() error {
		if mutate != nil {
			if mErr := mutate(); mErr != nil {
				return mErr
			}
		}
		return controllerutil.SetControllerReference(owner, obj, scheme)
	})
	if err != nil {
		return res, fmt.Errorf("createOrUpdate %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	return res, nil
}

// SetControllerOwnerRef sets a controller owner reference (controller=true,
// blockOwnerDeletion=true per 03 §1) on obj pointing at owner. Useful when an
// object is built outside a CreateOrUpdate mutate closure.
func SetControllerOwnerRef(owner Owner, obj metav1.Object, scheme *runtime.Scheme) error {
	co, ok := obj.(client.Object)
	if !ok {
		return fmt.Errorf("object %T is not a client.Object", obj)
	}
	if err := controllerutil.SetControllerReference(owner, co, scheme); err != nil {
		return fmt.Errorf("set controller reference: %w", err)
	}
	return nil
}
