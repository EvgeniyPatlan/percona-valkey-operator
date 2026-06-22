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

package tls

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LoadSecret reads the TLS Secret named (ns, name) and returns it. A NotFound (or
// any read error) is returned wrapped, so a missing user-supplied Secret fails the
// reconcile CLOSED (07 §3.3) instead of silently dropping TLS. The error never
// echoes secret material — only the Secret name.
func LoadSecret(ctx context.Context, c client.Client, ns, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("get tls secret %q: %w", name, err)
	}
	return secret, nil
}

// ValidateSecretRef reads the referenced Secret and fails CLOSED unless it carries
// a non-empty ca.crt/tls.crt/tls.key (secret-ref mode, 07 §3.3). It is the
// validation half of secret-ref TLS; the hash is computed separately by the caller
// via ComputeTLSHash on the same Secret.
func ValidateSecretRef(ctx context.Context, c client.Client, ns, name string) error {
	secret, err := LoadSecret(ctx, c, ns, name)
	if err != nil {
		return err
	}
	return ValidateSecretData(secret.Name, secret.Data)
}
