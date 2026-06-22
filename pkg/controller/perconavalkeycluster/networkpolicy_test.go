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

package perconavalkeycluster_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeycluster"
)

// repoRoot walks up from the test working directory
// (pkg/controller/perconavalkeycluster) to the module root (the dir holding
// go.mod) so the static-manifest tests can read config/ regardless of CWD.
func repoRoot() string {
	dir, err := os.Getwd()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	for i := 0; i < 10; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	ginkgo.Fail("go.mod not found walking up from test dir")
	return ""
}

// readConfig reads a file under the repo's config/ tree.
func readConfig(rel string) []byte {
	b, err := os.ReadFile(filepath.Join(repoRoot(), "config", rel))
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "reading config/%s", rel)
	return b
}

// netpolName mirrors the unexported networkPolicyName builder
// (valkey-<cluster>-netpol) for assertions.
func netpolName(cluster string) string { return "valkey-" + cluster + "-netpol" }

// getNetpol fetches the operator-managed NetworkPolicy for the cluster, or nil
// when absent.
func getNetpol(cluster, ns string) (*networkingv1.NetworkPolicy, bool) {
	np := &networkingv1.NetworkPolicy{}
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: netpolName(cluster), Namespace: ns}, np)
	if err != nil {
		return nil, false
	}
	return np, true
}

// hasPort reports whether the rule's port list contains protocol/port.
func portMatches(ports []networkingv1.NetworkPolicyPort, proto corev1.Protocol, p int32) bool {
	for i := range ports {
		pr := ports[i]
		if pr.Protocol != nil && *pr.Protocol == proto && pr.Port != nil && pr.Port.IntVal == p {
			return true
		}
	}
	return false
}

var _ = ginkgo.Describe("PerconaValkeyCluster NetworkPolicy (M5 GO-5.9)", func() {
	var (
		ns      string
		fc      *fakeCluster
		r       *perconavalkeycluster.Reconciler
		nsIndex int
	)

	ginkgo.BeforeEach(func() {
		nsIndex++
		ns = makeNamespace(fmt.Sprintf("pvk-netpol-%d", nsIndex))
		fc = newFakeCluster()
		r = perconavalkeycluster.NewReconcilerForTest(k8sClient, apiScheme, &fakeClientFactory{fc: fc})
	})

	// reconcileN drives Reconcile n times, promoting nodes between passes so the
	// pipeline advances; phase 2b (NetworkPolicy) runs on the very first pass.
	reconcileN := func(key types.NamespacedName, n int) {
		for i := 0; i < n; i++ {
			_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			promoteNodes(key.Namespace)
		}
	}

	ginkgo.It("creates a default-deny NetworkPolicy with the seven flows when enabled (E9)", func() {
		cluster := makeCluster("np1", ns, 1)
		cluster.Annotations = map[string]string{
			perconavalkeycluster.AnnNetworkPolicyEnabled: "true",
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 3)

		np, ok := getNetpol("np1", ns)
		gomega.Expect(ok).To(gomega.BeTrue(), "NetworkPolicy should exist when enabled")

		// Default-deny posture: both Ingress and Egress policy types present, so an
		// unmatched flow is denied (07 §7).
		gomega.Expect(np.Spec.PolicyTypes).To(gomega.ConsistOf(
			networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))

		// Pod selector is scoped to THIS cluster's Valkey pods.
		gomega.Expect(np.Spec.PodSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"valkey.percona.com/cluster", "np1"))
		gomega.Expect(np.Spec.PodSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"valkey.percona.com/component", "valkey"))

		// Five ingress rules: client, operator, bus-intra, bus-data-intra, metrics.
		gomega.Expect(np.Spec.Ingress).To(gomega.HaveLen(5))
		// Two egress rules: storage 443, DNS 53.
		gomega.Expect(np.Spec.Egress).To(gomega.HaveLen(2))

		// client-ingress 6379 (any pod in ns).
		gomega.Expect(portMatches(np.Spec.Ingress[0].Ports, corev1.ProtocolTCP, 6379)).To(gomega.BeTrue())
		gomega.Expect(np.Spec.Ingress[0].From[0].PodSelector).NotTo(gomega.BeNil())

		// operator-ingress 6379 from the operator pod.
		gomega.Expect(portMatches(np.Spec.Ingress[1].Ports, corev1.ProtocolTCP, 6379)).To(gomega.BeTrue())
		gomega.Expect(np.Spec.Ingress[1].From[0].PodSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"app.kubernetes.io/name", "valkey-operator"))

		// bus-intra 16379 scoped by the cluster label (cross-cluster MEET block, B4).
		gomega.Expect(portMatches(np.Spec.Ingress[2].Ports, corev1.ProtocolTCP, 16379)).To(gomega.BeTrue())
		gomega.Expect(np.Spec.Ingress[2].From[0].PodSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"valkey.percona.com/cluster", "np1"))

		// bus-data-intra 6379 (replication / slot migration) same cluster.
		gomega.Expect(portMatches(np.Spec.Ingress[3].Ports, corev1.ProtocolTCP, 6379)).To(gomega.BeTrue())
		gomega.Expect(np.Spec.Ingress[3].From[0].PodSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"valkey.percona.com/cluster", "np1"))

		// metrics-ingress 9121 from the monitoring namespace's Prometheus pods.
		gomega.Expect(portMatches(np.Spec.Ingress[4].Ports, corev1.ProtocolTCP, 9121)).To(gomega.BeTrue())
		gomega.Expect(np.Spec.Ingress[4].From[0].PodSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"app.kubernetes.io/name", "prometheus"))
		gomega.Expect(np.Spec.Ingress[4].From[0].NamespaceSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"kubernetes.io/metadata.name", ns))

		// storage-egress 443.
		gomega.Expect(portMatches(np.Spec.Egress[0].Ports, corev1.ProtocolTCP, 443)).To(gomega.BeTrue())
		// dns-egress 53 (both UDP and TCP).
		gomega.Expect(portMatches(np.Spec.Egress[1].Ports, corev1.ProtocolUDP, 53)).To(gomega.BeTrue())
		gomega.Expect(portMatches(np.Spec.Egress[1].Ports, corev1.ProtocolTCP, 53)).To(gomega.BeTrue())

		// Owner reference back to the cluster so it is GC'd with the cluster.
		gomega.Expect(np.OwnerReferences).To(gomega.HaveLen(1))
		gomega.Expect(np.OwnerReferences[0].Kind).To(gomega.Equal("PerconaValkeyCluster"))
		gomega.Expect(np.OwnerReferences[0].Name).To(gomega.Equal("np1"))
	})

	ginkgo.It("honours the monitoring-namespace override on the metrics rule", func() {
		cluster := makeCluster("np2", ns, 1)
		cluster.Annotations = map[string]string{
			perconavalkeycluster.AnnNetworkPolicyEnabled:             "true",
			perconavalkeycluster.AnnNetworkPolicyMonitoringNamespace: "observability",
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)

		np, ok := getNetpol("np2", ns)
		gomega.Expect(ok).To(gomega.BeTrue())
		gomega.Expect(np.Spec.Ingress[4].From[0].NamespaceSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"kubernetes.io/metadata.name", "observability"))
	})

	ginkgo.It("is a no-op when network policy is not enabled (no orphaned policy)", func() {
		cluster := makeCluster("np3", ns, 1)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 3)

		_, ok := getNetpol("np3", ns)
		gomega.Expect(ok).To(gomega.BeFalse(), "no NetworkPolicy when disabled")
	})

	ginkgo.It("is idempotent: repeated reconciles re-patch the same policy (stable identity)", func() {
		cluster := makeCluster("np4", ns, 1)
		cluster.Annotations = map[string]string{
			perconavalkeycluster.AnnNetworkPolicyEnabled: "true",
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)
		first, ok := getNetpol("np4", ns)
		gomega.Expect(ok).To(gomega.BeTrue())
		firstUID := first.UID
		firstRV := first.ResourceVersion

		// Several more passes must not recreate or churn the object.
		reconcileN(key, 4)
		second, ok := getNetpol("np4", ns)
		gomega.Expect(ok).To(gomega.BeTrue())
		gomega.Expect(second.UID).To(gomega.Equal(firstUID), "policy must not be recreated")
		// CreateOrUpdate writes only on a real diff; an idempotent body leaves the
		// resourceVersion unchanged after the initial create+settle.
		gomega.Expect(second.ResourceVersion).To(gomega.Equal(firstRV),
			"idempotent reconcile must not churn the policy")
		gomega.Expect(second.Spec.Ingress).To(gomega.HaveLen(5))
		gomega.Expect(second.Spec.Egress).To(gomega.HaveLen(2))
	})

	ginkgo.It("deletes the policy when the enable annotation is removed", func() {
		cluster := makeCluster("np5", ns, 1)
		cluster.Annotations = map[string]string{
			perconavalkeycluster.AnnNetworkPolicyEnabled: "true",
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)
		_, ok := getNetpol("np5", ns)
		gomega.Expect(ok).To(gomega.BeTrue())

		// Flip the gate off and reconcile: the policy must be removed.
		fresh := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, key, fresh)).To(gomega.Succeed())
		fresh.Annotations[perconavalkeycluster.AnnNetworkPolicyEnabled] = "false"
		gomega.Expect(k8sClient.Update(testCtx, fresh)).To(gomega.Succeed())

		reconcileN(key, 2)
		gomega.Eventually(func() bool {
			_, present := getNetpol("np5", ns)
			return present
		}, timeout, interval).Should(gomega.BeFalse(), "policy must be deleted when disabled")
	})

	ginkgo.It("matches the expected name (valkey-<cluster>-netpol) and component labels", func() {
		cluster := makeCluster("np6", ns, 1)
		cluster.Annotations = map[string]string{
			perconavalkeycluster.AnnNetworkPolicyEnabled: "true",
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)
		np := &networkingv1.NetworkPolicy{}
		gomega.Expect(k8sClient.Get(testCtx,
			types.NamespacedName{Name: "valkey-np6-netpol", Namespace: ns}, np)).To(gomega.Succeed())
		gomega.Expect(np.Labels).To(gomega.HaveKeyWithValue("app.kubernetes.io/managed-by", "percona-valkey-operator"))
		gomega.Expect(np.Labels).To(gomega.HaveKeyWithValue("valkey.percona.com/component", "valkey"))
	})

	ginkgo.It("creates the policy from spec.networkPolicy.enabled=true (first-class field, no annotation)", func() {
		cluster := makeCluster("np7", ns, 1)
		enabled := true
		cluster.Spec.NetworkPolicy = &valkeyv1alpha1.NetworkPolicySpec{Enabled: &enabled}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)
		np, ok := getNetpol("np7", ns)
		gomega.Expect(ok).To(gomega.BeTrue(), "spec.networkPolicy.enabled=true must create the policy")
		gomega.Expect(np.Spec.Ingress).To(gomega.HaveLen(5))
		// Default client-ingress: same-namespace pods (empty pod selector, no ns selector).
		gomega.Expect(np.Spec.Ingress[0].From).To(gomega.HaveLen(1))
		gomega.Expect(np.Spec.Ingress[0].From[0].PodSelector).NotTo(gomega.BeNil())
		gomega.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector).To(gomega.BeNil())
	})

	ginkgo.It("does not create the policy when spec.networkPolicy.enabled=false (overrides the annotation)", func() {
		cluster := makeCluster("np8", ns, 1)
		// Annotation says enabled, but the first-class field explicitly disables.
		cluster.Annotations = map[string]string{perconavalkeycluster.AnnNetworkPolicyEnabled: "true"}
		disabled := false
		cluster.Spec.NetworkPolicy = &valkeyv1alpha1.NetworkPolicySpec{Enabled: &disabled}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 3)
		_, ok := getNetpol("np8", ns)
		gomega.Expect(ok).To(gomega.BeFalse(), "explicit spec.networkPolicy.enabled=false must win over the annotation")
	})

	ginkgo.It("scopes client-ingress to spec.networkPolicy.clientNamespaceSelectors", func() {
		cluster := makeCluster("np9", ns, 1)
		enabled := true
		cluster.Spec.NetworkPolicy = &valkeyv1alpha1.NetworkPolicySpec{
			Enabled: &enabled,
			ClientNamespaceSelectors: []metav1.LabelSelector{
				{MatchLabels: map[string]string{"team": "payments"}},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)
		np, ok := getNetpol("np9", ns)
		gomega.Expect(ok).To(gomega.BeTrue())
		clientRule := np.Spec.Ingress[0]
		gomega.Expect(clientRule.From).To(gomega.HaveLen(1))
		gomega.Expect(clientRule.From[0].NamespaceSelector).NotTo(gomega.BeNil())
		gomega.Expect(clientRule.From[0].NamespaceSelector.MatchLabels).To(gomega.HaveKeyWithValue("team", "payments"))
		gomega.Expect(clientRule.From[0].PodSelector).To(gomega.BeNil())
		gomega.Expect(portMatches(clientRule.Ports, corev1.ProtocolTCP, 6379)).To(gomega.BeTrue())
	})

	ginkgo.It("scopes client-ingress to spec.networkPolicy.clientPodSelectors", func() {
		cluster := makeCluster("np10", ns, 1)
		enabled := true
		cluster.Spec.NetworkPolicy = &valkeyv1alpha1.NetworkPolicySpec{
			Enabled: &enabled,
			ClientPodSelectors: []metav1.LabelSelector{
				{MatchLabels: map[string]string{"app": "api"}},
				{MatchLabels: map[string]string{"app": "worker"}},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)
		np, ok := getNetpol("np10", ns)
		gomega.Expect(ok).To(gomega.BeTrue())
		clientRule := np.Spec.Ingress[0]
		// One peer per pod selector, namespace defaults to the policy's own ns.
		gomega.Expect(clientRule.From).To(gomega.HaveLen(2))
		for _, peer := range clientRule.From {
			gomega.Expect(peer.PodSelector).NotTo(gomega.BeNil())
			gomega.Expect(peer.NamespaceSelector).To(gomega.BeNil())
		}
		gomega.Expect(clientRule.From[0].PodSelector.MatchLabels).To(gomega.HaveKeyWithValue("app", "api"))
		gomega.Expect(clientRule.From[1].PodSelector.MatchLabels).To(gomega.HaveKeyWithValue("app", "worker"))
	})

	ginkgo.It("cross-products client namespace x pod selectors (AND within each peer)", func() {
		cluster := makeCluster("np11", ns, 1)
		enabled := true
		cluster.Spec.NetworkPolicy = &valkeyv1alpha1.NetworkPolicySpec{
			Enabled: &enabled,
			ClientNamespaceSelectors: []metav1.LabelSelector{
				{MatchLabels: map[string]string{"env": "prod"}},
				{MatchLabels: map[string]string{"env": "staging"}},
			},
			ClientPodSelectors: []metav1.LabelSelector{
				{MatchLabels: map[string]string{"app": "api"}},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)
		np, ok := getNetpol("np11", ns)
		gomega.Expect(ok).To(gomega.BeTrue())
		clientRule := np.Spec.Ingress[0]
		// 2 namespaces x 1 pod selector = 2 peers, each carrying BOTH selectors.
		gomega.Expect(clientRule.From).To(gomega.HaveLen(2))
		for _, peer := range clientRule.From {
			gomega.Expect(peer.NamespaceSelector).NotTo(gomega.BeNil())
			gomega.Expect(peer.PodSelector).NotTo(gomega.BeNil())
			gomega.Expect(peer.PodSelector.MatchLabels).To(gomega.HaveKeyWithValue("app", "api"))
		}
	})

	ginkgo.It("honours spec.networkPolicy.monitoringNamespace on the metrics rule", func() {
		cluster := makeCluster("np12", ns, 1)
		enabled := true
		cluster.Spec.NetworkPolicy = &valkeyv1alpha1.NetworkPolicySpec{
			Enabled:             &enabled,
			MonitoringNamespace: "monitoring-prod",
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)

		reconcileN(key, 2)
		np, ok := getNetpol("np12", ns)
		gomega.Expect(ok).To(gomega.BeTrue())
		gomega.Expect(np.Spec.Ingress[4].From[0].NamespaceSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"kubernetes.io/metadata.name", "monitoring-prod"))
		gomega.Expect(np.Spec.Ingress[4].From[0].PodSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"app.kubernetes.io/name", "prometheus"))
	})
})

var _ = ginkgo.Describe("M5 static observability/platform manifests parse (OPS-5.3/5.4)", func() {
	ginkgo.It("config/prometheus/podmonitor.yaml is a valid PodMonitor scraping :metrics at 20s", func() {
		var pm map[string]interface{}
		gomega.Expect(yaml.Unmarshal(readConfig("prometheus/podmonitor.yaml"), &pm)).To(gomega.Succeed())
		gomega.Expect(pm["apiVersion"]).To(gomega.Equal("monitoring.coreos.com/v1"))
		gomega.Expect(pm["kind"]).To(gomega.Equal("PodMonitor"))
		spec := pm["spec"].(map[string]interface{})
		eps := spec["podMetricsEndpoints"].([]interface{})
		gomega.Expect(eps).To(gomega.HaveLen(1))
		ep := eps[0].(map[string]interface{})
		gomega.Expect(ep["port"]).To(gomega.Equal("metrics"))
		gomega.Expect(ep["interval"]).To(gomega.Equal("20s"))
		gomega.Expect(ep["path"]).To(gomega.Equal("/metrics"))
		// Relabels derive cluster/shard_index/node_index/pod (08 §3.2).
		rl := ep["relabelings"].([]interface{})
		targets := map[string]bool{}
		for _, r := range rl {
			targets[r.(map[string]interface{})["targetLabel"].(string)] = true
		}
		for _, want := range []string{"cluster", "shard_index", "node_index", "pod"} {
			gomega.Expect(targets).To(gomega.HaveKey(want))
		}
	})

	ginkgo.It("config/prometheus/podmonitor-tls.yaml uses scheme: https + tlsConfig (08 §3.3)", func() {
		var pm map[string]interface{}
		gomega.Expect(yaml.Unmarshal(readConfig("prometheus/podmonitor-tls.yaml"), &pm)).To(gomega.Succeed())
		ep := pm["spec"].(map[string]interface{})["podMetricsEndpoints"].([]interface{})[0].(map[string]interface{})
		gomega.Expect(ep["scheme"]).To(gomega.Equal("https"))
		gomega.Expect(ep).To(gomega.HaveKey("tlsConfig"))
	})

	ginkgo.It("config/prometheus/prometheusrule.yaml carries the contract alert set", func() {
		var pr map[string]interface{}
		gomega.Expect(yaml.Unmarshal(readConfig("prometheus/prometheusrule.yaml"), &pr)).To(gomega.Succeed())
		gomega.Expect(pr["kind"]).To(gomega.Equal("PrometheusRule"))
		// Collect every alert name.
		alerts := map[string]map[string]interface{}{}
		groups := pr["spec"].(map[string]interface{})["groups"].([]interface{})
		for _, g := range groups {
			for _, ru := range g.(map[string]interface{})["rules"].([]interface{}) {
				rule := ru.(map[string]interface{})
				if name, ok := rule["alert"].(string); ok {
					alerts[name] = rule
				}
			}
		}
		// The frozen M5 contract alert set: primary down, slots unassigned,
		// replication broken, memory pressure, backup failed, cluster not Ready,
		// observedGeneration lag, leader thrash.
		for _, want := range []string{
			"ValkeyPrimaryDownNoQuorum",
			"ValkeySlotsUnassigned",
			"ValkeyReplicationBroken",
			"ValkeyMemoryPressure",
			"ValkeyBackupFailed",
			"ValkeyClusterNotReady",
			"ValkeyObservedGenerationLag",
			"ValkeyLeaderThrash",
		} {
			gomega.Expect(alerts).To(gomega.HaveKey(want), "missing contract alert %s", want)
		}
		// Memory pressure must guard against maxmemory 0 (div-by-zero, 08 §7.4).
		gomega.Expect(alerts["ValkeyMemoryPressure"]["expr"].(string)).To(
			gomega.ContainSubstring("redis_memory_max_bytes > 0"))
		// Replication lag must group by (cluster, shard_index) (08 §7.4).
		gomega.Expect(alerts["ValkeyReplicationLagHigh"]["expr"].(string)).To(
			gomega.ContainSubstring("shard_index"))
		// Slots-unassigned mirrors the 16384 full-coverage requirement.
		gomega.Expect(alerts["ValkeySlotsUnassigned"]["expr"].(string)).To(
			gomega.ContainSubstring("16384"))
	})

	ginkgo.It("config/network-policy/networkpolicy.yaml matches the rendered perimeter shape", func() {
		var np networkingv1.NetworkPolicy
		gomega.Expect(yaml.Unmarshal(readConfig("network-policy/networkpolicy.yaml"), &np)).To(gomega.Succeed())
		gomega.Expect(np.Spec.PolicyTypes).To(gomega.ConsistOf(
			networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		gomega.Expect(np.Spec.Ingress).To(gomega.HaveLen(5))
		gomega.Expect(np.Spec.Egress).To(gomega.HaveLen(2))
		gomega.Expect(np.Spec.PodSelector.MatchLabels).To(gomega.HaveKeyWithValue(
			"valkey.percona.com/component", "valkey"))
	})

	ginkgo.It("config/grafana/valkey-cluster-overview.json parses and never derives role from node_index (08 §2.3)", func() {
		raw := readConfig("grafana/valkey-cluster-overview.json")
		var dash map[string]interface{}
		gomega.Expect(json.Unmarshal(raw, &dash)).To(gomega.Succeed())
		gomega.Expect(dash["title"]).To(gomega.Equal("Valkey / Cluster Overview"))
		gomega.Expect(dash).To(gomega.HaveKey("panels"))
		// The single most common observability mistake: deriving primary/replica
		// from the node_index LABEL inside a panel query. A role panel must read
		// redis_master_link_up / redis_connected_slaves instead. Assert no query
		// keys on node_index as a role discriminator (a relabel target label of
		// node_index is fine; a query selector node_index=... for role is not).
		text := string(raw)
		gomega.Expect(text).To(gomega.ContainSubstring("redis_master_link_up"))
		gomega.Expect(text).NotTo(gomega.ContainSubstring("node_index=\"0\""),
			"role must come from the engine, not node_index==0")
		// Replication-lag panel must group by shard_index.
		gomega.Expect(text).To(gomega.ContainSubstring("by (cluster, shard_index)"))
		gomega.Expect(strings.Count(text, "redis_master_repl_offset")).To(gomega.BeNumerically(">=", 1))
	})

	ginkgo.It("config/rbac aggregated roles do not grant ValkeyNode write access (07 §6.4)", func() {
		raw := readConfig("rbac/perconavalkeycluster_aggregated_roles.yaml")
		// Each YAML doc is a ClusterRole; assert the editor never lists valkeynodes
		// with a write verb.
		docs := strings.Split(string(raw), "\n---")
		var sawEditor bool
		for _, d := range docs {
			if !strings.Contains(d, "percona-valkey-cluster-editor") {
				continue
			}
			sawEditor = true
			var cr map[string]interface{}
			gomega.Expect(yaml.Unmarshal([]byte(d), &cr)).To(gomega.Succeed())
			for _, ru := range cr["rules"].([]interface{}) {
				rule := ru.(map[string]interface{})
				resources := fmt.Sprintf("%v", rule["resources"])
				if strings.Contains(resources, "valkeynodes") {
					ginkgo.Fail("editor role must not reference valkeynodes (internal CR)")
				}
			}
		}
		gomega.Expect(sawEditor).To(gomega.BeTrue(), "editor ClusterRole must be present")
	})
})
