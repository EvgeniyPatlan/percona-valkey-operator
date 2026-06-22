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

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// reconcilePodDisruptionBudget ensures a cluster-wide PDB so voluntary
// disruptions never break a per-shard quorum when policy is Managed; it deletes
// the PDB when policy is Disabled (04 §2.1 step2).
//
// OQ-3.B (PDB sizing): the doc leaves the exact formula open. Wave 2a uses a
// conservative cluster-wide maxUnavailable=1 — at most one Valkey pod may be
// voluntarily evicted across the whole cluster at a time. With replicas>=1 every
// shard has >=2 pods, so a single eviction can never drop a shard below its
// surviving member, and the operator's one-at-a-time roll already serialises
// planned disruptions. (A per-shard PDB is a 2b refinement.)
func (r *Reconciler) reconcilePodDisruptionBudget(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	if cluster.Spec.PodDisruptionBudget == valkeyv1alpha1.PDBDisabled {
		return r.deletePodDisruptionBudget(ctx, cluster)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	pdb.Name, pdb.Namespace = naming.ClusterPDBName(cluster.Name), cluster.Namespace
	maxUnavailable := intstr.FromInt32(1)
	_, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, pdb, func() error {
		pdb.Labels = naming.Labels(cluster.Name, naming.ComponentValkey)
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: naming.ClusterSelector(cluster.Name)}
		return nil
	})
	return err
}

// deletePodDisruptionBudget removes the operator-managed PDB (policy Disabled).
// A NotFound is benign.
func (r *Reconciler) deletePodDisruptionBudget(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	pdb := &policyv1.PodDisruptionBudget{}
	pdb.Name, pdb.Namespace = naming.ClusterPDBName(cluster.Name), cluster.Namespace
	if err := r.Delete(ctx, pdb); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}
