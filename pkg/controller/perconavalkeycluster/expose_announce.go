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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// This file is the expose-announce SEAM (CR / gap §2.12 per-pod cluster access).
// reconcileExpose (expose.go) provisions the per-pod EXTERNAL Services; this seam
// owns the SECOND half of per-pod cluster access: discovering each pod's external
// address (the per-pod NodePort/LoadBalancer ingress) and propagating it onto each
// ValkeyNode's spec.announceHost/announcePort so the engine gossips its EXTERNAL
// address via --cluster-announce-ip/--cluster-announce-port (rendered in
// valkeynode_resources.go announceArgs). Without the announce wiring a cluster-mode
// client that reaches a per-pod external Service still receives the in-cluster
// POD_IP in MOVED/ASK redirects and cannot follow them.
//
// The FOUNDATION provides the disjoint stub plus the per-node announce accessor
// (announceForNode) that buildValkeyNodeSpec calls; this expose-announce LEG fills
// the real discovery (reading the per-pod Service's NodePort / LoadBalancer ingress
// and PATCHing the resolved external host/port onto each existing ValkeyNode)
// without touching the FOUNDATION's files.
//
// Why a PATCH onto the live ValkeyNode rather than the accessor: announceForNode is
// a pure accessor with no cluster status to read the discovered external ingress
// from (the in-mesh FQDN it returns is buildValkeyNodeSpec's create/roll-time
// default). The external NodePort/LoadBalancer address is only knowable after the
// Service is admitted, so this leg resolves it from live state and persists it onto
// each node via a MergeFrom PATCH that REFINES the FQDN default. Crucially
// nodeNeedsRoll only compares serverConfigHash/image (nodes.go), so refining the
// announce host/port never triggers a rolling restart — the node controller simply
// re-renders --cluster-announce-ip on the node's next own reconcile. The refinement
// is idempotent: a later create/roll re-stamps the FQDN default and the next
// expose-announce pass re-refines it, converging without churn.

// reconcileExposeAnnounce reconciles the per-pod cluster-announce wiring once the
// per-pod external Services exist: for each ValkeyNode it resolves the pod's
// external address (LoadBalancer .status.loadBalancer.ingress, or a NodePort node
// IP + nodePort) from its per-pod external Service and PATCHes the matching
// ValkeyNode's spec.announceHost/announcePort so the engine re-announces the
// EXTERNAL address. It is a no-op unless expose.perPod is requested in cluster mode
// (announceWanted). A node whose external address is still pending (no LoadBalancer
// ingress yet) is left unchanged and NOT errored: the cluster Owns the per-pod
// Services, so the cloud LB controller populating .status.loadBalancer.ingress
// re-enqueues the cluster and this pass resolves it then (the requeue-while-pending
// contract, surfaced through the owned-Service watch rather than this error-only
// signature).
func (r *Reconciler) reconcileExposeAnnounce(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	if !announceWanted(cluster) {
		return nil
	}
	log := logf.FromContext(ctx)

	nodes, err := r.listClusterNodes(ctx, cluster)
	if err != nil {
		return fmt.Errorf("list nodes for expose-announce: %w", err)
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		host, port, err := r.resolveExternalAnnounce(ctx, cluster, node)
		if err != nil {
			return err
		}
		if host == "" {
			// External address still pending (e.g. LoadBalancer ingress not yet
			// assigned). Leave the node on its current announce value; the owned
			// per-pod Service status update re-enqueues the cluster when the address
			// lands. No error => no fail/requeue storm, just a clean wait.
			log.V(1).Info("per-pod external address pending; deferring announce", "node", node.Name)
			continue
		}
		if err := r.patchNodeAnnounce(ctx, node, host, port); err != nil {
			return err
		}
	}
	return nil
}

// resolveExternalAnnounce resolves the external announce host+port a ValkeyNode
// should advertise from its per-pod external Service. It returns ("", nil, nil)
// when the per-pod Service does not exist yet or its external address is still
// pending (a LoadBalancer without ingress, or a NodePort whose backing pod has no
// node IP yet) — the caller treats that as "defer, requeue via the owned-Service
// watch", not an error.
func (r *Reconciler) resolveExternalAnnounce(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, node *valkeyv1alpha1.ValkeyNode,
) (host string, port *int32, err error) {
	svc := &corev1.Service{}
	name := perPodServiceName(node.Name)
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, svc); getErr != nil {
		if client.IgnoreNotFound(getErr) != nil {
			return "", nil, fmt.Errorf("get per-pod service %s: %w", name, getErr)
		}
		// Service not created yet (reconcileExpose runs in the same pass; the node
		// may predate this generation's Services): defer.
		return "", nil, nil
	}

	switch svc.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		host, port = loadBalancerAnnounce(svc)
		return host, port, nil
	case corev1.ServiceTypeNodePort:
		host, port, err = r.nodePortAnnounce(ctx, cluster, node, svc)
		return host, port, err
	default:
		// The Service flipped away from an external type (or was never external):
		// nothing to announce externally.
		return "", nil, nil
	}
}

// loadBalancerAnnounce extracts the external announce host+port from a
// LoadBalancer Service: the first .status.loadBalancer.ingress entry's IP (or
// hostname) and the published client port. Returns ("", nil) while the ingress is
// still pending so the caller defers.
func loadBalancerAnnounce(svc *corev1.Service) (host string, port *int32) {
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			host = ing.IP
			break
		}
		if ing.Hostname != "" {
			host = ing.Hostname
			break
		}
	}
	if host == "" {
		return "", nil
	}
	return host, servicePublishedClientPort(svc)
}

// nodePortAnnounce extracts the external announce host+port for a NodePort
// Service: the backing pod's node IP (status.hostIP — the cluster controller has
// pods RBAC but not nodes RBAC, so the node address is read from the pod) and the
// assigned client nodePort. Returns ("", nil) while the backing pod or its node IP
// or the nodePort assignment is still pending so the caller defers.
func (r *Reconciler) nodePortAnnounce(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
	node *valkeyv1alpha1.ValkeyNode, svc *corev1.Service,
) (host string, port *int32, err error) {
	nodePort := serviceClientNodePort(svc)
	if nodePort == nil {
		return "", nil, nil // nodePort not assigned yet: defer.
	}
	hostIP, err := r.podNodeIP(ctx, cluster, node)
	if err != nil {
		return "", nil, err
	}
	if hostIP == "" {
		return "", nil, nil // backing pod / its node IP not ready yet: defer.
	}
	return hostIP, nodePort, nil
}

// podNodeIP returns the node (host) IP of the single pod backing a ValkeyNode,
// matched by the per-pod topology selector (cluster + shard + node labels — the
// same selector the per-pod external Service routes on). Empty when no pod exists
// yet or it has not been scheduled to a node (status.hostIP unset). Uses the
// cluster controller's pods RBAC, avoiding a nodes List the controller is not
// granted.
func (r *Reconciler) podNodeIP(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, node *valkeyv1alpha1.ValkeyNode,
) (string, error) {
	key := nodeKeyOf(node)
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(perPodPodSelector(cluster.Name, key))); err != nil {
		return "", fmt.Errorf("list backing pod for %s: %w", node.Name, err)
	}
	for i := range pods.Items {
		if ip := pods.Items[i].Status.HostIP; ip != "" {
			return ip, nil
		}
	}
	return "", nil
}

// patchNodeAnnounce persists the resolved external announce host+port onto a
// ValkeyNode via a MergeFrom PATCH of a COPY (never a full Update), refining the
// FQDN default buildValkeyNodeSpec stamped. It is a no-op when the node already
// carries the resolved value (only-if-differs, so a steady pass writes nothing).
// nodeNeedsRoll ignores announce fields, so this refinement never triggers a roll.
func (r *Reconciler) patchNodeAnnounce(
	ctx context.Context, node *valkeyv1alpha1.ValkeyNode, host string, port *int32,
) error {
	if node.Spec.AnnounceHost == host && portsEqual(node.Spec.AnnouncePort, port) {
		return nil
	}
	base := node.DeepCopy()
	patchTarget := node.DeepCopy()
	patchTarget.Spec.AnnounceHost = host
	patchTarget.Spec.AnnouncePort = port
	if err := r.Patch(ctx, patchTarget, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch announce on %s: %w", node.Name, err)
	}
	// Mirror onto the in-memory object so a later read in the same pass is consistent.
	node.ResourceVersion = patchTarget.ResourceVersion
	node.Spec.AnnounceHost = host
	node.Spec.AnnouncePort = port
	return nil
}

// servicePublishedClientPort returns a copy of the external Service's published
// client port (the .spec.ports[name=valkey] Port), defaulting to the charter
// client port (6379) when the named port is absent.
func servicePublishedClientPort(svc *corev1.Service) *int32 {
	clientPort := int32(valkey.ClientPort)
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Name == valkeyPortName {
			clientPort = svc.Spec.Ports[i].Port
			break
		}
	}
	return &clientPort
}

// serviceClientNodePort returns a copy of the assigned client nodePort
// (.spec.ports[name=valkey].nodePort), or nil while it is still unassigned (0).
func serviceClientNodePort(svc *corev1.Service) *int32 {
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Name != valkeyPortName {
			continue
		}
		if svc.Spec.Ports[i].NodePort == 0 {
			return nil
		}
		np := svc.Spec.Ports[i].NodePort
		return &np
	}
	return nil
}

// valkeyPortName is the name of the client port on the external Service (matches
// externalServicePorts in expose.go).
const valkeyPortName = "valkey"

// portsEqual reports whether two optional ports are equal (both nil, or both set
// to the same value).
func portsEqual(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// announceWanted reports whether per-pod cluster-announce wiring is requested: an
// external (NodePort/LoadBalancer) expose with perPod set, in cluster mode. The
// announce address only matters for cluster-mode redirects, so non-cluster /
// non-per-pod exposes never announce an external address.
func announceWanted(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	expose := cluster.Spec.Expose
	if expose == nil || !expose.PerPod {
		return false
	}
	return cluster.Spec.Mode == valkeyv1alpha1.ModeCluster && exposeExternal(expose)
}

// announceForNode returns the announce host/port to stamp onto a node's
// spec.announceHost/announcePort for a (shard, node) position, or ("", nil) when no
// external address is announced (the node then falls back to the in-cluster POD_IP
// in announceArgs). When per-pod cluster access is wanted this returns the node's
// per-pod external Service FQDN on the client port as the create/roll-time announce
// default — a real, node-specific, in-mesh-resolvable address so a smart cluster
// client can follow MOVED/ASK redirects to the right pod immediately even before
// the external ingress is discovered. reconcileExposeAnnounce then REFINES the host
// to the resolved EXTERNAL ingress (NodePort node IP / LoadBalancer ingress) for
// out-of-mesh clients via a MergeFrom PATCH. Kept a pure accessor (no receiver
// state) so it is wired into buildValkeyNodeSpec deterministically.
func announceForNode(cluster *valkeyv1alpha1.PerconaValkeyCluster, key nodeKey) (host string, port *int32) {
	if !announceWanted(cluster) {
		return "", nil
	}
	// FOUNDATION default: the per-pod external Service FQDN (valkey-<node>-ext) in the
	// cluster namespace, on the client port. reconcileExposeAnnounce overrides host
	// with the resolved external ingress address for that node key.
	node := naming.NodeName(cluster.Name, key.shard, key.node)
	host = perPodServiceName(node) + "." + cluster.Namespace + ".svc"
	clientPort := int32(valkey.ClientPort)
	return host, &clientPort
}
