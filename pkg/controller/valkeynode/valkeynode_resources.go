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
	"slices"
	"strconv"

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
	// portExporter is the exporter sidecar metrics port default (08 §2.4,
	// redis_exporter default). The effective port is resolved by exporterPort,
	// which honors spec.exporter.port when the parent sets it.
	portExporter = 9121

	// envPodIP is the reserved, operator-managed env var injected into the server
	// container (the downward-API pod IP used by --cluster-announce-ip). User env
	// (spec.env / spec.extraEnvVars) must never override it; mergeServerEnv drops
	// any user entry colliding with a reserved name and the operator-managed
	// entries always win (03 §2.6 user-env precedence).
	envPodIP = "POD_IP"

	// defaultExporterImage is the documented redis_exporter-compatible fallback
	// (08 §2.4) used only when the exporter is enabled but the parent supplied no
	// image. In production the cluster controller (M3) sets spec.exporter.image to
	// the Percona-branded GA exporter; this keeps a standalone node valid.
	defaultExporterImage = "oliver006/redis_exporter:v1.80.0"

	// serverContainerName is the Valkey server container name.
	serverContainerName = "server"
	// exporterContainerName is the exporter sidecar container name.
	exporterContainerName = "metrics-exporter"
	// restoreSeedContainerName is the restore-seed init container name (CR-8 /
	// 06 §7.4): it downloads this shard's RDB into /data/dump.rdb before the engine
	// starts when node.Spec.RestoreFrom is set.
	restoreSeedContainerName = "restore-seed"

	// defaultValkeyFSGroup is the gid the pod-level securityContext.fsGroup defaults
	// to when spec.podSecurityContext.fsGroup is nil. The percona/valkey image runs
	// the engine as uid 998; without an fsGroup the kubelet leaves a CSI-provisioned
	// /data owned by root, so uid 998 cannot write the RDB/AOF (a fail-to-start on
	// many CSI backends). Setting fsGroup=998 makes the kubelet chown the volume to
	// the engine's group so /data is writable. A user-supplied fsGroup always wins.
	defaultValkeyFSGroup int64 = 998

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

	// dhParamsVolName / dhParamsMountPath are the DH-params Secret volume and its
	// read-only mount point. FROZEN M5 contract: dhParamsMountPath MUST equal the
	// path the cluster-side config renderer points tls-dh-params-file at (the
	// dhParamsMountPath constant in the perconavalkeycluster auth seam, 07 §3.2) so
	// a rendered tls-dh-params-file directive always has a matching mounted file —
	// a missing mount would crash-loop the pod. Kept distinct from the cert mount so
	// DH params and the cert family rotate independently.
	dhParamsVolName   = "valkey-tls-dhparams"
	dhParamsMountPath = "/etc/valkey/tls-dhparams"

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

// sortedKeys returns the map keys in ascending order, so a map-derived env list
// renders deterministically (stable pod-template => no phantom rolls).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// reservedServerEnvNames are the operator-managed server-container env var names
// that user env (spec.env / spec.extraEnvVars) must never override. The
// operator-managed entries are emitted first and always win; any user entry
// colliding with one of these names is dropped (03 §2.6 user-env precedence).
func reservedServerEnvNames() map[string]bool {
	return map[string]bool{envPodIP: true}
}

// mergeServerEnv builds the server container env: the operator-managed entries
// (managed) first, then user env appended in deterministic order — the simple
// spec.env map (sorted by key for stable output) followed by spec.extraEnvVars in
// declared order. User entries whose name collides with a reserved
// operator-managed name are dropped so the operator's value always wins
// (precedence: operator-managed > user). Within the user set, a later
// extraEnvVars entry may shadow an earlier env-map entry of the same name (last
// wins for user-vs-user), but never a reserved name.
func mergeServerEnv(managed []corev1.EnvVar, envMap map[string]string, extra []corev1.EnvVar) []corev1.EnvVar {
	reserved := reservedServerEnvNames()
	out := make([]corev1.EnvVar, 0, len(managed)+len(envMap)+len(extra))
	out = append(out, managed...)

	for _, k := range sortedKeys(envMap) {
		if reserved[k] {
			continue
		}
		out = append(out, corev1.EnvVar{Name: k, Value: envMap[k]})
	}
	for _, e := range extra {
		if reserved[e.Name] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// exporterPort resolves the effective exporter metrics port: spec.exporter.port
// when the parent set it, else the documented portExporter (9121) fallback. The
// parent normally materializes the kubebuilder default (9121), but the pointer
// may be nil for a standalone ValkeyNode, so this keeps the build nil-safe.
func exporterPort(node *valkeyv1alpha1.ValkeyNode) int32 {
	if node.Spec.Exporter.Port != nil {
		return *node.Spec.Exporter.Port
	}
	return portExporter
}

// exporterTLSEnabled reports whether the exporter must serve /metrics over TLS
// (spec.exporter.tls.enabled). nil block => false (plain HTTP). This is the
// metrics-serving scheme toggle (08 §3.3); the PodMonitor/ServiceMonitor scheme
// switch is the observability leg's follow-up, but the serving side is wired
// here so the container actually listens over HTTPS.
func exporterTLSEnabled(node *valkeyv1alpha1.ValkeyNode) bool {
	return node.Spec.Exporter.TLS != nil && node.Spec.Exporter.TLS.Enabled
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

// buildProbes builds the startup/liveness/readiness probes. All three are a bare
// PING (process-alive / serving-commands only — never coupled to cluster health,
// 08 §6). Readiness MUST NOT require CLUSTER INFO cluster_state:ok: during initial
// bootstrap no node can ever reach cluster_state:ok until the operator has run
// MEET -> ADDSLOTSRANGE -> REPLICATE, but the cluster controller gates that
// bootstrap behind every node's pod becoming Ready (nodeConverged -> Status.Ready
// -> pod Ready). Coupling readiness to cluster_state therefore deadlocks cluster
// formation (the pod is never Ready, so the operator never bootstraps, so
// cluster_state never becomes ok). Pod readiness here means "the engine is up and
// answering commands" so the node is reachable for the operator's CLUSTER
// orchestration; full cluster health is surfaced via the CR status conditions
// (ClusterFormed / SlotsAssigned), not pod readiness. Probe scripts/commands are
// excluded from the config hash (04 §11).
func buildProbes(node *valkeyv1alpha1.ValkeyNode) (startup, liveness, readiness *corev1.Probe) {
	cli := valkeyCliArgs(node)
	ping := append(append([]string{}, cli...), "PING")

	startup = execProbe(ping, startupFailureThreshold, probeTimeoutSeconds)
	liveness = execProbe(ping, defaultFailureThreshold, probeTimeoutSeconds)
	readiness = execProbe(ping, defaultFailureThreshold, readinessTimeoutSeconds)
	return startup, liveness, readiness
}

// buildServerContainer builds the Valkey server container with ports, probes,
// volume mounts and env. The operator-managed env (the downward-API POD_IP used
// by --cluster-announce-ip) is emitted first; user env (spec.env then
// spec.extraEnvVars) is appended via mergeServerEnv with the operator-managed
// names reserved (user env can never clobber POD_IP — operator-managed wins). The
// propagated containerSecurityContext is applied for a hardened runtime (07 §6).
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
	if dhParamsSecretName(node) != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: dhParamsVolName, MountPath: dhParamsMountPath, ReadOnly: true})
	}

	managedEnv := []corev1.EnvVar{{
		Name:      envPodIP,
		ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}},
	}}

	c := corev1.Container{
		Name:      serverContainerName,
		Image:     serverImage(node),
		Resources: node.Spec.Resources,
		Command:   serverCommand(node),
		Env:       mergeServerEnv(managedEnv, node.Spec.Env, node.Spec.ExtraEnvVars),
		Ports: []corev1.ContainerPort{
			{Name: "client", ContainerPort: portClient},
			{Name: "cluster-bus", ContainerPort: portBus},
		},
		StartupProbe:    startup,
		LivenessProbe:   liveness,
		ReadinessProbe:  readiness,
		VolumeMounts:    mounts,
		SecurityContext: node.Spec.ContainerSecurityContext,
	}
	return c
}

// serverCommand builds the valkey-server argv. The base is the mounted
// valkey.conf, plus:
//
//   - the cluster-announce wiring: --cluster-announce-ip/--cluster-announce-port
//     from spec.announceHost/announcePort when the parent set an EXTERNAL address
//     (expose.perPod), else the in-cluster downward-API $(POD_IP) on the default
//     client port (the historical behaviour).
//   - --aclfile when an ACL Secret is mounted.
//   - --appendonly no when spec.restoreFrom is set, so the seeded /data/dump.rdb is
//     loaded on boot instead of an (empty) AOF (CR-8 / 06 §7.4); AOF is re-enabled
//     live (CONFIG SET appendonly yes) by the controller once the keyspace loads.
func serverCommand(node *valkeyv1alpha1.ValkeyNode) []string {
	cmd := []string{"valkey-server", configMountPath + "/valkey.conf"}
	cmd = append(cmd, announceArgs(node)...)
	if node.Spec.ACLSecretName != "" {
		cmd = append(cmd, "--aclfile", aclFilePath)
	}
	if node.Spec.RestoreFrom != nil {
		// Seed boot: load the downloaded dump.rdb, never an empty AOF.
		cmd = append(cmd, "--appendonly", "no")
	}
	return cmd
}

// announceArgs returns the --cluster-announce-ip (and, when an external port is
// set, --cluster-announce-port) args. When spec.announceHost is set the engine
// gossips that EXTERNAL address so a cluster-mode client reaching the per-pod
// external Service can follow MOVED/ASK redirects to the same address; otherwise
// it falls back to the downward-API $(POD_IP) (in-cluster default). A nil/zero
// announcePort with an announceHost set advertises the host on the default client
// port (the cluster bus port is derived by the engine as port+10000).
func announceArgs(node *valkeyv1alpha1.ValkeyNode) []string {
	if node.Spec.AnnounceHost == "" {
		return []string{"--cluster-announce-ip", "$(" + envPodIP + ")"}
	}
	args := []string{"--cluster-announce-ip", node.Spec.AnnounceHost}
	if node.Spec.AnnouncePort != nil && *node.Spec.AnnouncePort > 0 {
		args = append(args, "--cluster-announce-port", strconv.Itoa(int(*node.Spec.AnnouncePort)))
	}
	return args
}

// exporterScrapeArgs returns the exporter args for scraping the co-located Valkey
// over loopback, plus any TLS VolumeMounts. Two independent TLS axes are honored:
//
//   - scrape side (spec.tls): when the engine speaks TLS the exporter dials
//     rediss:// and validates the local node against the shared CA mounted at
//     naming.TLSMountPath (08 §2.4); otherwise it dials plain redis:// over
//     loopback.
//   - serving side (spec.exporter.tls.enabled): when set the exporter serves
//     /metrics over HTTPS using the cluster TLS cert family (08 §3.3), wiring the
//     redis_exporter --tls-server-cert-file/--tls-server-key-file flags. The
//     matching PodMonitor/ServiceMonitor scheme=https switch is the observability
//     leg's follow-up; the serving side is wired here so the container truly
//     listens over HTTPS.
//
// Either axis mounts the shared TLS volume (deduplicated to a single mount).
func exporterScrapeArgs(node *valkeyv1alpha1.ValkeyNode) (args []string, mounts []corev1.VolumeMount) {
	port := exporterPort(node)
	scrapeTLS := node.Spec.TLS != nil
	serveTLS := exporterTLSEnabled(node)

	if scrapeTLS {
		args = append(args,
			fmt.Sprintf("--redis.addr=rediss://localhost:%d", portClient),
			fmt.Sprintf("--tls-ca-cert-file=%s/%s", tlsMountPath, tlsKeyCA),
		)
	} else {
		args = append(args, fmt.Sprintf("--redis.addr=redis://localhost:%d", portClient))
	}
	args = append(args, fmt.Sprintf("--web.listen-address=:%d", port))
	if serveTLS {
		args = append(args,
			fmt.Sprintf("--tls-server-cert-file=%s/%s", tlsMountPath, tlsKeyCert),
			fmt.Sprintf("--tls-server-key-file=%s/%s", tlsMountPath, tlsKeyKey),
		)
	}

	if scrapeTLS || serveTLS {
		mounts = []corev1.VolumeMount{{Name: tlsVolName, MountPath: tlsMountPath, ReadOnly: true}}
	}
	return args, mounts
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
// exporter is disabled. It serves /metrics on the resolved exporter port
// (spec.exporter.port, default 9121), authenticates as _exporter
// (exporterCredEnv) and scrapes the local engine over loopback (TLS when
// spec.tls is set, exporterScrapeArgs). It has its OWN HTTP(S) readiness probe so
// its outage never marks Valkey unready or triggers a failover (08 §2.4, §6). The
// readiness scheme follows spec.exporter.tls.enabled (HTTPS when serving over
// TLS). The propagated containerSecurityContext is applied so the sidecar runs
// under the same hardened context as the server (07 §6).
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

	port := exporterPort(node)
	probeScheme := corev1.URISchemeHTTP
	if exporterTLSEnabled(node) {
		probeScheme = corev1.URISchemeHTTPS
	}

	return &corev1.Container{
		Name:            exporterContainerName,
		Image:           image,
		Args:            args,
		Env:             exporterCredEnv(node),
		Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
		VolumeMounts:    mounts,
		SecurityContext: node.Spec.ContainerSecurityContext,
		ReadinessProbe: &corev1.Probe{
			InitialDelaySeconds: exporterReadinessInitial,
			PeriodSeconds:       exporterReadinessInterval,
			TimeoutSeconds:      exporterTimeoutSeconds,
			SuccessThreshold:    1,
			FailureThreshold:    defaultFailureThreshold,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(port), Scheme: probeScheme},
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
	if name := dhParamsSecretName(node); name != "" {
		volumes = append(volumes, corev1.Volume{
			Name: dhParamsVolName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: name},
			},
		})
	}
	return volumes
}

// dhParamsSecretName returns the propagated DH-params Secret name (or "" when
// TLS is off or no DH params are configured). The cluster propagates
// spec.tls.dhParamsSecret onto node.Spec.TLS.DHParamsSecret; when set, the
// Secret is mounted at dhParamsMountPath so the rendered tls-dh-params-file
// directive resolves (07 §3.2).
func dhParamsSecretName(node *valkeyv1alpha1.ValkeyNode) string {
	if node.Spec.TLS == nil || node.Spec.TLS.DHParamsSecret == nil {
		return ""
	}
	return node.Spec.TLS.DHParamsSecret.Name
}

// buildPodTemplate builds the PodTemplateSpec, including the serverConfigHash
// pod-template annotation that triggers the rolling restart on change (04 §11).
// The propagated pod-level security knobs are applied: podSecurityContext sets
// the pod SecurityContext, serviceAccountName sets the pod ServiceAccountName,
// and automountServiceAccountToken (a *bool, default false materialized by the
// API) gates SA-token automounting (07 §6). Per-container hardening
// (containerSecurityContext) is applied in buildServerContainer / the exporter.
func buildPodTemplate(node *valkeyv1alpha1.ValkeyNode, labels map[string]string) (corev1.PodTemplateSpec, error) {
	containers, err := buildContainers(node)
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers:                   containers,
			InitContainers:               buildInitContainers(node),
			ImagePullSecrets:             node.Spec.ImagePullSecrets,
			NodeSelector:                 node.Spec.NodeSelector,
			Affinity:                     node.Spec.Affinity,
			Tolerations:                  node.Spec.Tolerations,
			TopologySpreadConstraints:    node.Spec.TopologySpreadConstraints,
			Volumes:                      buildVolumes(node),
			SecurityContext:              podSecurityContext(node),
			ServiceAccountName:           node.Spec.ServiceAccountName,
			AutomountServiceAccountToken: node.Spec.AutomountServiceAccountToken,
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

// podSecurityContext returns the pod-level SecurityContext to apply, defaulting
// fsGroup to defaultValkeyFSGroup (998 — the percona/valkey image uid) when the
// propagated spec.podSecurityContext leaves it nil, so a CSI-provisioned /data is
// group-owned by the engine and writable on boot. A user-supplied fsGroup (or any
// other field) always wins; the input is never mutated (a copy is returned). nil
// in => a fresh SecurityContext carrying only the default fsGroup.
func podSecurityContext(node *valkeyv1alpha1.ValkeyNode) *corev1.PodSecurityContext {
	src := node.Spec.PodSecurityContext
	if src != nil && src.FSGroup != nil {
		return src // user set fsGroup explicitly — honour it verbatim.
	}
	var out corev1.PodSecurityContext
	if src != nil {
		out = *src.DeepCopy()
	}
	fsGroup := defaultValkeyFSGroup
	out.FSGroup = &fsGroup
	return &out
}

// buildInitContainers returns the pod init containers. When spec.restoreFrom is set
// it injects the restore-seed init container that downloads this shard's RDB into
// /data/dump.rdb before the engine boots (CR-8 / 06 §7.4); otherwise it is empty.
func buildInitContainers(node *valkeyv1alpha1.ValkeyNode) []corev1.Container {
	if node.Spec.RestoreFrom == nil {
		return nil
	}
	return []corev1.Container{buildRestoreSeedContainer(node)}
}

// buildRestoreSeedContainer builds the restore-seed init container (CR-8 /
// 06 §7.4): it runs cmd/valkey-backup --download --shard=<i> in the DB image to
// fetch this node's shard RDB into the shared data volume at /data/dump.rdb BEFORE
// the engine starts, verifying the SHA-256 against the manifest (the sidecar fails
// pod start on mismatch, 06 §9.3). The storage type/coordinates + cluster/backup
// names that derive the object keys are passed by the restore leg via the
// VALKEY_BACKUP_* env contract (it wires the env + credentials when it stamps
// spec.restoreFrom); this builder owns only the container shape (image, the
// --download/--shard flags, and the data-volume mount) so the seed mechanism is
// rendered identically by the node controller regardless of which leg triggers it.
func buildRestoreSeedContainer(node *valkeyv1alpha1.ValkeyNode) corev1.Container {
	mounts := []corev1.VolumeMount{}
	if node.Spec.Persistence != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: naming.NodePVCName(node.Name), MountPath: dataMountPath})
	}
	return corev1.Container{
		Name:  restoreSeedContainerName,
		Image: serverImage(node),
		Command: []string{
			"/valkey-backup",
			"--download",
			"--shard=" + strconv.Itoa(int(node.Spec.RestoreFrom.ShardIndex)),
		},
		VolumeMounts:    mounts,
		SecurityContext: node.Spec.ContainerSecurityContext,
	}
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
