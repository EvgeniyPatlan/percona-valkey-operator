package valkeynode

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

func unitNode(name string) *valkeyv1alpha1.ValkeyNode {
	return &valkeyv1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "valkey",
			Labels: map[string]string{
				naming.LabelCluster:    "mycluster",
				naming.LabelShardIndex: "0",
				naming.LabelNodeIndex:  "0",
			},
		},
		Spec: valkeyv1alpha1.ValkeyNodeSpec{
			Image:        "percona/percona-valkey:9.0",
			WorkloadType: valkeyv1alpha1.WorkloadStatefulSet,
		},
	}
}

func TestRoleFromInfo(t *testing.T) {
	cases := []struct {
		role string
		want valkeyv1alpha1.NodeRole
	}{
		{valkey.InfoRoleMaster, valkeyv1alpha1.NodeRolePrimary},
		{valkey.InfoRoleSlave, valkeyv1alpha1.NodeRoleReplica},
		{"unknown", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := roleFromInfo(map[string]string{valkey.InfoKeyRole: c.role})
		if got != c.want {
			t.Errorf("roleFromInfo(%q) = %q, want %q", c.role, got, c.want)
		}
	}
	if got := roleFromInfo(map[string]string{}); got != "" {
		t.Errorf("roleFromInfo(empty) = %q, want empty", got)
	}
}

func TestLiveSettableKeysExact(t *testing.T) {
	keys := liveSettableKeys()
	want := map[string]bool{"maxmemory": true, "maxmemory-policy": true, "maxclients": true}
	if len(keys) != len(want) {
		t.Fatalf("liveSettableKeys = %v, want exactly %v", keys, want)
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected live-settable key %q", k)
		}
	}
	// Roll-only keys must NOT be live-settable.
	for _, k := range []string{"appendonly", "save", "cluster-node-timeout", "maxmemory-samples"} {
		if isLiveSettableKey(k) {
			t.Errorf("%q must not be live-settable", k)
		}
	}
}

func TestPendingLiveConfig(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Config = map[string]string{
		"maxmemory":        "200mb",
		"maxmemory-policy": "allkeys-lru",
		"appendonly":       "yes", // roll-only, must be excluded
	}
	pending := pendingLiveConfig(node)
	if len(pending) != 2 {
		t.Fatalf("pending = %v, want 2 live keys", pending)
	}
	// Deterministic order: maxclients, maxmemory, maxmemory-policy (alpha).
	if pending[0][0] != "maxmemory" || pending[1][0] != "maxmemory-policy" {
		t.Errorf("unexpected order/keys: %v", pending)
	}
}

func TestGuardWorkloadType(t *testing.T) {
	sts := unitNode("n")
	if err := guardWorkloadType(sts); err != nil {
		t.Errorf("StatefulSet should pass: %v", err)
	}

	dep := unitNode("n")
	dep.Spec.WorkloadType = valkeyv1alpha1.WorkloadDeployment
	if err := guardWorkloadType(dep); err != nil {
		t.Errorf("Deployment without persistence should pass: %v", err)
	}

	depPersist := unitNode("n")
	depPersist.Spec.WorkloadType = valkeyv1alpha1.WorkloadDeployment
	depPersist.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")}
	if err := guardWorkloadType(depPersist); err == nil {
		t.Error("Deployment + persistence must be rejected")
	}

	bad := unitNode("n")
	bad.Spec.WorkloadType = "Garbage"
	if err := guardWorkloadType(bad); err == nil {
		t.Error("unknown workloadType must be rejected")
	}
}

func TestGuardPVCImmutable(t *testing.T) {
	mk := func(size string, sc *string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: sc,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
				},
			},
		}
	}
	sc := "fast"
	scOther := "slow"

	if err := guardPVCImmutable(mk("1Gi", &sc), mk("2Gi", &sc)); err != nil {
		t.Errorf("grow should be allowed: %v", err)
	}
	if err := guardPVCImmutable(mk("2Gi", &sc), mk("1Gi", &sc)); err == nil {
		t.Error("shrink must be rejected")
	}
	if err := guardPVCImmutable(mk("1Gi", &sc), mk("1Gi", &scOther)); err == nil {
		t.Error("storageClass change must be rejected")
	}
	if err := guardPVCImmutable(nil, mk("1Gi", &sc)); err != nil {
		t.Errorf("nil current should be a no-op: %v", err)
	}
}

func TestBuildStatefulSetShape(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")}
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: true, Image: "percona/valkey-exporter:1"}
	node.Spec.ServerConfigHash = "abc123"

	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}
	if sts.Name != "valkey-mycluster-0-0" {
		t.Errorf("STS name = %q", sts.Name)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Errorf("replicas must be 1, got %v", sts.Spec.Replicas)
	}
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected 1 volumeClaimTemplate, got %d", len(sts.Spec.VolumeClaimTemplates))
	}
	if sts.Spec.VolumeClaimTemplates[0].Name != "valkey-mycluster-0-0-data" {
		t.Errorf("PVC template name = %q", sts.Spec.VolumeClaimTemplates[0].Name)
	}
	if sts.Spec.Template.Annotations[naming.AnnServerConfigHash] != "abc123" {
		t.Errorf("config-hash annotation = %q", sts.Spec.Template.Annotations[naming.AnnServerConfigHash])
	}

	// server + exporter containers.
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 2 {
		t.Fatalf("expected server+exporter, got %d containers", len(containers))
	}
	server := containers[0]
	if server.Name != serverContainerName {
		t.Errorf("first container = %q, want server", server.Name)
	}
	if !hasPort(server.Ports, portClient) || !hasPort(server.Ports, portBus) {
		t.Errorf("server ports = %v, want client+bus", server.Ports)
	}
	if server.StartupProbe == nil || server.LivenessProbe == nil || server.ReadinessProbe == nil {
		t.Error("server must have startup/liveness/readiness probes")
	}
	exp := containers[1]
	if exp.Name != exporterContainerName || !hasPort(exp.Ports, portExporter) {
		t.Errorf("exporter container/port wrong: %+v", exp.Ports)
	}
	if exp.Ports[0].Name != "metrics" {
		t.Errorf("exporter port name = %q, want metrics", exp.Ports[0].Name)
	}
	if exp.ReadinessProbe == nil || exp.ReadinessProbe.HTTPGet == nil {
		t.Error("exporter must have its own HTTP readiness probe")
	}
}

func TestBuildDeploymentNoPVC(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.WorkloadType = valkeyv1alpha1.WorkloadDeployment
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: false}

	labels := naming.NodeLabels(node.Name, node.Labels)
	dep, err := buildDeployment(node, labels)
	if err != nil {
		t.Fatalf("buildDeployment: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas must be 1")
	}
	if dep.Spec.Strategy.Type != "Recreate" {
		t.Errorf("strategy = %q, want Recreate", dep.Spec.Strategy.Type)
	}
	// exporter disabled => single container.
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("exporter disabled should yield 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	// No data volume mount for cache mode.
	for _, vm := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if vm.MountPath == dataMountPath {
			t.Error("cache deployment must not mount data volume")
		}
	}
}

func TestTLSProbesAndVolumes(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}
	node.Spec.ACLSecretName = "internal-mycluster-acl"

	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatal(err)
	}
	server := sts.Spec.Template.Spec.Containers[0]
	// Probes must carry --tls.
	if !probeHasArg(server.LivenessProbe, "--tls") {
		t.Error("liveness probe must be TLS-aware (--tls)")
	}
	// Liveness must be a bare PING (not cluster-state dependent).
	if !probeHasArg(server.LivenessProbe, "PING") {
		t.Error("liveness must use PING")
	}
	if probeHasArg(server.LivenessProbe, "CLUSTER") {
		t.Error("liveness must NOT depend on cluster state")
	}
	// Volumes: config + acl + tls.
	if !hasVolume(sts.Spec.Template.Spec.Volumes, tlsVolName) {
		t.Error("missing tls volume")
	}
	if !hasVolume(sts.Spec.Template.Spec.Volumes, aclVolName) {
		t.Error("missing acl volume")
	}
}

func TestMergeContainerPatch(t *testing.T) {
	base := []corev1.Container{
		{Name: "server", Image: "base:1"},
		{Name: "metrics-exporter", Image: "exp:1"},
	}
	patches := []corev1.Container{
		{Name: "server", Image: "patched:2"},
		{Name: "extra", Image: "side:1"},
	}
	out, err := mergeContainerPatch(base, patches)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(out))
	}
	if out[0].Image != "patched:2" {
		t.Errorf("server image = %q, want patched:2", out[0].Image)
	}
	if out[2].Name != "extra" {
		t.Errorf("appended container = %q, want extra", out[2].Name)
	}
}

func TestApplyConfigHashAnnotationEmpty(t *testing.T) {
	tmpl := &corev1.PodTemplateSpec{}
	applyConfigHashAnnotation(tmpl, "")
	if tmpl.Annotations != nil {
		t.Error("empty hash must not set annotations")
	}
	applyConfigHashAnnotation(tmpl, "h1")
	if tmpl.Annotations[naming.AnnServerConfigHash] != "h1" {
		t.Errorf("annotation = %q", tmpl.Annotations[naming.AnnServerConfigHash])
	}
}

func TestApplyTLSHashAnnotationEmpty(t *testing.T) {
	tmpl := &corev1.PodTemplateSpec{}
	applyTLSHashAnnotation(tmpl, "")
	if tmpl.Annotations != nil {
		t.Error("empty tlsHash must not set annotations (no phantom roll)")
	}
	applyTLSHashAnnotation(tmpl, "t1")
	if tmpl.Annotations[naming.AnnTLSHash] != "t1" {
		t.Errorf("tls-hash annotation = %q, want t1", tmpl.Annotations[naming.AnnTLSHash])
	}
}

// TestTLSHashPropagatedToPodTemplate verifies the cluster-stamped naming.AnnTLSHash
// on the ValkeyNode object propagates onto the pod template so a real cert change
// rolls the workload through the config-hash roll machinery (07 §3.4).
func TestTLSHashPropagatedToPodTemplate(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Annotations = map[string]string{naming.AnnTLSHash: "tlshash-abc"}

	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatal(err)
	}
	if got := sts.Spec.Template.Annotations[naming.AnnTLSHash]; got != "tlshash-abc" {
		t.Errorf("pod-template tls-hash = %q, want tlshash-abc", got)
	}
}

// TestExporterCredEnvFrozenNames locks the FROZEN M5 contract: the exporter
// authenticates as _exporter via the REDIS_USER/REDIS_PASSWORD env vars, the
// password sourced from internal-<cluster>-system-passwords[key _exporter].
func TestExporterCredEnvFrozenNames(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: true}

	exp := buildExporterSidecar(node)
	if exp == nil {
		t.Fatal("exporter must be built when enabled")
	}
	envByName := map[string]corev1.EnvVar{}
	for _, e := range exp.Env {
		envByName[e.Name] = e
	}
	user, ok := envByName[naming.EnvExporterUser]
	if !ok || user.Value != naming.SystemUserExporter {
		t.Errorf("%s = %+v, want value %q", naming.EnvExporterUser, user, naming.SystemUserExporter)
	}
	pw, ok := envByName[naming.EnvExporterPassword]
	if !ok || pw.ValueFrom == nil || pw.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s must be a SecretKeyRef, got %+v", naming.EnvExporterPassword, pw)
	}
	ref := pw.ValueFrom.SecretKeyRef
	if ref.Name != naming.SystemPasswordsSecretName("mycluster") {
		t.Errorf("password secret = %q, want %q", ref.Name, naming.SystemPasswordsSecretName("mycluster"))
	}
	if ref.Key != naming.SystemUserExporter {
		t.Errorf("password key = %q, want %q", ref.Key, naming.SystemUserExporter)
	}
}

// TestExporterScrapeArgsTLS verifies the TLS-aware metrics-option seam: with TLS
// on, the exporter dials rediss:// with the shared CA mounted at the frozen
// naming.TLSMountPath; with TLS off it dials plain redis:// over loopback.
func TestExporterScrapeArgsTLS(t *testing.T) {
	plain := unitNode("mycluster-0-0")
	plain.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: true}
	expPlain := buildExporterSidecar(plain)
	if hasArg(expPlain.Args, "--redis.addr=redis://localhost:6379") == false {
		t.Errorf("plain exporter args = %v, want plain redis:// addr", expPlain.Args)
	}
	if len(expPlain.VolumeMounts) != 0 {
		t.Errorf("plain exporter must mount no TLS volume, got %v", expPlain.VolumeMounts)
	}

	tlsNode := unitNode("mycluster-0-0")
	tlsNode.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: true}
	tlsNode.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}
	expTLS := buildExporterSidecar(tlsNode)
	if !hasArg(expTLS.Args, "--redis.addr=rediss://localhost:6379") {
		t.Errorf("TLS exporter args = %v, want rediss:// addr", expTLS.Args)
	}
	if !hasArg(expTLS.Args, "--tls-ca-cert-file="+naming.TLSMountPath+"/"+naming.TLSSecretKeyCA) {
		t.Errorf("TLS exporter args = %v, want --tls-ca-cert-file at %s", expTLS.Args, naming.TLSMountPath)
	}
	var mounted bool
	for _, m := range expTLS.VolumeMounts {
		if m.Name == tlsVolName && m.MountPath == naming.TLSMountPath && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("TLS exporter must mount %s read-only, got %v", naming.TLSMountPath, expTLS.VolumeMounts)
	}
}

// --- helpers ---

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasPort(ports []corev1.ContainerPort, p int32) bool {
	for _, cp := range ports {
		if cp.ContainerPort == p {
			return true
		}
	}
	return false
}

func hasVolume(vols []corev1.Volume, name string) bool {
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}

func probeHasArg(p *corev1.Probe, arg string) bool {
	if p == nil || p.Exec == nil {
		return false
	}
	for _, a := range p.Exec.Command {
		if a == arg {
			return true
		}
		// readiness wraps in sh -c "<joined>"; check substring too.
		if containsWord(a, arg) {
			return true
		}
	}
	return false
}

func containsWord(s, w string) bool {
	return len(s) >= len(w) && (s == w || indexWord(s, w) >= 0)
}

func indexWord(s, w string) int {
	for i := 0; i+len(w) <= len(s); i++ {
		if s[i:i+len(w)] == w {
			return i
		}
	}
	return -1
}
