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

package v1alpha1_test

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// fullCluster returns a cluster with EVERY optional pointer/slice/map field set,
// so the generated DeepCopyInto branches all execute (deepcopy coverage + the R5
// Quantity/pointer-aliasing guard).
func fullCluster() *v1.PerconaValkeyCluster {
	sc := "fast"
	return &v1.PerconaValkeyCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "valkey.percona.com/v1alpha1", Kind: "PerconaValkeyCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", Labels: map[string]string{"a": "b"}},
		Spec: v1.PerconaValkeyClusterSpec{
			CrVersion:        "1.0",
			Image:            "repo/valkey:1",
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "reg"}},
			Pause:            true,
			Mode:             v1.ModeCluster,
			Shards:           3,
			Replicas:         2,
			WorkloadType:     v1.WorkloadStatefulSet,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
			},
			Affinity:     &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
			NodeSelector: map[string]string{"disk": "ssd"},
			Tolerations:  []corev1.Toleration{{Key: "k", Operator: corev1.TolerationOpExists}},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "zone", WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}},
			}},
			Persistence: &v1.PersistenceSpec{Size: resource.MustParse("50Gi"), StorageClassName: &sc, ReclaimPolicy: v1.ReclaimRetain},
			Config:      map[string]string{"maxmemory": "1gb"},
			Containers:  []corev1.Container{{Name: "server"}},
			Users: []v1.UserACLSpec{{
				Name:           "app",
				Enabled:        true,
				PasswordSecret: v1.UserPasswordSecret{Name: "s", Keys: []string{"k1", "k2"}},
				Commands:       &v1.UserCommands{Allow: []string{"@read"}, Deny: []string{"flushall"}},
				Keys:           &v1.UserKeys{ReadWrite: []string{"a:*"}, ReadOnly: []string{"b:*"}, WriteOnly: []string{"c:*"}},
				Channels:       &v1.UserChannels{Patterns: []string{"news:*"}},
				Permissions:    "+ping",
			}},
			TLS:                 &v1.TLSConfig{CertManager: &v1.CertManagerSpec{IssuerRef: v1.IssuerRef{Name: "ca", Kind: v1.IssuerKindClusterIssuer}}},
			Exporter:            v1.ExporterSpec{Enabled: true, Image: "exp:1", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}},
			PodDisruptionBudget: v1.PDBManaged,
			Backup: v1.BackupSpec{
				Image:              "repo/backup:1",
				ServiceAccountName: "sa",
				Storages: map[string]v1.BackupStorageSpec{
					"s3":    {Type: v1.BackupStorageS3, S3: &v1.BackupStorageS3Spec{Bucket: "b", Prefix: "p", Region: "eu", EndpointURL: "https://x", CredentialsSecret: "cs"}},
					"gcs":   {Type: v1.BackupStorageGCS, GCS: &v1.BackupStorageGCSSpec{Bucket: "b", Prefix: "p", CredentialsSecret: "cs"}},
					"azure": {Type: v1.BackupStorageAzure, Azure: &v1.BackupStorageAzureSpec{Container: "c", Prefix: "p", CredentialsSecret: "cs"}},
				},
				Schedule: []v1.BackupScheduleSpec{{Name: "n", Schedule: "0 2 * * *", StorageName: "s3", Keep: 7, Type: v1.BackupTypeFull}},
			},
			UpgradeOptions: v1.UpgradeOptions{Apply: v1.UpgradeApplyRecommended, Schedule: "0 4 * * *", VersionServiceEndpoint: "https://x"},
		},
		Status: v1.PerconaValkeyClusterStatus{
			State: v1.StateReady, Reason: "OK", Message: "m", Host: "h", Shards: 3, ReadyShards: 3, ObservedGeneration: 1,
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}},
		},
	}
}

func fullNode() *v1.ValkeyNode {
	sc := "fast"
	return &v1.ValkeyNode{
		Spec: v1.ValkeyNodeSpec{
			Image:                     "repo/valkey:1",
			ImagePullSecrets:          []corev1.LocalObjectReference{{Name: "reg"}},
			WorkloadType:              v1.WorkloadStatefulSet,
			Persistence:               &v1.PersistenceSpec{Size: resource.MustParse("10Gi"), StorageClassName: &sc},
			Resources:                 corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
			NodeSelector:              map[string]string{"disk": "ssd"},
			Affinity:                  &corev1.Affinity{},
			Tolerations:               []corev1.Toleration{{Key: "k"}},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{MaxSkew: 1}},
			Exporter:                  v1.ExporterSpec{Enabled: true},
			Containers:                []corev1.Container{{Name: "server"}},
			TLS:                       &v1.TLSConfig{SecretName: "tls"},
			Config:                    map[string]string{"maxmemory": "1gb"},
			ServerConfigMapName:       "valkey-c",
			ServerConfigHash:          "deadbeef",
			ACLSecretName:             "internal-c-acl",
		},
		Status: v1.ValkeyNodeStatus{
			ObservedGeneration: 1, Ready: true, PodName: "p", PodIP: "1.2.3.4", Role: v1.NodeRolePrimary,
			Conditions: []metav1.Condition{{Type: v1.NodeConditionReady, Status: metav1.ConditionTrue, Reason: "OK"}},
		},
	}
}

func fullBackup() *v1.PerconaValkeyBackup {
	d := int64(3600)
	s := int64(60)
	return &v1.PerconaValkeyBackup{
		Spec: v1.PerconaValkeyBackupSpec{
			ClusterName: "c", StorageName: "s3", Type: v1.BackupTypeFull, Consistency: v1.ConsistencyStrict,
			StartingDeadlineSeconds: &s, ActiveDeadlineSeconds: &d,
			Retention:        &v1.BackupRetentionSpec{Keep: 7, KeepAge: "168h"},
			ContainerOptions: &v1.BackupContainerOptions{Args: []string{"--x"}, Env: []corev1.EnvVar{{Name: "E", Value: "v"}}, CompressionLevel: ptrI(3), ParallelShards: ptrI(2), PreferReplica: ptrB(true)},
		},
		Status: v1.PerconaValkeyBackupStatus{
			State: v1.BackupStateSucceeded, StateDescription: "done", Destination: "s3://b/p", StorageName: "s3",
			S3:           &v1.BackupStorageS3Spec{Bucket: "b"},
			GCS:          &v1.BackupStorageGCSSpec{Bucket: "b"},
			Azure:        &v1.BackupStorageAzureSpec{Container: "c"},
			Shards:       []v1.ShardBackupStatus{{ShardIndex: 0, SlotRange: "0-5460"}},
			SlotCoverage: v1.SlotCoverageComplete,
			Start:        &metav1.Time{Time: metav1.Now().Time},
			Completed:    &metav1.Time{Time: metav1.Now().Time},
		},
	}
}

func fullRestore() *v1.PerconaValkeyRestore {
	return &v1.PerconaValkeyRestore{
		Spec: v1.PerconaValkeyRestoreSpec{
			ClusterName:  "c",
			BackupSource: &v1.BackupSource{Destination: "s3://b/p", StorageName: "s3", S3: &v1.BackupStorageS3Spec{Bucket: "b"}, GCS: &v1.BackupStorageGCSSpec{Bucket: "b"}, Azure: &v1.BackupStorageAzureSpec{Container: "c"}},
			Strategy:     v1.RestoreStrategyNewCluster,
		},
		Status: v1.PerconaValkeyRestoreStatus{State: v1.RestoreStateRunning, Completed: &metav1.Time{Time: metav1.Now().Time}},
	}
}

func ptrB(v bool) *bool { return &v }

// TestDeepCopyExhaustive exercises the generated DeepCopy/DeepCopyInto for every
// kind and List type with all optional fields set, asserting the copy is equal
// to but independent of the original (a runtime.Object that does not alias).
func TestDeepCopyExhaustive(t *testing.T) {
	t.Parallel()
	cluster := fullCluster()
	node := fullNode()
	backup := fullBackup()
	restore := fullRestore()

	objs := []runtime.Object{
		cluster,
		&v1.PerconaValkeyClusterList{Items: []v1.PerconaValkeyCluster{*cluster, *cluster}},
		node,
		&v1.ValkeyNodeList{Items: []v1.ValkeyNode{*node}},
		backup,
		&v1.PerconaValkeyBackupList{Items: []v1.PerconaValkeyBackup{*backup}},
		restore,
		&v1.PerconaValkeyRestoreList{Items: []v1.PerconaValkeyRestore{*restore}},
	}
	for _, o := range objs {
		cp := o.DeepCopyObject()
		if !reflect.DeepEqual(o, cp) {
			t.Errorf("%T DeepCopyObject not equal to original", o)
		}
		if cp == o {
			t.Errorf("%T DeepCopyObject returned the same pointer", o)
		}
	}
}

// TestSubStructDeepCopy exercises the standalone DeepCopy() wrapper generated for
// every shared sub-struct, asserting each returns a non-nil, equal copy. This
// covers the generated wrappers the top-level DeepCopyObject path does not invoke
// directly.
func TestSubStructDeepCopy(t *testing.T) {
	t.Parallel()
	cl := fullCluster()
	bk := fullBackup()
	rs := fullRestore()
	nd := fullNode()

	// Each entry calls the type's DeepCopy() and DeepEqual-checks it.
	assertCopy := func(name string, orig, cp any) {
		if reflect.ValueOf(cp).IsNil() {
			t.Errorf("%s DeepCopy returned nil", name)
			return
		}
		if !reflect.DeepEqual(orig, cp) {
			t.Errorf("%s DeepCopy not equal to original", name)
		}
	}

	p := cl.Spec.Persistence
	assertCopy("PersistenceSpec", p, p.DeepCopy())
	tls := cl.Spec.TLS
	assertCopy("TLSConfig", tls, tls.DeepCopy())
	assertCopy("CertManagerSpec", tls.CertManager, tls.CertManager.DeepCopy())
	ir := &tls.CertManager.IssuerRef
	assertCopy("IssuerRef", ir, ir.DeepCopy())
	exp := cl.Spec.Exporter
	assertCopy("ExporterSpec", &exp, exp.DeepCopy())
	u := &cl.Spec.Users[0]
	assertCopy("UserACLSpec", u, u.DeepCopy())
	assertCopy("UserPasswordSecret", &u.PasswordSecret, u.PasswordSecret.DeepCopy())
	assertCopy("UserCommands", u.Commands, u.Commands.DeepCopy())
	assertCopy("UserKeys", u.Keys, u.Keys.DeepCopy())
	assertCopy("UserChannels", u.Channels, u.Channels.DeepCopy())
	uo := cl.Spec.UpgradeOptions
	assertCopy("UpgradeOptions", &uo, uo.DeepCopy())
	bs := cl.Spec.Backup
	assertCopy("BackupSpec", &bs, bs.DeepCopy())
	st := bs.Storages["s3"]
	assertCopy("BackupStorageSpec", &st, st.DeepCopy())
	assertCopy("BackupStorageS3Spec", st.S3, st.S3.DeepCopy())
	gcsSt := bs.Storages["gcs"]
	assertCopy("BackupStorageGCSSpec", gcsSt.GCS, gcsSt.GCS.DeepCopy())
	azSt := bs.Storages["azure"]
	assertCopy("BackupStorageAzureSpec", azSt.Azure, azSt.Azure.DeepCopy())
	sch := &bs.Schedule[0]
	assertCopy("BackupScheduleSpec", sch, sch.DeepCopy())

	assertCopy("BackupContainerOptions", bk.Spec.ContainerOptions, bk.Spec.ContainerOptions.DeepCopy())
	assertCopy("BackupRetentionSpec", bk.Spec.Retention, bk.Spec.Retention.DeepCopy())
	shard := &bk.Status.Shards[0]
	assertCopy("ShardBackupStatus", shard, shard.DeepCopy())

	assertCopy("BackupSource", rs.Spec.BackupSource, rs.Spec.BackupSource.DeepCopy())

	// Spec/Status standalone wrappers.
	assertCopy("PerconaValkeyClusterSpec", &cl.Spec, cl.Spec.DeepCopy())
	assertCopy("PerconaValkeyClusterStatus", &cl.Status, cl.Status.DeepCopy())
	assertCopy("ValkeyNodeSpec", &nd.Spec, nd.Spec.DeepCopy())
	assertCopy("ValkeyNodeStatus", &nd.Status, nd.Status.DeepCopy())
	assertCopy("PerconaValkeyBackupSpec", &bk.Spec, bk.Spec.DeepCopy())
	assertCopy("PerconaValkeyBackupStatus", &bk.Status, bk.Status.DeepCopy())
	assertCopy("PerconaValkeyRestoreSpec", &rs.Spec, rs.Spec.DeepCopy())
	assertCopy("PerconaValkeyRestoreStatus", &rs.Status, rs.Status.DeepCopy())

	// Top-level kind wrappers (DeepCopy, not DeepCopyObject).
	assertCopy("PerconaValkeyCluster", cl, cl.DeepCopy())
	assertCopy("ValkeyNode", nd, nd.DeepCopy())
	assertCopy("PerconaValkeyBackup", bk, bk.DeepCopy())
	assertCopy("PerconaValkeyRestore", rs, rs.DeepCopy())
}

// TestResourceHelper covers the Resource group-qualifier helper.
func TestResourceHelper(t *testing.T) {
	t.Parallel()
	gr := v1.Resource("perconavalkeyclusters")
	if gr.Group != "valkey.percona.com" || gr.Resource != "perconavalkeyclusters" {
		t.Errorf("Resource() = %+v, want group valkey.percona.com", gr)
	}
}
