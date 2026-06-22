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

package perconavalkeycluster

import (
	"context"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// getValkeyClusterState dials every node with a non-empty status.podIP via the
// injected ClusterClientFactory (ForceSingleClient + _operator auth + TLS,
// resolved by the factory), scrapes one CLUSTER batch per node and assembles a
// ClusterState. Nodes without a podIP are skipped; a single unreachable node is
// tolerated (its scrape error is logged, not fatal) so one dead node never
// blinds the operator. Returns nil when no node could be scraped yet (the
// caller waits). The caller must defer state.CloseClients(). 04 §2.1 step7 /
// GO-3.11. (The operator-credential resolution lives inside the injected
// ClusterClientFactory.ForNode, so this function needs no cluster argument.)
func (r *Reconciler) getValkeyClusterState(
	ctx context.Context, nodes *valkeyv1alpha1.ValkeyNodeList,
) *valkey.ClusterState {
	log := logf.FromContext(ctx)

	scraped := make([]*valkey.NodeState, 0, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Status.PodIP == "" {
			continue // not ready yet; skip.
		}
		addr, c, err := r.clientFactory.ForNode(ctx, node)
		if err != nil {
			log.V(1).Info("dial node failed, skipping", "node", node.Name, "err", err.Error())
			continue
		}
		ns, err := valkey.ScrapeNode(ctx, addr, c)
		if err != nil {
			log.V(1).Info("scrape node failed, skipping", "node", node.Name, "err", err.Error())
			_ = c.Close()
			continue
		}
		scraped = append(scraped, ns)
	}

	if len(scraped) == 0 {
		return nil
	}
	return valkey.NewClusterState(scraped)
}
