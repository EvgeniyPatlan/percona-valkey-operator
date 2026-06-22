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
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// LiveConfigApplied condition reasons (04 §3.1 step 5 / 08 §4.3).
const (
	reasonLiveConfigApplied = "Applied"
	reasonNoLiveKeys        = "NoLiveKeys"
	reasonApplyFailed       = "ApplyFailed"
)

// The EXACT live-settable config keys (05 §11). Hard-coded as constants so a
// roll-only key can never be hot-applied.
const (
	keyMaxClients     = "maxclients"
	keyMaxMemory      = "maxmemory"
	keyMaxMemoryPolic = "maxmemory-policy"
)

// liveSettableKeys is the EXACT live-settable allowlist (05 §11), sorted for
// deterministic apply order.
func liveSettableKeys() []string {
	return []string{keyMaxClients, keyMaxMemory, keyMaxMemoryPolic}
}

// pendingLiveConfig returns the subset of node.Spec.Config restricted to the
// live-settable allowlist, in deterministic key order.
func pendingLiveConfig(node *valkeyv1alpha1.ValkeyNode) [][2]string {
	var pending [][2]string
	for _, k := range liveSettableKeys() {
		if v, ok := node.Spec.Config[k]; ok {
			pending = append(pending, [2]string{k, v})
		}
	}
	return pending
}

// applyLiveConfig applies the live-settable allowlist via CONFIG SET over the
// connection the factory built to status.podIP. Sets LiveConfigApplied=True on
// success, NoLiveKeys when none apply, and False (+Warning event) on a CONFIG
// SET error — fail-closed, no auto-remediation, retried each reconcile. Returns
// the CONFIG SET error so the reconciler can flip status.ready=false and block
// the cluster roll (04 §3.1 step 5 / §9).
func (r *Reconciler) applyLiveConfig(
	ctx context.Context, node *valkeyv1alpha1.ValkeyNode, vc valkey.ConfigClient,
) error {
	pending := pendingLiveConfig(node)
	if len(pending) == 0 {
		setCondition(node, valkeyv1alpha1.NodeConditionLiveConfigApplied, metav1.ConditionTrue,
			reasonNoLiveKeys, "no live-settable keys in spec.config")
		return nil
	}
	for _, kv := range pending {
		key, value := kv[0], kv[1]
		if err := vc.ConfigSet(ctx, key, value); err != nil {
			r.recorder.Eventf(node, nil, corev1.EventTypeWarning, "LiveConfigApplyFailed", "ApplyLiveConfig",
				"CONFIG SET %s=%q failed: %v", key, value, err)
			setCondition(node, valkeyv1alpha1.NodeConditionLiveConfigApplied, metav1.ConditionFalse, reasonApplyFailed,
				fmt.Sprintf("CONFIG SET %s=%q: %v", key, value, err))
			return fmt.Errorf("apply live config %s: %w", key, err)
		}
	}
	setCondition(node, valkeyv1alpha1.NodeConditionLiveConfigApplied, metav1.ConditionTrue, reasonLiveConfigApplied,
		fmt.Sprintf("applied %d live-settable key(s)", len(pending)))
	return nil
}

// isLiveSettableKey reports whether key is in the allowlist (exported intent for
// tests / future cluster controller reuse).
func isLiveSettableKey(key string) bool {
	return slices.Contains(liveSettableKeys(), key)
}
