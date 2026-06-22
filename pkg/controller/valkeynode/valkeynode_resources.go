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
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

const (
	// portClient is the Valkey client port (Charter / 05 §ports).
	portClient = valkey.ClientPort // 6379
	// portBus is the Valkey cluster-bus port (Charter / 05 §ports).
	portBus = 16379
	// portExporter is the exporter sidecar metrics port (08 §2.4, redis_exporter default).
	portExporter = 9121

	// defaultExporterImage is the documented redis_exporter-compatible fallback
	// (08 §2.4) used only when the exporter is enabled but the parent supplied no
	// image. In production the cluster controller (M3) sets spec.exporter.image to
	// the Percona-branded GA exporter; this keeps a standalone node valid.
	defaultExporterImage = "oliver006/redis_exporter:v1.80.0"

	// serverContainerName is the Valkey server container name.
	serverContainerName = "server"
	// exporterContainerName is the exporter sidecar container name.
	exporterContainerName = "metrics-exporter"

	// Volume / mount names and paths. The data volume name is derived from the
	// PVC/volumeClaimTemplate name (valkey-<node>-data), so there is no fixed
	// constant for it.
	dataMountPath   = "/data"
	configVolName   = "valkey-config"
	configMountPath = "/config"
	aclVolName      = "users-acl"
	aclMountPath    = "/config/users"
	aclFilePath     = "/config/users/users.acl"
	tlsVolName      = "valkey-tls"
	// tlsMountPath is the read-only TLS cert mount point. FROZEN M5 contract: it
	// is the single naming.TLSMountPath shared with the config renderer
	// (pkg/valkey/config.go tls-*-file directives) so the mounted files and the
	// rendered paths always agree (07 §3.1).
	tlsMountPath = naming.TLSMountPath

	// TLS Secret data keys (03 §2.8 / 07 §3.1) — the single naming.TLSSecretKey*
	// constants so the mount and the renderer reference identical key names.
	tlsKeyCA   = naming.TLSSecretKeyCA
	tlsKeyCert = naming.TLSSecretKeyCert
	tlsKeyKey  = naming.TLSSecretKeyKey

	// Probe thresholds (OQ-2.4 interim: PXC/PSMDB-style defaults). Startup is
	// generous so a large RDB/AOF load does not trip liveness; liveness/readiness
	// use tighter windows.
	startupFailureThreshold   = 30
	defaultFailureThreshold   = 5
	probePeriodSeconds        = 10
	probeTimeoutSeconds       = 5
	readinessTimeoutSeconds   = 3
	probeInitialDelaySeconds  = 5
	exporterPeriodSeconds     = 10
	exporterTimeoutSeconds    = 3
	exporterReadinessInitial  = 5
	exporterReadinessInterval = 5
)

// exporterDefaultResources are the small default requests for the exporter
// sidecar (08 §2.4: ~50m/64Mi), used when spec.exporter.resources is empty.
func exporterDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    mustQuantity("50m"),
			corev1.ResourceMemory: mustQuantity("64Mi"),
		},
	}
}

// serverImage returns the resolved server image (spec.image, never defaulted
// here — the parent resolves it; an empty value is left as-is so the build
// surfaces it).
func serverImage(node *valkeyv1alpha1.ValkeyNode) string {
	return node.Spec.Image
}

// configMapName resolves the ConfigMap to mount: the parent-supplied
// serverConfigMapName when set, else the node's own (standalone) ConfigMap.
func configMapName(node *valkeyv1alpha1.ValkeyNode) string {
	if node.Spec.ServerConfigMapName != "" {
		return node.Spec.ServerConfigMapName
	}
	return naming.NodeConfigMapName(node.Name)
}

// valkeyCliArgs returns the base valkey-cli args used by exec probes, adding the
// TLS flags + mounted CA/cert/key when spec.tls is set (08 §6 TLS dual-port).
func valkeyCliArgs(node *valkeyv1alpha1.ValkeyNode) []string {
	args := []string{"valkey-cli", "-p", fmt.Sprintf("%d", portClient)}
	if node.Spec.TLS != nil {
		args = append(args,
			"--tls",
			"--cacert", tlsMountPath+"/"+tlsKeyCA,
			"--cert", tlsMountPath+"/"+tlsKeyCert,
			"--key", tlsMountPath+"/"+tlsKeyKey,
		)
	}
	return args
}

// execProbe builds an exec probe running the given command with the supplied
// failure threshold and timeout.
func execProbe(cmd []string, failureThreshold, timeoutSeconds int32) *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: probeInitialDelaySeconds,
		PeriodSeconds:       probePeriodSeconds,
		FailureThreshold:    failureThreshold,
		TimeoutSeconds:      timeoutSeconds,
		SuccessThreshold:    1,
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: cmd},
		},
	}
}

// buildProbes builds the startup/liveness/readiness probes. Liveness is a bare
// PING (process-alive only — never coupled to cluster health, 08 §6); readiness
// adds CLUSTER INFO cluster_state:ok in cluster mode so it gates the roll
// correctly. Probe scripts/commands are excluded from the config hash (04 §11).
func buildProbes(node *valkeyv1alpha1.ValkeyNode) (startup, liveness, readiness *corev1.Probe) {
	cli := valkeyCliArgs(node)
	ping := append(append([]string{}, cli...), "PING")

	startup = execProbe(ping, startupFailureThreshold, probeTimeoutSeconds)
	liveness = execProbe(ping, defaultFailureThreshold, probeTimeoutSeconds)

	// Readiness: PING succeeds AND cluster_state is ok (cluster mode). A shell
	// wrapper keeps liveness independent while readiness reflects cluster_state.
	readinessCmd := []string{
		"sh", "-c",
		fmt.Sprintf("%s PING | grep -q PONG && %s CLUSTER INFO | grep -q cluster_state:ok",
			shellJoin(cli), shellJoin(cli)),
	}
	readiness = &corev1.Probe{
		InitialDelaySeconds: probeInitialDelaySeconds,
		PeriodSeconds:       probePeriodSeconds,
		FailureThreshold:    defaultFailureThreshold,
		TimeoutSeconds:      readinessTimeoutSeconds,
		SuccessThreshold:    1,
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: readinessCmd},
		},
	}
	return startup, liveness, readiness
}

// buildServerContainer builds the Valkey server container with ports, probes,
// volume mounts and env.
func buildServerContainer(node *valkeyv1alpha1.ValkeyNode) corev1.Container {
	startup, liveness, readiness := buildProbes(node)

	mounts := []corev1.VolumeMount{
		{Name: configVolName, MountPath: configMountPath, ReadOnly: true},
	}
	if node.Spec.Persistence != nil {
		// The data volume is supplied by the StatefulSet volumeClaimTemplate, so
		// the mount name must equal the VCT name (valkey-<node>-data) for the STS
		// to validate (persistence is forbidden with a Deployment).
		mounts = append(mounts, corev1.VolumeMount{Name: naming.NodePVCName(node.Name), MountPath: dataMountPath})
	}
	if node.Spec.ACLSecretName != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: aclVolName, MountPath: aclMountPath, ReadOnly: true})
	}
	if node.Spec.TLS != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: tlsVolName, MountPath: tlsMountPath, ReadOnly: true})
	}

	c := corev1.Container{
		Name:      serverContainerName,
		Image:     serverImage(node),
		Resources: node.Spec.Resources,
		Command: []string{
			"valkey-server", configMountPath + "/valkey.conf",
			"--cluster-announce-ip", "$(POD_IP)",
		},
		Env: []corev1.EnvVar{{
			Name:      "POD_IP",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}},
		}},
		Ports: []corev1.ContainerPort{
			{Name: "client", ContainerPort: portClient},
			{Name: "cluster-bus", ContainerPort: portBus},
		},
		StartupProbe:   startup,
		LivenessProbe:  liveness,
		ReadinessProbe: readiness,
		VolumeMounts:   mounts,
	}
	if node.Spec.ACLSecretName != "" {
		c.Command = append(c.Command, "--aclfile", aclFilePath)
	}
	return c
}

// exporterScrapeArgs returns the exporter args for scraping the co-located Valkey
// over loopback, plus any TLS VolumeMounts. This is the TLS-aware metrics-option
// seam: when spec.tls is set the exporter dials rediss:// and validates the local
// node against the shared CA mounted at naming.TLSMountPath (08 §2.4); otherwise
// it dials plain redis:// over loopback. The metrics-TLS *serving* side
// (spec.exporter.tls, scheme=https on the PodMonitor) is an OPS/observability-leg
// concern (GO-5.x / OPS-5.3, OQ-2) layered on top of these scrape args and is
// deliberately not wired here.
func exporterScrapeArgs(node *valkeyv1alpha1.ValkeyNode) (args []string, mounts []corev1.VolumeMount) {
	if node.Spec.TLS != nil {
		args = []string{
			fmt.Sprintf("--redis.addr=rediss://localhost:%d", portClient),
			fmt.Sprintf("--tls-ca-cert-file=%s/%s", tlsMountPath, tlsKeyCA),
			fmt.Sprintf("--web.listen-address=:%d", portExporter),
		}
		mounts = []corev1.VolumeMount{{Name: tlsVolName, MountPath: tlsMountPath, ReadOnly: true}}
		return args, mounts
	}
	args = []string{
		fmt.Sprintf("--redis.addr=redis://localhost:%d", portClient),
		fmt.Sprintf("--web.listen-address=:%d", portExporter),
	}
	return args, nil
}

// exporterCredEnv returns the _exporter auth env: the username plus the password
// injected via a SecretKeyRef to internal-<cluster>-system-passwords[key
// _exporter] (the redis_exporter REDIS_USER/REDIS_PASSWORD convention, 08 §2.4).
// The ref is Optional so a standalone ValkeyNode (created without a parent
// cluster, hence without the system-passwords Secret) still schedules; in a
// cluster the parent always provisions the Secret first (GO-5.3). The env var
// names + Secret key are the FROZEN M5 contract (naming.EnvExporter*).
func exporterCredEnv(node *valkeyv1alpha1.ValkeyNode) []corev1.EnvVar {
	cluster := naming.NodeCluster(node.Name, node.Labels)
	return []corev1.EnvVar{
		{Name: naming.EnvExporterUser, Value: naming.SystemUserExporter},
		{
			Name: naming.EnvExporterPassword,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: naming.SystemPasswordsSecretName(cluster)},
				Key:                  naming.SystemUserExporter,
				Optional:             ptrTo(true),
			}},
		},
	}
}

// buildExporterSidecar builds the exporter sidecar container, or nil when the
// exporter is disabled. It serves /metrics on port 9121, authenticates as
// _exporter (exporterCredEnv) and scrapes the local engine over loopback (TLS
// when spec.tls is set, exporterScrapeArgs). It has its OWN HTTP readiness probe
// so its outage never marks Valkey unready or triggers a failover (08 §2.4, §6).
func buildExporterSidecar(node *valkeyv1alpha1.ValkeyNode) *corev1.Container {
	if !node.Spec.Exporter.Enabled {
		return nil
	}

	args, mounts := exporterScrapeArgs(node)

	resources := node.Spec.Exporter.Resources
	if resources.Requests == nil && resources.Limits == nil {
		resources = exporterDefaultResources()
	}

	image := node.Spec.Exporter.Image
	if image == "" {
		image = defaultExporterImage
	}

	return &corev1.Container{
		Name:         exporterContainerName,
		Image:        image,
		Args:         args,
		Env:          exporterCredEnv(node),
		Ports:        []corev1.ContainerPort{{Name: "metrics", ContainerPort: portExporter, Protocol: corev1.ProtocolTCP}},
		VolumeMounts: mounts,
		ReadinessProbe: &corev1.Probe{
			InitialDelaySeconds: exporterReadinessInitial,
			PeriodSeconds:       exporterReadinessInterval,
			TimeoutSeconds:      exporterTimeoutSeconds,
			SuccessThreshold:    1,
			FailureThreshold:    defaultFailureThreshold,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt(portExporter)},
			},
		},
		Resources: resources,
	}
}

// buildContainers assembles the server container plus exporter sidecar, then
// applies the user strategic-merge spec.containers patch (matched by name;
// unknown names appended).
func buildContainers(node *valkeyv1alpha1.ValkeyNode) ([]corev1.Container, error) {
	containers := []corev1.Container{buildServerContainer(node)}
	if exp := buildExporterSidecar(node); exp != nil {
		containers = append(containers, *exp)
	}
	return mergeContainerPatch(containers, node.Spec.Containers)
}

// mergeContainerPatch applies a strategic-merge patch (patches) to base
// containers by name. A patch container matching a base name is strategic-merged
// into it; unmatched patch containers are appended in patch-list order (03 §2.6).
func mergeContainerPatch(base, patches []corev1.Container) ([]corev1.Container, error) {
	if len(patches) == 0 {
		return base, nil
	}
	patchByName := make(map[string]corev1.Container, len(patches))
	for _, c := range patches {
		patchByName[c.Name] = c
	}
	out := make([]corev1.Container, 0, len(base)+len(patches))
	for _, c := range base {
		patch, ok := patchByName[c.Name]
		if !ok {
			out = append(out, c)
			continue
		}
		baseBytes, err := json.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("marshal base container %s: %w", c.Name, err)
		}
		patchBytes, err := json.Marshal(patch)
		if err != nil {
			return nil, fmt.Errorf("marshal patch container %s: %w", c.Name, err)
		}
		merged, err := strategicpatch.StrategicMergePatch(baseBytes, patchBytes, corev1.Container{})
		if err != nil {
			return nil, fmt.Errorf("merge container %s: %w", c.Name, err)
		}
		var result corev1.Container
		if err := json.Unmarshal(merged, &result); err != nil {
			return nil, fmt.Errorf("unmarshal merged container %s: %w", c.Name, err)
		}
		out = append(out, result)
		delete(patchByName, c.Name)
	}
	for _, c := range patches {
		if _, remaining := patchByName[c.Name]; remaining {
			out = append(out, c)
		}
	}
	return out, nil
}

// buildVolumes assembles the pod volumes: the mounted config, plus ACL/TLS/data
// volumes when their respective specs are set. The data volume is supplied via
// the StatefulSet volumeClaimTemplate, so it is NOT added here for STS; for a
// Deployment there is no data volume (persistence is forbidden with Deployment).
func buildVolumes(node *valkeyv1alpha1.ValkeyNode) []corev1.Volume {
	volumes := []corev1.Volume{{
		Name: configVolName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(node)},
			},
		},
	}}
	if node.Spec.ACLSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: aclVolName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: node.Spec.ACLSecretName},
			},
		})
	}
	if node.Spec.TLS != nil && node.Spec.TLS.SecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: tlsVolName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: node.Spec.TLS.SecretName},
			},
		})
	}
	return volumes
}

// buildPodTemplate builds the PodTemplateSpec, including the serverConfigHash
// pod-template annotation that triggers the rolling restart on change (04 §11).
func buildPodTemplate(node *valkeyv1alpha1.ValkeyNode, labels map[string]string) (corev1.PodTemplateSpec, error) {
	containers, err := buildContainers(node)
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers:                containers,
			ImagePullSecrets:          node.Spec.ImagePullSecrets,
			NodeSelector:              node.Spec.NodeSelector,
			Affinity:                  node.Spec.Affinity,
			Tolerations:               node.Spec.Tolerations,
			TopologySpreadConstraints: node.Spec.TopologySpreadConstraints,
			Volumes:                   buildVolumes(node),
		},
	}
	applyConfigHashAnnotation(&tmpl, node.Spec.ServerConfigHash)
	applyTLSHashAnnotation(&tmpl, node.Annotations[naming.AnnTLSHash])
	return tmpl, nil
}

// applyConfigHashAnnotation stamps the parent-supplied server-config hash onto
// the pod-template annotations VERBATIM (the node never recomputes it). A change
// to the value rolls the single-replica workload (04 §3.1 step 3 / §11).
func applyConfigHashAnnotation(tmpl *corev1.PodTemplateSpec, hash string) {
	if hash == "" {
		return
	}
	if tmpl.Annotations == nil {
		tmpl.Annotations = map[string]string{}
	}
	tmpl.Annotations[naming.AnnServerConfigHash] = hash
}

// applyTLSHashAnnotation stamps the parent-supplied TLS hash onto the pod-template
// annotations VERBATIM. The cluster controller (M5 GO-5.8 tls_rotation) computes
// the hash from the cert material and stamps naming.AnnTLSHash onto the ValkeyNode
// object; this propagates it onto the pod template so a real cert change rolls the
// single-replica workload through the SAME machinery as the config hash (07 §3.4).
// Empty (TLS off / no hash yet) leaves the annotation absent, so it never causes a
// phantom roll. The value changes ONLY on a real cert change.
func applyTLSHashAnnotation(tmpl *corev1.PodTemplateSpec, hash string) {
	if hash == "" {
		return
	}
	if tmpl.Annotations == nil {
		tmpl.Annotations = map[string]string{}
	}
	tmpl.Annotations[naming.AnnTLSHash] = hash
}

// buildStatefulSet builds the 1-replica durable StatefulSet with a
// volumeClaimTemplate (PVC valkey-<node>-data) when persistence is set.
func buildStatefulSet(node *valkeyv1alpha1.ValkeyNode, labels map[string]string) (*appsv1.StatefulSet, error) {
	tmpl, err := buildPodTemplate(node, labels)
	if err != nil {
		return nil, err
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.NodeWorkloadName(node.Name),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptrTo(int32(1)),
			ServiceName: naming.NodeWorkloadName(node.Name),
			Selector:    &metav1.LabelSelector{MatchLabels: selectorLabels(labels)},
			Template:    tmpl,
		},
	}
	if node.Spec.Persistence != nil {
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{*buildPVCTemplate(node, labels)}
	}
	return sts, nil
}

// buildDeployment builds the 1-replica cache Deployment (no PVC; persistence is
// forbidden with Deployment). The Recreate strategy ensures the single pod is
// torn down before a replacement, matching the at-most-one-pod invariant.
func buildDeployment(node *valkeyv1alpha1.ValkeyNode, labels map[string]string) (*appsv1.Deployment, error) {
	tmpl, err := buildPodTemplate(node, labels)
	if err != nil {
		return nil, err
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.NodeWorkloadName(node.Name),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrTo(int32(1)),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(labels)},
			Template: tmpl,
		},
	}, nil
}

// buildPVCTemplate builds the data PVC (used as the STS volumeClaimTemplate and
// for the expand-only guard). Returns nil when persistence is unset.
func buildPVCTemplate(node *valkeyv1alpha1.ValkeyNode, labels map[string]string) *corev1.PersistentVolumeClaim {
	if node.Spec.Persistence == nil {
		return nil
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.NodePVCName(node.Name),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: node.Spec.Persistence.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: node.Spec.Persistence.Size},
			},
		},
	}
}

// selectorLabels returns the immutable subset of labels used as the workload
// pod selector (StatefulSet/Deployment selectors are immutable, so they must not
// include the full recommended-label set that may evolve). It keys on the
// operator cluster/shard/node labels plus the stable app instance.
func selectorLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range []string{naming.LabelAppInstance, naming.LabelCluster, naming.LabelShardIndex, naming.LabelNodeIndex} {
		if v, ok := labels[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		// Fallback so the selector is never empty.
		out[naming.LabelAppName] = labels[naming.LabelAppName]
	}
	return out
}
