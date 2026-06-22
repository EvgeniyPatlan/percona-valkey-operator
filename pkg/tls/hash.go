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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// certKeys is the canonical, fixed order in which the three TLS Secret data keys
// are folded into the roll-triggering hash. The order is FROZEN so the hash is
// byte-stable across reconciles for unchanged material (07 §3.4): a map iteration
// would be non-deterministic and could churn the hash without a real cert change.
var certKeys = []string{
	naming.TLSSecretKeyCA,
	naming.TLSSecretKeyCert,
	naming.TLSSecretKeyKey,
}

// ValidateSecretData fails CLOSED when any of the three required TLS keys
// (ca.crt/tls.crt/tls.key) is missing or empty from the Secret data (07 §3.3,
// §3.4): the reconcile must never fall back to plaintext on a malformed Secret.
// The returned error names the offending key (never echoes secret material).
func ValidateSecretData(name string, data map[string][]byte) error {
	for _, k := range certKeys {
		v, ok := data[k]
		if !ok || len(v) == 0 {
			return fmt.Errorf("tls secret %q missing or empty key %q", name, k)
		}
	}
	return nil
}

// ComputeTLSHash returns the roll-triggering hash over the cert material identity
// (SHA-256 of ca.crt/tls.crt/tls.key, folded in a FIXED order). It first validates
// the three keys are present (fail-closed, 07 §3.3): a malformed/incomplete Secret
// returns a non-nil error that the controller surfaces as Degraded/TLSError rather
// than a silent plaintext fallback. For identical material the hash is byte-stable,
// and it changes ONLY on a real cert change, so it never causes a phantom roll
// (07 §3.4).
func ComputeTLSHash(secret *corev1.Secret) (string, error) {
	if secret == nil {
		return "", fmt.Errorf("tls secret is nil")
	}
	if err := ValidateSecretData(secret.Name, secret.Data); err != nil {
		return "", err
	}
	h := sha256.New()
	for _, k := range certKeys {
		// Length-prefix each field so distinct key boundaries can never collide
		// (e.g. moving a byte between tls.crt and tls.key must change the hash).
		v := secret.Data[k]
		_, _ = fmt.Fprintf(h, "%s:%d:", k, len(v))
		_, _ = h.Write(v)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
