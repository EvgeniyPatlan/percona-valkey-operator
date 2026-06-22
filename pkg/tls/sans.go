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
	"fmt"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// DNSNames builds the deterministic, fully-deduplicated DNS SAN list for the
// cluster's serving certificate (07 §3.3): the headless Service (short + .ns +
// .ns.svc + .ns.svc.cluster.local FQDN), a wildcard for any pod under the headless
// Service, and an explicit per-pod FQDN for every (shard, node) position so the
// operator's per-pod DNS dial (05 §5) validates against a named SAN even on the
// pod's first boot. Ordering is stable so the rendered Certificate (and thus the
// downstream tlsHash) does not churn across reconciles.
//
// The per-pod workload DNS name is valkey-<cluster>-<shard>-<node> (the
// StatefulSet/Deployment name = naming.NodeWorkloadName(naming.NodeName(...))),
// addressed through the headless Service:
//
//	valkey-<cluster>-<shard>-<node>.valkey-<cluster>.<ns>.svc.cluster.local
func DNSNames(cluster *valkeyv1alpha1.PerconaValkeyCluster) []string {
	svc := naming.HeadlessServiceName(cluster.Name)
	ns := cluster.Namespace

	names := []string{
		svc,
		fmt.Sprintf("%s.%s", svc, ns),
		fmt.Sprintf("%s.%s.svc", svc, ns),
		fmt.Sprintf("%s.%s.svc.cluster.local", svc, ns),
		// Wildcard covers any pod A-record under the headless Service.
		fmt.Sprintf("*.%s.%s.svc", svc, ns),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", svc, ns),
	}

	shards := int(cluster.Spec.Shards)
	replicas := int(cluster.Spec.Replicas)
	for shard := 0; shard < shards; shard++ {
		// node 0 is the initial primary, 1..replicas are the replicas.
		for node := 0; node <= replicas; node++ {
			pod := naming.NodeWorkloadName(naming.NodeName(cluster.Name, shard, node))
			names = append(names,
				fmt.Sprintf("%s.%s.%s.svc", pod, svc, ns),
				fmt.Sprintf("%s.%s.%s.svc.cluster.local", pod, svc, ns),
			)
		}
	}
	return dedupe(names)
}

// dedupe returns the input with duplicates removed, preserving first-seen order
// so the SAN list stays deterministic.
func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
