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
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// upsertService ensures the headless Service valkey-<cluster> for per-pod DNS
// addressing (client port 6379 + cluster-bus port 16379, clusterIP None,
// selector valkey.percona.com/cluster=<cluster>). PublishNotReadyAddresses is
// set so a bootstrapping (not-yet-Ready) pod still gets a stable DNS A record —
// MEET targets must resolve before the node passes readiness (04 §2.1 step1,
// 05 §3). 04 §2.1 step1.
func (r *Reconciler) upsertService(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	svc := &corev1.Service{}
	svc.Name, svc.Namespace = naming.HeadlessServiceName(cluster.Name), cluster.Namespace
	res, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, svc, func() error {
		svc.Labels = naming.Labels(cluster.Name, naming.ComponentValkey)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		// ClusterIP is immutable after creation; only set None on create.
		if svc.Spec.ClusterIP == "" {
			svc.Spec.ClusterIP = corev1.ClusterIPNone
		}
		svc.Spec.PublishNotReadyAddresses = true
		svc.Spec.Selector = naming.ClusterSelector(cluster.Name)
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "valkey", Port: valkey.ClientPort, TargetPort: intstr.FromInt32(valkey.ClientPort)},
			{Name: "cluster-bus", Port: valkey.BusPort, TargetPort: intstr.FromInt32(valkey.BusPort)},
		}
		return nil
	})
	if err != nil {
		return err
	}
	if res == controllerutil.OperationResultCreated {
		r.recorder.Eventf(cluster, svc, eventNormal, EventServiceCreated, "CreateService", "Created headless Service %s", svc.Name)
	}
	return nil
}
