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
	"encoding/json"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// TestClusterRoundTrip marshals a fully-populated cluster to JSON and back and
// asserts equality, guarding against json-tag drift and lossy types.
func TestClusterRoundTrip(t *testing.T) {
	t.Parallel()
	sc := "fast"
	deadline := int64(120)
	orig := &v1.PerconaValkeyCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "valkey.percona.com/v1alpha1", Kind: "PerconaValkeyCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: v1.PerconaValkeyClusterSpec{
			CrVersion:        "1.0",
			Image:            "repo/valkey:1",
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "reg"}},
			Pause:            true,
			Mode:             v1.ModeCluster,
			Shards:           3,
			Replicas:         2,
			WorkloadType:     v1.WorkloadStatefulSet,
			NodeSelector:     map[string]string{"disk": "ssd"},
			Persistence: &v1.PersistenceSpec{
				Size:             resource.MustParse("50Gi"),
				StorageClassName: &sc,
				ReclaimPolicy:    v1.ReclaimRetain,
			},
			Config: map[string]string{"maxmemory": "1gb"},
			Users: []v1.UserACLSpec{{
				Name:           "app",
				Enabled:        true,
				PasswordSecret: v1.UserPasswordSecret{Name: "s", Keys: []string{"k1"}},
				Commands:       &v1.UserCommands{Allow: []string{"@read"}, Deny: []string{"flushall"}},
				Keys:           &v1.UserKeys{ReadWrite: []string{"app:*"}},
				Channels:       &v1.UserChannels{Patterns: []string{"news:*"}},
				Permissions:    "+ping",
			}},
			TLS:                 &v1.TLSConfig{SecretName: "tls"},
			Exporter:            v1.ExporterSpec{Enabled: true, Image: "exp:1"},
			PodDisruptionBudget: v1.PDBManaged,
			Backup: v1.BackupSpec{
				Image: "repo/backup:1",
				Storages: map[string]v1.BackupStorageSpec{
					"s3": {Type: v1.BackupStorageS3, S3: &v1.BackupStorageS3Spec{Bucket: "b", Region: "eu"}},
				},
				Schedule: []v1.BackupScheduleSpec{{Name: "n", Schedule: "0 2 * * *", StorageName: "s3", Keep: 7, Type: v1.BackupTypeFull}},
			},
			UpgradeOptions: v1.UpgradeOptions{Apply: v1.UpgradeApplyRecommended, Schedule: "0 4 * * *", VersionServiceEndpoint: "https://x"},
		},
		Status: v1.PerconaValkeyClusterStatus{
			State:       v1.StateReady,
			Host:        "valkey-c.ns.svc",
			Shards:      3,
			ReadyShards: 3,
			Conditions:  []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Time{Time: time.Date(2026, 6, 22, 2, 0, 0, 0, time.UTC)}}},
		},
	}
	_ = deadline

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got v1.PerconaValkeyCluster
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(orig.Spec, got.Spec) {
		t.Errorf("spec round-trip mismatch:\norig=%+v\ngot =%+v", orig.Spec, got.Spec)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	deadline := int64(3600)
	starting := int64(60)
	b := &v1.PerconaValkeyBackup{
		Spec: v1.PerconaValkeyBackupSpec{
			ClusterName:             "c",
			StorageName:             "s3",
			Type:                    v1.BackupTypeFull,
			Consistency:             v1.ConsistencyStrict,
			StartingDeadlineSeconds: &starting,
			ActiveDeadlineSeconds:   &deadline,
			Retention:               &v1.BackupRetentionSpec{Keep: 7, KeepAge: "168h"},
			ContainerOptions:        &v1.BackupContainerOptions{Args: []string{"--x"}, CompressionLevel: ptrI(3)},
		},
		Status: v1.PerconaValkeyBackupStatus{
			State:        v1.BackupStateSucceeded,
			Destination:  "s3://b/p/x",
			SlotCoverage: v1.SlotCoverageComplete,
			Shards:       []v1.ShardBackupStatus{{ShardIndex: 0, SlotRange: "0-5460", RDBObject: "x.rdb", SizeBytes: 1024, Checksum: "abc"}},
			// metav1.Time JSON-serializes to RFC3339 (second precision, UTC), so
			// use a UTC second-aligned timestamp for a lossless round-trip.
			Start: &metav1.Time{Time: time.Date(2026, 6, 22, 2, 0, 0, 0, time.UTC)},
		},
	}
	roundTrip(t, b, &v1.PerconaValkeyBackup{})

	r := &v1.PerconaValkeyRestore{
		Spec: v1.PerconaValkeyRestoreSpec{
			ClusterName:  "c",
			BackupSource: &v1.BackupSource{Destination: "s3://b/p/x", StorageName: "s3", S3: &v1.BackupStorageS3Spec{Bucket: "b"}},
			Strategy:     v1.RestoreStrategyNewCluster,
		},
		Status: v1.PerconaValkeyRestoreStatus{State: v1.RestoreStateRunning},
	}
	roundTrip(t, r, &v1.PerconaValkeyRestore{})

	n := &v1.ValkeyNode{
		Spec: v1.ValkeyNodeSpec{
			Image:               "repo/valkey:1",
			WorkloadType:        v1.WorkloadStatefulSet,
			Persistence:         &v1.PersistenceSpec{Size: resource.MustParse("10Gi")},
			ServerConfigMapName: "valkey-c",
			ServerConfigHash:    "deadbeef",
			ACLSecretName:       "internal-c-acl",
		},
		Status: v1.ValkeyNodeStatus{Ready: true, Role: v1.NodeRolePrimary, PodName: "p", PodIP: "1.2.3.4"},
	}
	roundTrip(t, n, &v1.ValkeyNode{})
}

// roundTrip marshals orig, unmarshals into `into`, then re-marshals `into` and
// asserts byte equality. Comparing the canonical JSON (rather than
// reflect.DeepEqual) avoids spurious failures from metav1.Time's time.Location
// pointer differing between UTC and the local zone after Unmarshal, while still
// catching any field/tag drift.
func roundTrip[T any](t *testing.T, orig, into *T) {
	t.Helper()
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal %T: %v", orig, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("unmarshal %T: %v", orig, err)
	}
	again, err := json.Marshal(into)
	if err != nil {
		t.Fatalf("re-marshal %T: %v", into, err)
	}
	if string(data) != string(again) {
		t.Errorf("%T round-trip mismatch:\nfirst =%s\nsecond=%s", orig, data, again)
	}
}

func ptrI(v int) *int { return &v }

// TestFieldPresence guards against accidental field drift vs the 03 catalogue by
// asserting the documented JSON tags exist on each spec/status type.
func TestFieldPresence(t *testing.T) {
	t.Parallel()
	checks := map[reflect.Type][]string{
		reflect.TypeOf(v1.PerconaValkeyClusterSpec{}): {
			"crVersion", "image", "imagePullSecrets", "pause", "mode", "shards", "replicas",
			"workloadType", "resources", "affinity", "nodeSelector", "tolerations",
			"topologySpreadConstraints", "persistence", "config", "containers", "users", "tls",
			"exporter", "podDisruptionBudget", "backup", "upgradeOptions",
		},
		reflect.TypeOf(v1.PerconaValkeyClusterStatus{}): {
			"state", "reason", "message", "host", "shards", "readyShards", "observedGeneration", "conditions",
		},
		reflect.TypeOf(v1.ValkeyNodeSpec{}): {
			"image", "imagePullSecrets", "workloadType", "persistence", "resources", "nodeSelector",
			"affinity", "tolerations", "topologySpreadConstraints", "exporter", "containers", "tls",
			"config", "serverConfigMapName", "serverConfigHash", "aclSecretName",
		},
		reflect.TypeOf(v1.ValkeyNodeStatus{}): {
			"observedGeneration", "ready", "podName", "podIP", "role", "conditions",
		},
		reflect.TypeOf(v1.PerconaValkeyBackupSpec{}): {
			"clusterName", "storageName", "type", "consistency", "startingDeadlineSeconds",
			"activeDeadlineSeconds", "retention", "containerOptions",
		},
		reflect.TypeOf(v1.PerconaValkeyBackupStatus{}): {
			"state", "stateDescription", "destination", "storageName", "s3", "gcs", "azure",
			"shards", "slotCoverage", "start", "completed", "valkeyVersion",
		},
		reflect.TypeOf(v1.PerconaValkeyRestoreSpec{}): {
			"clusterName", "backupName", "backupSource", "strategy",
		},
		reflect.TypeOf(v1.PerconaValkeyRestoreStatus{}): {
			"state", "stateDescription", "completed",
		},
	}
	for typ, tags := range checks {
		present := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			tag := typ.Field(i).Tag.Get("json")
			if tag == "" {
				continue
			}
			present[splitTag(tag)] = true
		}
		for _, tag := range tags {
			if !present[tag] {
				t.Errorf("%s is missing expected json field %q", typ.Name(), tag)
			}
		}
	}
}

func splitTag(tag string) string {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}

// TestDeepCopyNoAlias verifies the generated DeepCopy does not alias mutable
// fields (Quantity, slices, maps, pointers).
func TestDeepCopyNoAlias(t *testing.T) {
	t.Parallel()
	sc := "fast"
	orig := &v1.PerconaValkeyCluster{
		Spec: v1.PerconaValkeyClusterSpec{
			Config:      map[string]string{"a": "b"},
			Persistence: &v1.PersistenceSpec{Size: resource.MustParse("10Gi"), StorageClassName: &sc},
			Users:       []v1.UserACLSpec{{Name: "app"}},
		},
	}
	cp := orig.DeepCopy()
	cp.Spec.Config["a"] = "changed"
	cp.Spec.Users[0].Name = "other"
	*cp.Spec.Persistence.StorageClassName = "slow"
	if orig.Spec.Config["a"] != "b" {
		t.Error("DeepCopy aliased the config map")
	}
	if orig.Spec.Users[0].Name != "app" {
		t.Error("DeepCopy aliased the users slice")
	}
	if *orig.Spec.Persistence.StorageClassName != "fast" {
		t.Error("DeepCopy aliased the storageClassName pointer")
	}
}
