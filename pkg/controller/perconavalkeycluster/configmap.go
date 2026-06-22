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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// ConfigMap data keys for the embedded health/probe scripts. They are
// deliberately EXCLUDED from the config-roll hash (04 §11) so an operator
// upgrade that only touches probe scripts does not roll every pod.
const (
	readinessScriptKey = "readiness-check.sh"
	livenessScriptKey  = "liveness-check.sh"
)

// probeScript is a minimal placeholder probe (the Node controller wires the
// actual probes; the script is shipped here so the mount exists). Excluded from
// the roll hash.
const probeScript = "#!/bin/sh\nvalkey-cli -p 6379 ping\n"

// configInput adapts a cluster spec to the pkg/valkey ConfigInput (the renderer
// stays decoupled from the API type shape).
func configInput(cluster *valkeyv1alpha1.PerconaValkeyCluster) valkey.ConfigInput {
	return valkey.ConfigInput{
		UserConfig:  cluster.Spec.Config,
		Persistence: cluster.Spec.Persistence != nil,
		TLS:         cluster.Spec.TLS != nil,
		ACL:         true,
	}
}

// upsertConfigMap renders valkey.conf (user-first, operator-base-last) + the
// probe scripts into the shared ConfigMap valkey-<cluster>, and returns the
// config-roll hash computed from spec (excluding live-settable keys, 04 §11).
// The hash is computed from spec — never read back from the live ConfigMap — so
// the stamped ValkeyNode.spec.serverConfigHash can never silently lag. 04 §2.1
// step4.
func (r *Reconciler) upsertConfigMap(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) (string, error) {
	// Re-materialize the security defaults stripped by the earlier finalizer PATCH
	// so the rendered config + roll hash are deterministic across reconciles.
	if err := r.ensureConfigDefaults(ctx, cluster); err != nil {
		return "", err
	}
	in, err := r.applySecurityConfigInput(ctx, cluster, configInput(cluster))
	if err != nil {
		return "", err
	}
	hash := valkey.ServerConfigRollHash(in)
	rendered := valkey.RenderServerConfig(in)

	cm := &corev1.ConfigMap{}
	cm.Name, cm.Namespace = naming.ClusterConfigMapName(cluster.Name), cluster.Namespace
	res, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, cm, func() error {
		cm.Labels = naming.Labels(cluster.Name, naming.ComponentValkey)
		cm.Data = map[string]string{
			valkey.ConfigFileKey: rendered,
			readinessScriptKey:   probeScript,
			livenessScriptKey:    probeScript,
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if res == controllerutil.OperationResultCreated {
		r.recorder.Eventf(cluster, cm, eventNormal, EventConfigMapCreated, "CreateConfigMap", "Created ConfigMap %s", cm.Name)
	}
	return hash, nil
}
