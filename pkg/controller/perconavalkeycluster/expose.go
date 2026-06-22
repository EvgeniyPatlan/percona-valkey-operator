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
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// Expose condition + event reasons (03 §2.12 Service exposure). A render/apply
// failure fails the reconcile with ReasonExposeError; the first creation of an
// external Service emits EventExposeServiceCreated.
const (
	// ReasonExposeError marks a failure rendering or applying one of the
	// operator-managed external (NodePort/LoadBalancer) Services.
	ReasonExposeError = "ExposeError"

	// EventExposeServiceCreated is emitted when an operator-managed external
	// Service is first created.
	EventExposeServiceCreated = "ExposeServiceCreated"
)

// External-Service name suffixes. They are built locally (not in pkg/naming)
// because, like the NetworkPolicy name, this leg alone owns the external-access
// resource names; pkg/naming stays the source of truth for the long-lived
// headless Service / workload names.
const (
	// clientServiceSuffix names the single aggregate external client Service:
	// valkey-<cluster>-client. Used for the non-per-pod NodePort/LoadBalancer case
	// (a load balancer in front of every node, suitable for replication/standalone
	// or a cluster-mode smart client that resolves redirects out of band).
	clientServiceSuffix = "-client"

	// perPodServiceSuffix names each per-pod external Service:
	// valkey-<node>-ext. One Service per ValkeyNode so a cluster-mode client can
	// reach an individual shard directly when following MOVED/ASK redirects.
	perPodServiceSuffix = "-ext"

	// exposeManagedByLabel marks Services this leg created so the cleanup pass can
	// list and prune external Services that are no longer desired (e.g. perPod was
	// turned off, or expose dropped back to ClusterIP) without touching the
	// long-lived headless Service.
	exposeAccessLabel = naming.LabelComponent + "-access"

	// exposeAccessValue is the exposeAccessLabel value stamped on external
	// Services rendered by reconcileExpose.
	exposeAccessValue = "external"
)

// clientServiceName returns the aggregate external client Service name:
// valkey-<cluster>-client.
func clientServiceName(cluster string) string {
	return naming.ResourcePrefix + cluster + clientServiceSuffix
}

// perPodServiceName returns the per-pod external Service name for a node:
// valkey-<node>-ext.
func perPodServiceName(node string) string {
	return naming.ResourcePrefix + node + perPodServiceSuffix
}

// reconcileExpose renders the cluster's external client access per spec.expose
// (03 §2.12). When expose is nil or type ClusterIP the cluster stays reachable
// only via the in-cluster headless Service and any previously-created external
// Services are pruned (no orphans). When type is NodePort/LoadBalancer the
// operator provisions external Service(s):
//
//   - Aggregate mode (perPod=false): one external Service valkey-<cluster>-client
//     selecting every Valkey pod, carrying loadBalancerSourceRanges + annotations.
//   - Per-pod mode (perPod=true, cluster mode): one external Service
//     valkey-<node>-ext per ValkeyNode, each selecting exactly that pod, so a
//     cluster-mode client can reach an individual shard when following MOVED/ASK
//     redirects.
//
// Every external Service carries an owner-ref to the cluster (via
// k8s.CreateOrUpdate) so it is GC'd with the cluster and an owner change
// re-enqueues the parent.
//
// LIMITATION / Integrate seam: per-pod cluster access also needs
// cluster-announce-ip/cluster-announce-port set on each node so the engine
// gossips its EXTERNAL address (the chart's init container performed this
// announce-IP discovery). That announce value is per-node and there is no
// ValkeyNodeSpec field to carry it today, and the config renderer
// (pkg/valkey.RenderServerConfig) emits no announce directive — both are outside
// this leg's file boundary. This leg therefore renders every external Service it
// can and leaves the announce plumbing for the Integrate phase (see the report).
func (r *Reconciler) reconcileExpose(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	expose := cluster.Spec.Expose
	if !exposeExternal(expose) {
		// In-cluster only: prune any external Services left from a prior expose.
		return r.pruneExternalServices(ctx, cluster, nil)
	}

	if expose.PerPod && cluster.Spec.Mode == valkeyv1alpha1.ModeCluster {
		return r.reconcilePerPodServices(ctx, cluster, expose)
	}
	return r.reconcileAggregateService(ctx, cluster, expose)
}

// exposeExternal reports whether spec.expose requests an externally-reachable
// Service (NodePort or LoadBalancer). A nil block or the default ClusterIP keeps
// access in-cluster (headless Service only).
func exposeExternal(expose *valkeyv1alpha1.ExposeSpec) bool {
	if expose == nil {
		return false
	}
	return expose.Type == corev1.ServiceTypeNodePort || expose.Type == corev1.ServiceTypeLoadBalancer
}

// reconcileAggregateService renders the single external client Service
// valkey-<cluster>-client selecting every Valkey pod, then prunes any per-pod
// Services left from a prior perPod=true generation.
func (r *Reconciler) reconcileAggregateService(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, expose *valkeyv1alpha1.ExposeSpec,
) error {
	name := clientServiceName(cluster.Name)
	if err := r.upsertExternalService(ctx, cluster, name, naming.ClusterSelector(cluster.Name), expose); err != nil {
		return err
	}
	// Keep only the aggregate Service; drop any stale per-pod Services.
	return r.pruneExternalServices(ctx, cluster, map[string]struct{}{name: {}})
}

// reconcilePerPodServices renders one external Service per ValkeyNode
// (valkey-<node>-ext), each selecting exactly that pod, then prunes any external
// Service no longer backed by a desired node (and the aggregate Service if a
// prior generation used it).
func (r *Reconciler) reconcilePerPodServices(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, expose *valkeyv1alpha1.ExposeSpec,
) error {
	keep := make(map[string]struct{})
	for _, key := range desiredNodes(cluster) {
		node := naming.NodeName(cluster.Name, key.shard, key.node)
		name := perPodServiceName(node)
		keep[name] = struct{}{}
		if err := r.upsertExternalService(ctx, cluster, name, perPodPodSelector(cluster.Name, key), expose); err != nil {
			return err
		}
	}
	return r.pruneExternalServices(ctx, cluster, keep)
}

// perPodPodSelector matches exactly one ValkeyNode's pod by its cluster + shard +
// node topology labels (the same labels naming.ClusterTopologyLabels stamps), so
// the per-pod external Service routes to a single shard member.
func perPodPodSelector(cluster string, key nodeKey) map[string]string {
	return map[string]string{
		naming.LabelCluster:    cluster,
		naming.LabelShardIndex: strconv.Itoa(key.shard),
		naming.LabelNodeIndex:  strconv.Itoa(key.node),
	}
}

// upsertExternalService creates/updates one external Service of the configured
// type with the given pod selector, carrying the client port (6379) plus, for a
// per-pod cluster Service, the cluster-bus port (16379) so a cluster-mode client
// reaching this address can still gossip-redirect. loadBalancerSourceRanges and
// annotations from spec.expose are applied (sourceRanges only bind for
// LoadBalancer). Owner-ref + create/update via k8s.CreateOrUpdate.
func (r *Reconciler) upsertExternalService(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	name string, selector map[string]string, expose *valkeyv1alpha1.ExposeSpec,
) error {
	svc := &corev1.Service{}
	svc.Name, svc.Namespace = name, cluster.Namespace
	res, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, svc, func() error {
		svc.Labels = exposeServiceLabels(cluster.Name)
		svc.Annotations = mergeExposeAnnotations(svc.Annotations, expose.Annotations)
		svc.Spec.Type = expose.Type
		svc.Spec.Selector = selector
		svc.Spec.Ports = externalServicePorts()
		// loadBalancerSourceRanges is honoured by the cloud LB controller only for
		// type LoadBalancer; clear it otherwise so a type flip does not leave a
		// dangling restriction on a NodePort.
		if expose.Type == corev1.ServiceTypeLoadBalancer {
			svc.Spec.LoadBalancerSourceRanges = expose.LoadBalancerSourceRanges
		} else {
			svc.Spec.LoadBalancerSourceRanges = nil
		}
		return nil
	})
	if err != nil {
		return err
	}
	if res == controllerutil.OperationResultCreated {
		r.recorder.Eventf(cluster, svc, eventNormal, EventExposeServiceCreated,
			"CreateExposeService", "Created external Service %s (%s)", svc.Name, expose.Type)
	}
	return nil
}

// externalServicePorts is the external Service port list: the client port (6379)
// plus the cluster-bus port (16379) so cluster-mode clients reaching an external
// address can still follow gossip/redirects. Charter ports 6379/16379.
func externalServicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "valkey", Port: valkey.ClientPort, TargetPort: intstr.FromInt32(valkey.ClientPort)},
		{Name: "cluster-bus", Port: valkey.BusPort, TargetPort: intstr.FromInt32(valkey.BusPort)},
	}
}

// exposeServiceLabels is the label set on an operator-managed external Service:
// the base component labels plus the access marker the prune pass lists on.
func exposeServiceLabels(cluster string) map[string]string {
	l := naming.Labels(cluster, naming.ComponentValkey)
	l[exposeAccessLabel] = exposeAccessValue
	return l
}

// mergeExposeAnnotations layers the user's spec.expose.annotations onto whatever
// annotations the live Service already carries, returning a NEW map (immutable
// update — never mutate the existing map in place). A nil result is fine when
// there is nothing to set.
func mergeExposeAnnotations(existing, desired map[string]string) map[string]string {
	if len(existing) == 0 && len(desired) == 0 {
		return nil
	}
	out := make(map[string]string, len(existing)+len(desired))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range desired {
		out[k] = v
	}
	return out
}

// pruneExternalServices deletes every operator-managed external Service for this
// cluster (listed by the access marker label) that is not in keep. A nil/empty
// keep set removes them all (expose dropped to in-cluster). NotFound is benign.
func (r *Reconciler) pruneExternalServices(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, keep map[string]struct{},
) error {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			naming.LabelCluster: cluster.Name,
			exposeAccessLabel:   exposeAccessValue,
		}); err != nil {
		return err
	}
	for i := range list.Items {
		svc := &list.Items[i]
		if _, want := keep[svc.Name]; want {
			continue
		}
		if err := r.Delete(ctx, svc); err != nil {
			if delErr := client.IgnoreNotFound(err); delErr != nil {
				return delErr
			}
		}
	}
	return nil
}
