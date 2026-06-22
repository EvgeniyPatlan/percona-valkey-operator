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
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReasonWebhookCertNotReady is the Degraded condition reason raised when the
// conversion-webhook serving certificate is missing/expired or cert-manager has
// not yet populated it (07 §3.5). It gates webhook registration so the API server
// never routes a conversion call at a webhook whose TLS material is absent.
//
// SCAFFOLD (M5 GO-5.14): this and WaitForWebhookCert are the reusable bootstrap
// gate M6's conversion webhook plugs into. No ConvertTo/ConvertFrom logic ships in
// M5 — the conversion body is M6 (arch 09 §6).
const ReasonWebhookCertNotReady = "WebhookCertNotReady"

// defaultWebhookCertPollInterval is the backoff between Secret polls in the
// startup gate.
const defaultWebhookCertPollInterval = 2 * time.Second

// WaitForWebhookCert blocks (with bounded polling, honouring ctx) until the named
// webhook Secret carries a non-empty tls.crt/tls.key/ca.crt, then returns nil. It
// is the manager startup gate for the conversion webhook (07 §3.5): leader
// election must not begin reconciling clusters until this passes, so the API
// server cannot route a conversion call to a webhook whose serving cert is not yet
// written. It is conversion-agnostic — M6 supplies the actual handler.
//
// A nil/zero timeout polls until ctx is cancelled.
func WaitForWebhookCert(ctx context.Context, c client.Client, secret types.NamespacedName, timeout time.Duration) error {
	cond := func(ctx context.Context) (bool, error) {
		if err := ValidateSecretRef(ctx, c, secret.Namespace, secret.Name); err != nil {
			// Not yet populated (or transiently unreadable): keep waiting rather
			// than failing the gate, so cert-manager has time to write the Secret.
			return false, nil
		}
		return true, nil
	}

	waitCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err := wait.PollUntilContextCancel(waitCtx, defaultWebhookCertPollInterval, true, cond); err != nil {
		return fmt.Errorf("waiting for webhook cert secret %q: %w", secret.Name, err)
	}
	return nil
}
