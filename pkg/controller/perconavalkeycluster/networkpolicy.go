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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// NetworkPolicy condition reason (07 §7). Owned by the NetworkPolicy leg
// (M5 GO-5.9); a render/apply failure fails the reconcile with this reason.
const (
	// ReasonNetworkPolicyError marks a failure rendering or applying one of the
	// operator-managed NetworkPolicies.
	ReasonNetworkPolicyError = "NetworkPolicyError"
)

// NetworkPolicy event reason (07 §7). Owned by the NetworkPolicy leg.
const (
	// EventNetworkPolicyCreated is emitted when an operator-managed NetworkPolicy
	// is first created.
	EventNetworkPolicyCreated = "NetworkPolicyCreated"
)

// metricsPort is the exporter scrape port (Charter / 08 §3.4: 9121). The client
// (6379) and cluster-bus (16379) ports come from pkg/valkey.
const metricsPort int32 = 9121

// NetworkPolicy gating + selector configuration.
//
// The first-class spec.networkPolicy block (enabled / clientNamespaceSelectors /
// clientPodSelectors / monitoringNamespace) is the source of truth (07 §7). The
// pre-API interim annotation gate is retained ONLY as a back-compat fallback for
// clusters that set it before the field landed: spec.networkPolicy takes
// precedence, and absent both the reconcile is a no-op so no orphaned policies
// are left behind (07 §7: opt-in, recommended true in production).
const (
	// AnnNetworkPolicyEnabled, when set to "true" on the cluster CR, turns on the
	// operator-managed default-deny perimeter. Back-compat fallback for
	// spec.networkPolicy.enabled (07 §7).
	AnnNetworkPolicyEnabled = "valkey.percona.com/network-policy"

	// AnnNetworkPolicyMonitoringNamespace overrides the namespace whose Prometheus
	// pods may scrape the exporter (08 §3.4). Empty => same namespace as the
	// cluster. Back-compat fallback for spec.networkPolicy.monitoringNamespace.
	AnnNetworkPolicyMonitoringNamespace = "valkey.percona.com/network-policy-monitoring-namespace"

	// annEnabledValue is the truthy value AnnNetworkPolicyEnabled must carry.
	annEnabledValue = "true"

	// operatorPodNameLabelValue is the app.kubernetes.io/name value the operator
	// pod carries; operator-ingress on 6379 is scoped to it (07 §7).
	operatorPodNameLabelValue = "valkey-operator"

	// prometheusPodNameLabelValue is the app.kubernetes.io/name value a Prometheus
	// pod carries; metrics-ingress on 9121 is scoped to it (08 §3.4).
	prometheusPodNameLabelValue = "prometheus"

	// metadataNameLabel is the well-known namespace label kube-apiserver stamps
	// (kubernetes.io/metadata.name == <namespace>), used to scope the
	// monitoring-namespace selector (08 §3.4).
	metadataNameLabel = "kubernetes.io/metadata.name"

	// networkPolicySuffix is appended to the cluster name for the operator-managed
	// NetworkPolicy (valkey-<cluster>-netpol). It is built locally rather than in
	// pkg/naming because the name builder is owned by this leg alone.
	networkPolicySuffix = "-netpol"
)

// networkPolicyName returns the operator-managed NetworkPolicy name:
// valkey-<cluster>-netpol (07 §7; one perimeter policy carries the data-plane
// flows plus the metrics rule from 08 §3.4).
func networkPolicyName(cluster string) string {
	return naming.ResourcePrefix + cluster + networkPolicySuffix
}

// reconcileNetworkPolicy renders the cluster's default-deny NetworkPolicy plus
// exactly the required flows (07 §7 + the metrics rule in 08 §3.4) when enabled,
// and removes it when disabled (no orphaned policies). It is dispatched in
// reconcileInfra alongside the Service/PDB infra — the data-plane perimeter is
// part of bringing the cluster's infra up.
//
// Flows (all scoped to this cluster's pods via the valkey.percona.com/cluster
// podSelector):
//   - Ingress client 6379 from spec.networkPolicy.clientNamespaceSelectors /
//     clientPodSelectors (default: any pod in the cluster namespace when neither
//     is set).
//   - Ingress operator 6379 from the operator pod (app.kubernetes.io/name=valkey-operator).
//   - Ingress bus-intra 16379 from same-cluster pods (blocks cross-cluster
//     CLUSTER MEET spoofing, B4).
//   - Ingress bus-data-intra 6379 from same-cluster pods (replication + slot
//     migration).
//   - Ingress metrics 9121 from the monitoring namespace's Prometheus pods.
//   - Egress storage 443 (backup upload) and DNS 53 (UDP/TCP).
//
// When network policy is disabled this is a no-op delete; the policy carries an
// owner-ref to the cluster (k8s.CreateOrUpdate) so it is GC'd with the cluster.
func (r *Reconciler) reconcileNetworkPolicy(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	if !networkPolicyEnabled(cluster) {
		return r.deleteNetworkPolicy(ctx, cluster)
	}

	np := &networkingv1.NetworkPolicy{}
	np.Name, np.Namespace = networkPolicyName(cluster.Name), cluster.Namespace
	res, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, np, func() error {
		np.Labels = naming.Labels(cluster.Name, naming.ComponentValkey)
		np.Spec = buildNetworkPolicySpec(cluster)
		return nil
	})
	if err != nil {
		return err
	}
	if res == controllerutil.OperationResultCreated {
		r.recorder.Eventf(cluster, np, eventNormal, EventNetworkPolicyCreated,
			"CreateNetworkPolicy", "Created NetworkPolicy %s", np.Name)
	}
	return nil
}

// deleteNetworkPolicy removes the operator-managed NetworkPolicy (policy
// disabled). A NotFound is benign.
func (r *Reconciler) deleteNetworkPolicy(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	np := &networkingv1.NetworkPolicy{}
	np.Name, np.Namespace = networkPolicyName(cluster.Name), cluster.Namespace
	if err := r.Delete(ctx, np); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// networkPolicyEnabled reports whether the operator-managed perimeter is on for
// this cluster. The first-class spec.networkPolicy.enabled field (the API leg)
// takes precedence; absent that block the OQ-3 interim annotation gate is the
// fallback (opt-in either way, so the M1 minimal cluster stays policy-free).
func networkPolicyEnabled(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	if np := cluster.Spec.NetworkPolicy; np != nil && np.Enabled != nil {
		return *np.Enabled
	}
	return cluster.Annotations[AnnNetworkPolicyEnabled] == annEnabledValue
}

// monitoringNamespace returns the namespace whose Prometheus pods may scrape the
// exporter, defaulting to the cluster's own namespace (08 §3.4). The first-class
// spec.networkPolicy.monitoringNamespace field takes precedence over the interim
// annotation.
func monitoringNamespace(cluster *valkeyv1alpha1.PerconaValkeyCluster) string {
	if np := cluster.Spec.NetworkPolicy; np != nil && np.MonitoringNamespace != "" {
		return np.MonitoringNamespace
	}
	if ns := cluster.Annotations[AnnNetworkPolicyMonitoringNamespace]; ns != "" {
		return ns
	}
	return cluster.Namespace
}

// buildNetworkPolicySpec assembles the default-deny + allowed-flow spec. The
// podSelector matches only this cluster's Valkey pods; PolicyTypes lists both
// Ingress and Egress so an empty rule list would default-deny — every flow below
// is therefore an explicit allow on top of that deny baseline (07 §7).
func buildNetworkPolicySpec(cluster *valkeyv1alpha1.PerconaValkeyCluster) networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: clusterValkeyPodSelector(cluster.Name)},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
		Ingress: buildIngressRules(cluster),
		Egress:  buildEgressRules(),
	}
}

// clusterValkeyPodSelector matches this cluster's Valkey pods (cluster label +
// component=valkey), the scope every rule is anchored to.
func clusterValkeyPodSelector(cluster string) map[string]string {
	return map[string]string{
		naming.LabelCluster:   cluster,
		naming.LabelComponent: naming.ComponentValkey,
	}
}

// buildIngressRules renders the five ingress flows (07 §7 + 08 §3.4).
func buildIngressRules(cluster *valkeyv1alpha1.PerconaValkeyCluster) []networkingv1.NetworkPolicyIngressRule {
	sameCluster := []networkingv1.NetworkPolicyPeer{podPeer(naming.ClusterSelector(cluster.Name))}
	return []networkingv1.NetworkPolicyIngressRule{
		// client-ingress 6379: application data access, scoped to the namespaces /
		// pods named by spec.networkPolicy.clientNamespaceSelectors and
		// clientPodSelectors (default: any pod in the cluster namespace, 07 §7).
		{
			From:  clientIngressPeers(cluster),
			Ports: tcpPorts(valkey.ClientPort),
		},
		// operator-ingress 6379: _operator orchestration from the operator pod.
		{
			From:  []networkingv1.NetworkPolicyPeer{podPeer(operatorPodSelector())},
			Ports: tcpPorts(valkey.ClientPort),
		},
		// bus-intra 16379: gossip/heartbeat/failover coordination, scoped by the
		// cluster label to block cross-cluster CLUSTER MEET spoofing (B4).
		{
			From:  sameCluster,
			Ports: tcpPorts(valkey.BusPort),
		},
		// bus-data-intra 6379: replication stream + slot migration between nodes.
		{
			From:  sameCluster,
			Ports: tcpPorts(valkey.ClientPort),
		},
		// metrics-ingress 9121: Prometheus scrape from the monitoring namespace.
		{
			From:  []networkingv1.NetworkPolicyPeer{prometheusPeer(monitoringNamespace(cluster))},
			Ports: tcpPorts(metricsPort),
		},
	}
}

// buildEgressRules renders the two egress flows (07 §7): object-storage upload
// (443) and DNS (53 UDP+TCP). Peers are left open (no IPBlock) because the
// storage endpoint and kube-dns address vary per platform; the ports constrain
// the flow and the default-deny on every other port/peer still applies.
func buildEgressRules() []networkingv1.NetworkPolicyEgressRule {
	return []networkingv1.NetworkPolicyEgressRule{
		// storage-egress 443: backup RDB upload to S3/GCS/Azure (backup Jobs).
		{Ports: tcpPorts(443)},
		// dns-egress 53: name resolution (UDP and TCP).
		{Ports: []networkingv1.NetworkPolicyPort{
			port(corev1.ProtocolUDP, 53),
			port(corev1.ProtocolTCP, 53),
		}},
	}
}

// clientIngressPeers builds the client-ingress (6379) peer list from
// spec.networkPolicy.clientNamespaceSelectors and clientPodSelectors (07 §7):
//
//   - neither set => a single peer matching any pod in the policy's own namespace
//     (the same-namespace default; preserves the pre-field behaviour).
//   - only namespace selectors => one peer per selector matching every pod in the
//     selected namespaces.
//   - only pod selectors => one peer per selector matching those pods in the
//     policy's own namespace.
//   - both set => the cross-product: a peer per (namespace, pod) selector pair
//     requiring BOTH to match (a NetworkPolicyPeer with namespace+pod selectors is
//     a logical AND), so only the named pods in the named namespaces are allowed.
func clientIngressPeers(cluster *valkeyv1alpha1.PerconaValkeyCluster) []networkingv1.NetworkPolicyPeer {
	var nsSelectors, podSelectors []metav1.LabelSelector
	if np := cluster.Spec.NetworkPolicy; np != nil {
		nsSelectors = np.ClientNamespaceSelectors
		podSelectors = np.ClientPodSelectors
	}

	switch {
	case len(nsSelectors) == 0 && len(podSelectors) == 0:
		return []networkingv1.NetworkPolicyPeer{podPeer(nil)}
	case len(nsSelectors) == 0:
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(podSelectors))
		for i := range podSelectors {
			peers = append(peers, networkingv1.NetworkPolicyPeer{PodSelector: podSelectors[i].DeepCopy()})
		}
		return peers
	case len(podSelectors) == 0:
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(nsSelectors))
		for i := range nsSelectors {
			peers = append(peers, networkingv1.NetworkPolicyPeer{NamespaceSelector: nsSelectors[i].DeepCopy()})
		}
		return peers
	default:
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(nsSelectors)*len(podSelectors))
		for i := range nsSelectors {
			for j := range podSelectors {
				peers = append(peers, networkingv1.NetworkPolicyPeer{
					NamespaceSelector: nsSelectors[i].DeepCopy(),
					PodSelector:       podSelectors[j].DeepCopy(),
				})
			}
		}
		return peers
	}
}

// podPeer builds an ingress peer matching pods by label (nil => any pod in the
// policy's namespace).
func podPeer(podLabels map[string]string) networkingv1.NetworkPolicyPeer {
	sel := &metav1.LabelSelector{}
	if podLabels != nil {
		sel.MatchLabels = podLabels
	}
	return networkingv1.NetworkPolicyPeer{PodSelector: sel}
}

// prometheusPeer builds the metrics-ingress peer: Prometheus pods in the
// configured monitoring namespace (08 §3.4).
func prometheusPeer(monitoringNS string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{metadataNameLabel: monitoringNS},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{naming.LabelAppName: prometheusPodNameLabelValue},
		},
	}
}

// operatorPodSelector matches the operator pod (app.kubernetes.io/name=valkey-operator).
func operatorPodSelector() map[string]string {
	return map[string]string{naming.LabelAppName: operatorPodNameLabelValue}
}

// tcpPorts builds a single-TCP-port rule list.
func tcpPorts(p int32) []networkingv1.NetworkPolicyPort {
	return []networkingv1.NetworkPolicyPort{port(corev1.ProtocolTCP, p)}
}

// port builds one NetworkPolicyPort for the given protocol/port.
func port(proto corev1.Protocol, p int32) networkingv1.NetworkPolicyPort {
	pr := proto
	tp := intstr.FromInt32(p)
	return networkingv1.NetworkPolicyPort{Protocol: &pr, Port: &tp}
}
