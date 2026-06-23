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
	"testing"

	corev1 "k8s.io/api/core/v1"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

// envByNameList indexes an env slice by name for assertions (last wins, matching
// Kubernetes' own last-wins env semantics).
func envByNameList(env []corev1.EnvVar) map[string]corev1.EnvVar {
	out := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		out[e.Name] = e
	}
	return out
}

// containerByName finds a container by name in a pod template.
func containerByName(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

// TestPodSecurityContextApplied verifies spec.podSecurityContext lands on the pod
// SecurityContext and spec.containerSecurityContext lands on BOTH the server and
// exporter container SecurityContexts (07 §6).
func TestPodSecurityContextApplied(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: true}

	runAsUser := int64(1001)
	fsGroup := int64(1001)
	node.Spec.PodSecurityContext = &corev1.PodSecurityContext{
		RunAsUser: &runAsUser,
		FSGroup:   &fsGroup,
	}
	allowEsc := false
	readOnlyRoot := true
	node.Spec.ContainerSecurityContext = &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowEsc,
		ReadOnlyRootFilesystem:   &readOnlyRoot,
	}

	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}

	psc := sts.Spec.Template.Spec.SecurityContext
	if psc == nil || psc.RunAsUser == nil || *psc.RunAsUser != runAsUser {
		t.Fatalf("pod SecurityContext runAsUser = %+v, want %d", psc, runAsUser)
	}
	if psc.FSGroup == nil || *psc.FSGroup != fsGroup {
		t.Errorf("pod SecurityContext fsGroup = %+v, want %d", psc.FSGroup, fsGroup)
	}

	server := containerByName(sts.Spec.Template.Spec.Containers, serverContainerName)
	if server == nil {
		t.Fatal("server container missing")
	}
	if server.SecurityContext == nil || server.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*server.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("server containerSecurityContext not applied: %+v", server.SecurityContext)
	}
	if server.SecurityContext.AllowPrivilegeEscalation == nil || *server.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("server allowPrivilegeEscalation must be false: %+v", server.SecurityContext)
	}

	exp := containerByName(sts.Spec.Template.Spec.Containers, exporterContainerName)
	if exp == nil {
		t.Fatal("exporter container missing")
	}
	if exp.SecurityContext == nil || exp.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*exp.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("exporter containerSecurityContext not applied: %+v", exp.SecurityContext)
	}
}

// TestSecurityContextDefaultsFSGroupWhenUnset verifies that when
// spec.podSecurityContext is unset the pod SecurityContext is materialized carrying
// ONLY the default fsGroup (998 — the percona/valkey image uid) so a CSI-provisioned
// /data is group-owned and writable, while every other pod SecurityContext field
// stays zero and the container SecurityContext remains nil (no phantom container
// defaults — let the API/PSA decide those).
func TestSecurityContextDefaultsFSGroupWhenUnset(t *testing.T) {
	node := unitNode("mycluster-0-0")
	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatal(err)
	}
	psc := sts.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatal("pod SecurityContext should default fsGroup when unset, got nil")
	}
	if psc.FSGroup == nil || *psc.FSGroup != defaultValkeyFSGroup {
		t.Errorf("pod SecurityContext fsGroup = %v, want %d", psc.FSGroup, defaultValkeyFSGroup)
	}
	if psc.RunAsUser != nil || psc.RunAsGroup != nil || psc.FSGroupChangePolicy != nil {
		t.Errorf("only fsGroup should be defaulted, got %+v", psc)
	}
	server := containerByName(sts.Spec.Template.Spec.Containers, serverContainerName)
	if server.SecurityContext != nil {
		t.Errorf("server SecurityContext should be nil when unset, got %+v", server.SecurityContext)
	}
}

// TestFSGroupUserValueWins verifies a user-supplied fsGroup is honoured verbatim
// (the default never clobbers it) and the rest of the user's pod SecurityContext is
// preserved.
func TestFSGroupUserValueWins(t *testing.T) {
	node := unitNode("mycluster-0-0")
	userFSGroup := int64(2000)
	runAsUser := int64(2001)
	node.Spec.PodSecurityContext = &corev1.PodSecurityContext{
		FSGroup:   &userFSGroup,
		RunAsUser: &runAsUser,
	}
	psc := podSecurityContext(node)
	if psc.FSGroup == nil || *psc.FSGroup != userFSGroup {
		t.Errorf("user fsGroup must win: got %v, want %d", psc.FSGroup, userFSGroup)
	}
	if psc.RunAsUser == nil || *psc.RunAsUser != runAsUser {
		t.Errorf("user runAsUser must be preserved: got %v", psc.RunAsUser)
	}
	// The builder must not mutate the caller's input.
	if node.Spec.PodSecurityContext.FSGroup == nil || *node.Spec.PodSecurityContext.FSGroup != userFSGroup {
		t.Error("podSecurityContext mutated the input spec")
	}
}

// TestServiceAccountWiring verifies spec.serviceAccountName -> pod
// ServiceAccountName and spec.automountServiceAccountToken -> pod
// AutomountServiceAccountToken (default false propagated by the API).
func TestServiceAccountWiring(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.ServiceAccountName = "valkey-sa"
	automount := false
	node.Spec.AutomountServiceAccountToken = &automount

	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatal(err)
	}
	podSpec := sts.Spec.Template.Spec
	if podSpec.ServiceAccountName != "valkey-sa" {
		t.Errorf("pod ServiceAccountName = %q, want valkey-sa", podSpec.ServiceAccountName)
	}
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %+v, want false", podSpec.AutomountServiceAccountToken)
	}
}

// TestAutomountNilWhenUnset verifies an unset automount pointer stays nil (the
// node never fabricates one; the API materializes the default).
func TestAutomountNilWhenUnset(t *testing.T) {
	node := unitNode("mycluster-0-0")
	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Spec.AutomountServiceAccountToken != nil {
		t.Errorf("AutomountServiceAccountToken should be nil when unset, got %+v",
			sts.Spec.Template.Spec.AutomountServiceAccountToken)
	}
	if sts.Spec.Template.Spec.ServiceAccountName != "" {
		t.Errorf("ServiceAccountName should be empty when unset, got %q",
			sts.Spec.Template.Spec.ServiceAccountName)
	}
}

// TestUserEnvAppendedAfterManaged verifies user env (spec.env + spec.extraEnvVars)
// is appended to the server container AFTER the operator-managed env, and that the
// managed POD_IP downward-API entry stays first and intact.
func TestUserEnvAppendedAfterManaged(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Env = map[string]string{"VALKEY_EXTRA": "1", "ANOTHER": "2"}
	node.Spec.ExtraEnvVars = []corev1.EnvVar{
		{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "s"},
				Key:                  "k",
			},
		}},
	}

	server := buildServerContainer(node)
	if len(server.Env) < 4 {
		t.Fatalf("expected managed + user env, got %d entries: %+v", len(server.Env), server.Env)
	}
	// Operator-managed POD_IP must be first.
	if server.Env[0].Name != envPodIP || server.Env[0].ValueFrom == nil ||
		server.Env[0].ValueFrom.FieldRef == nil {
		t.Fatalf("first env must be managed downward-API %s, got %+v", envPodIP, server.Env[0])
	}
	byName := envByNameList(server.Env)
	// env-map keys are sorted, so ANOTHER precedes VALKEY_EXTRA, both after POD_IP.
	if got := byName["VALKEY_EXTRA"]; got.Value != "1" {
		t.Errorf("VALKEY_EXTRA = %+v, want value 1", got)
	}
	if got := byName["ANOTHER"]; got.Value != "2" {
		t.Errorf("ANOTHER = %+v, want value 2", got)
	}
	if got, ok := byName["FROM_SECRET"]; !ok || got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil {
		t.Errorf("FROM_SECRET extraEnvVar not preserved: %+v", got)
	}
	// Determinism: the env-map keys must be in sorted order in the slice.
	idxAnother, idxValkey := -1, -1
	for i, e := range server.Env {
		switch e.Name {
		case "ANOTHER":
			idxAnother = i
		case "VALKEY_EXTRA":
			idxValkey = i
		}
	}
	if idxAnother == -1 || idxValkey == -1 || idxAnother > idxValkey {
		t.Errorf("env-map keys not in sorted order: ANOTHER@%d VALKEY_EXTRA@%d", idxAnother, idxValkey)
	}
}

// TestUserEnvCannotClobberReserved verifies a user attempt to override the
// operator-managed POD_IP is dropped — the managed downward-API value wins and
// remains the only POD_IP entry.
func TestUserEnvCannotClobberReserved(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Env = map[string]string{envPodIP: "10.0.0.99"}
	node.Spec.ExtraEnvVars = []corev1.EnvVar{
		{Name: envPodIP, Value: "192.168.1.1"},
		{Name: "SAFE", Value: "ok"},
	}

	server := buildServerContainer(node)
	count := 0
	for _, e := range server.Env {
		if e.Name == envPodIP {
			count++
			if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil {
				t.Errorf("%s must keep the managed downward-API source, got %+v", envPodIP, e)
			}
			if e.Value != "" {
				t.Errorf("%s must not carry a user literal value, got %q", envPodIP, e.Value)
			}
		}
	}
	if count != 1 {
		t.Fatalf("%s must appear exactly once (managed), got %d", envPodIP, count)
	}
	// Non-reserved user env must still pass through.
	if got := envByNameList(server.Env)["SAFE"]; got.Value != "ok" {
		t.Errorf("non-reserved SAFE env dropped: %+v", got)
	}
}

// TestMergeServerEnvNoUserEnv verifies the no-user-env case yields exactly the
// managed entries (no panic on nil maps/slices).
func TestMergeServerEnvNoUserEnv(t *testing.T) {
	managed := []corev1.EnvVar{{Name: envPodIP, Value: "x"}}
	out := mergeServerEnv(managed, nil, nil)
	if len(out) != 1 || out[0].Name != envPodIP {
		t.Fatalf("merge with no user env = %+v, want only managed", out)
	}
}

// TestExporterPortHonored verifies the exporter container port, the loopback
// listen-address arg and the readiness probe all use spec.exporter.port instead
// of the 9121 hardcode.
func TestExporterPortHonored(t *testing.T) {
	node := unitNode("mycluster-0-0")
	customPort := int32(19121)
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: true, Port: &customPort}

	exp := buildExporterSidecar(node)
	if exp == nil {
		t.Fatal("exporter must be built when enabled")
	}
	if !hasPort(exp.Ports, customPort) {
		t.Errorf("exporter container port = %+v, want %d", exp.Ports, customPort)
	}
	if hasPort(exp.Ports, portExporter) {
		t.Errorf("exporter must NOT use the 9121 default when port is set: %+v", exp.Ports)
	}
	if !hasArg(exp.Args, "--web.listen-address=:19121") {
		t.Errorf("exporter listen-address arg = %v, want :19121", exp.Args)
	}
	if exp.ReadinessProbe == nil || exp.ReadinessProbe.HTTPGet == nil {
		t.Fatal("exporter must have an HTTP readiness probe")
	}
	if got := exp.ReadinessProbe.HTTPGet.Port.IntValue(); got != int(customPort) {
		t.Errorf("readiness probe port = %d, want %d", got, customPort)
	}
}

// TestExporterPortDefaultsWhenNil verifies a nil spec.exporter.port falls back to
// the documented 9121 default (nil-safe for a standalone ValkeyNode).
func TestExporterPortDefaultsWhenNil(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: true}
	exp := buildExporterSidecar(node)
	if exp == nil {
		t.Fatal("exporter must be built when enabled")
	}
	if !hasPort(exp.Ports, portExporter) {
		t.Errorf("nil port must default to %d, got %+v", portExporter, exp.Ports)
	}
	if exp.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("readiness scheme = %q, want HTTP when exporter TLS off", exp.ReadinessProbe.HTTPGet.Scheme)
	}
}

// TestExporterServingTLS verifies spec.exporter.tls.enabled wires the
// --tls-server-cert-file/--tls-server-key-file flags, mounts the shared TLS
// volume and switches the readiness probe to HTTPS — independent of the scrape
// (engine) TLS axis.
func TestExporterServingTLS(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{
		Enabled: true,
		TLS:     &valkeyv1alpha1.ExporterTLSSpec{Enabled: true},
	}
	// No engine TLS (spec.tls nil): serving TLS must still wire on its own.

	exp := buildExporterSidecar(node)
	if exp == nil {
		t.Fatal("exporter must be built when enabled")
	}
	if !hasArg(exp.Args, "--tls-server-cert-file="+naming.TLSMountPath+"/"+naming.TLSSecretKeyCert) {
		t.Errorf("missing --tls-server-cert-file: %v", exp.Args)
	}
	if !hasArg(exp.Args, "--tls-server-key-file="+naming.TLSMountPath+"/"+naming.TLSSecretKeyKey) {
		t.Errorf("missing --tls-server-key-file: %v", exp.Args)
	}
	// Scrape side must remain plain (engine TLS off).
	if !hasArg(exp.Args, "--redis.addr=redis://localhost:6379") {
		t.Errorf("scrape addr should stay plain redis:// when engine TLS off: %v", exp.Args)
	}
	var mounted bool
	for _, m := range exp.VolumeMounts {
		if m.Name == tlsVolName && m.MountPath == naming.TLSMountPath && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("serving TLS must mount %s read-only, got %v", naming.TLSMountPath, exp.VolumeMounts)
	}
	if exp.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Errorf("readiness scheme = %q, want HTTPS when exporter serves TLS", exp.ReadinessProbe.HTTPGet.Scheme)
	}
}

// TestExporterServingAndScrapeTLSSingleMount verifies that when BOTH axes are on
// (engine TLS scrape + exporter serving TLS) the shared TLS volume is mounted
// exactly once.
func TestExporterServingAndScrapeTLSSingleMount(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{
		Enabled: true,
		TLS:     &valkeyv1alpha1.ExporterTLSSpec{Enabled: true},
	}
	exp := buildExporterSidecar(node)
	count := 0
	for _, m := range exp.VolumeMounts {
		if m.Name == tlsVolName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("TLS volume must be mounted exactly once, got %d: %v", count, exp.VolumeMounts)
	}
	if !hasArg(exp.Args, "--redis.addr=rediss://localhost:6379") {
		t.Errorf("scrape addr should be rediss:// when engine TLS on: %v", exp.Args)
	}
	if !hasArg(exp.Args, "--tls-server-cert-file="+naming.TLSMountPath+"/"+naming.TLSSecretKeyCert) {
		t.Errorf("serving cert flag must be present: %v", exp.Args)
	}
}

// TestExporterContainerSecurityContext verifies the propagated
// containerSecurityContext also hardens the exporter sidecar.
func TestExporterContainerSecurityContext(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.Exporter = valkeyv1alpha1.ExporterSpec{Enabled: true}
	runAsNonRoot := true
	node.Spec.ContainerSecurityContext = &corev1.SecurityContext{RunAsNonRoot: &runAsNonRoot}

	exp := buildExporterSidecar(node)
	if exp.SecurityContext == nil || exp.SecurityContext.RunAsNonRoot == nil || !*exp.SecurityContext.RunAsNonRoot {
		t.Errorf("exporter SecurityContext.RunAsNonRoot not applied: %+v", exp.SecurityContext)
	}
}

// TestSortedKeysDeterministic locks the helper's ascending-order contract that
// keeps the env list (and hence the pod template) stable across reconciles.
func TestSortedKeysDeterministic(t *testing.T) {
	got := sortedKeys(map[string]string{"c": "", "a": "", "b": ""})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("sortedKeys len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sortedKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(sortedKeys(nil)) != 0 {
		t.Error("sortedKeys(nil) must be empty")
	}
}

// TestDHParamsMountWhenConfigured verifies that a propagated
// spec.tls.dhParamsSecret yields both a DH-params Secret volume and a read-only
// mount at dhParamsMountPath on the server container, so the cluster-side
// tls-dh-params-file directive resolves and the pod does not crash-loop.
func TestDHParamsMountWhenConfigured(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{
		SecretName:     "tls",
		DHParamsSecret: &valkeyv1alpha1.SecretRef{Name: "dh", Key: "dh-params.pem"},
	}

	vols := buildVolumes(node)
	if !hasVolume(vols, dhParamsVolName) {
		t.Errorf("DH-params volume %q missing, got %v", dhParamsVolName, vols)
	}
	var src *corev1.SecretVolumeSource
	for i := range vols {
		if vols[i].Name == dhParamsVolName {
			src = vols[i].Secret
		}
	}
	if src == nil || src.SecretName != "dh" {
		t.Errorf("DH-params volume must source Secret %q, got %+v", "dh", src)
	}

	c := buildServerContainer(node)
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == dhParamsVolName {
			if m.MountPath != dhParamsMountPath {
				t.Errorf("DH-params mount path = %q, want %q", m.MountPath, dhParamsMountPath)
			}
			if !m.ReadOnly {
				t.Error("DH-params mount must be read-only")
			}
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("server container must mount DH-params volume, got %v", c.VolumeMounts)
	}
}

// TestDHParamsMountAbsentWithoutSecret verifies no DH-params volume/mount is
// emitted when TLS is on but no dhParamsSecret is configured (no phantom mount).
func TestDHParamsMountAbsentWithoutSecret(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}

	if hasVolume(buildVolumes(node), dhParamsVolName) {
		t.Error("DH-params volume must be absent when dhParamsSecret is nil")
	}
	for _, m := range buildServerContainer(node).VolumeMounts {
		if m.Name == dhParamsVolName {
			t.Error("DH-params mount must be absent when dhParamsSecret is nil")
		}
	}
}

// TestDHParamsSecretName locks the helper's contract: "" when TLS is off or no
// DH params are configured, the Secret name otherwise.
func TestDHParamsSecretName(t *testing.T) {
	off := unitNode("mycluster-0-0")
	if got := dhParamsSecretName(off); got != "" {
		t.Errorf("dhParamsSecretName(TLS off) = %q, want empty", got)
	}

	noDH := unitNode("mycluster-0-0")
	noDH.Spec.TLS = &valkeyv1alpha1.TLSConfig{SecretName: "tls"}
	if got := dhParamsSecretName(noDH); got != "" {
		t.Errorf("dhParamsSecretName(no dhParamsSecret) = %q, want empty", got)
	}

	withDH := unitNode("mycluster-0-0")
	withDH.Spec.TLS = &valkeyv1alpha1.TLSConfig{
		SecretName:     "tls",
		DHParamsSecret: &valkeyv1alpha1.SecretRef{Name: "dh"},
	}
	if got := dhParamsSecretName(withDH); got != "dh" {
		t.Errorf("dhParamsSecretName(with dhParamsSecret) = %q, want %q", got, "dh")
	}
}

// cmdContains reports whether the argv contains flag immediately followed by value.
func cmdContains(cmd []string, flag, value string) bool {
	for i := 0; i+1 < len(cmd); i++ {
		if cmd[i] == flag && cmd[i+1] == value {
			return true
		}
	}
	return false
}

// TestAnnounceFallsBackToPodIP verifies that with no external announce address the
// server boots with --cluster-announce-ip $(POD_IP) and no --cluster-announce-port
// (the in-cluster downward-API default).
func TestAnnounceFallsBackToPodIP(t *testing.T) {
	node := unitNode("mycluster-0-0")
	cmd := buildServerContainer(node).Command
	if !cmdContains(cmd, "--cluster-announce-ip", "$("+envPodIP+")") {
		t.Errorf("expected POD_IP announce fallback, got %v", cmd)
	}
	if hasArg(cmd, "--cluster-announce-port") {
		t.Errorf("no announce-port expected without an external address, got %v", cmd)
	}
}

// TestAnnounceExternalHostAndPort verifies spec.announceHost/announcePort render
// --cluster-announce-ip <host> --cluster-announce-port <port> (the expose.perPod
// external address) instead of the POD_IP fallback.
func TestAnnounceExternalHostAndPort(t *testing.T) {
	node := unitNode("mycluster-0-0")
	port := int32(31999)
	node.Spec.AnnounceHost = "203.0.113.5"
	node.Spec.AnnouncePort = &port
	cmd := buildServerContainer(node).Command
	if !cmdContains(cmd, "--cluster-announce-ip", "203.0.113.5") {
		t.Errorf("expected external announce host, got %v", cmd)
	}
	if !cmdContains(cmd, "--cluster-announce-port", "31999") {
		t.Errorf("expected external announce port, got %v", cmd)
	}
	if hasArg(cmd, "$("+envPodIP+")") {
		t.Errorf("POD_IP must not be announced when an external host is set, got %v", cmd)
	}
}

// TestAnnounceHostWithoutPort verifies an announceHost with a nil announcePort
// advertises the host with no explicit port (the engine uses the default client
// port).
func TestAnnounceHostWithoutPort(t *testing.T) {
	node := unitNode("mycluster-0-0")
	node.Spec.AnnounceHost = "lb.example.com"
	cmd := buildServerContainer(node).Command
	if !cmdContains(cmd, "--cluster-announce-ip", "lb.example.com") {
		t.Errorf("expected external announce host, got %v", cmd)
	}
	if hasArg(cmd, "--cluster-announce-port") {
		t.Errorf("no announce-port expected when announcePort is nil, got %v", cmd)
	}
}

// TestRestoreSeedInitContainerInjected verifies that when spec.restoreFrom is set
// the pod template carries the restore-seed init container (running
// /valkey-backup --download --shard=<i> into the data volume) and the server boots
// with --appendonly no so the seeded dump.rdb is loaded (CR-8 / 06 §7.4).
func TestRestoreSeedInitContainerInjected(t *testing.T) {
	node := unitNode("mycluster-2-0")
	node.Spec.Persistence = &valkeyv1alpha1.PersistenceSpec{}
	node.Spec.RestoreFrom = &valkeyv1alpha1.RestoreSource{
		Storage:     "s3-primary",
		BackupName:  "nightly-full",
		ShardIndex:  2,
		ClusterName: "sourcecluster",
		StorageSpec: &valkeyv1alpha1.BackupStorageSpec{
			Type: valkeyv1alpha1.BackupStorageS3,
			S3: &valkeyv1alpha1.BackupStorageS3Spec{
				Bucket:            "my-backups",
				Prefix:            "pvk",
				Region:            "us-east-1",
				EndpointURL:       "http://minio.minio.svc:9000",
				CredentialsSecret: "minio-creds",
			},
		},
		CredentialsSecret: "minio-creds",
	}
	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}

	seed := containerByName(sts.Spec.Template.Spec.InitContainers, restoreSeedContainerName)
	if seed == nil {
		t.Fatal("restore-seed init container missing when restoreFrom is set")
	}
	if !hasArg(seed.Command, "--download") || !hasArg(seed.Command, "--shard=2") {
		t.Errorf("restore-seed command = %v, want --download --shard=2", seed.Command)
	}
	if seed.Image != node.Spec.Image {
		t.Errorf("restore-seed image = %q, want the engine image %q", seed.Image, node.Spec.Image)
	}
	var mountsData bool
	for _, m := range seed.VolumeMounts {
		if m.MountPath == dataMountPath {
			mountsData = true
		}
	}
	if !mountsData {
		t.Errorf("restore-seed must mount the data volume at %s, got %+v", dataMountPath, seed.VolumeMounts)
	}

	// CR-8 regression guard: the seed container MUST carry the VALKEY_BACKUP_* env
	// (so cmd/valkey-backup --download can locate the shard RDB) and the creds Secret
	// via EnvFrom (so it authenticates to the backend). A bare container with only the
	// --download/--shard flags downloads nothing — the bug that blocked live restore.
	envByName := map[string]string{}
	for _, e := range seed.Env {
		envByName[e.Name] = e.Value
	}
	wantEnv := map[string]string{
		"VALKEY_BACKUP_CLUSTER":      "sourcecluster",
		"VALKEY_BACKUP_NAME":         "nightly-full",
		"VALKEY_BACKUP_STORAGE_TYPE": "s3",
		"VALKEY_BACKUP_S3_BUCKET":    "my-backups",
		"VALKEY_BACKUP_S3_PREFIX":    "pvk",
		"VALKEY_BACKUP_S3_ENDPOINT":  "http://minio.minio.svc:9000",
	}
	for k, want := range wantEnv {
		if got := envByName[k]; got != want {
			t.Errorf("restore-seed env %s = %q, want %q (seed env: %+v)", k, got, want, seed.Env)
		}
	}
	var hasCreds bool
	for _, ef := range seed.EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == "minio-creds" {
			hasCreds = true
		}
	}
	if !hasCreds {
		t.Errorf("restore-seed must mount the credentials Secret via EnvFrom, got %+v", seed.EnvFrom)
	}

	server := containerByName(sts.Spec.Template.Spec.Containers, serverContainerName)
	if !cmdContains(server.Command, "--appendonly", "no") {
		t.Errorf("seed boot must use --appendonly no, got %v", server.Command)
	}
}

// TestNoInitContainerWithoutRestore verifies a normal (non-restore) node has no
// init container and boots with appendonly governed by config (no --appendonly no).
func TestNoInitContainerWithoutRestore(t *testing.T) {
	node := unitNode("mycluster-0-0")
	labels := naming.NodeLabels(node.Name, node.Labels)
	sts, err := buildStatefulSet(node, labels)
	if err != nil {
		t.Fatal(err)
	}
	if len(sts.Spec.Template.Spec.InitContainers) != 0 {
		t.Errorf("no init containers expected without restoreFrom, got %+v", sts.Spec.Template.Spec.InitContainers)
	}
	server := containerByName(sts.Spec.Template.Spec.Containers, serverContainerName)
	if cmdContains(server.Command, "--appendonly", "no") {
		t.Errorf("non-restore node must not force --appendonly no, got %v", server.Command)
	}
}
