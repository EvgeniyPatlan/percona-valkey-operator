package naming

import "testing"

func TestLabelKeysMatchCharter(t *testing.T) {
	cases := map[string]string{
		"LabelCluster":      "valkey.percona.com/cluster",
		"LabelShardIndex":   "valkey.percona.com/shard-index",
		"LabelNodeIndex":    "valkey.percona.com/node-index",
		"LabelComponent":    "valkey.percona.com/component",
		"LabelAppName":      "app.kubernetes.io/name",
		"LabelAppInstance":  "app.kubernetes.io/instance",
		"LabelAppComponent": "app.kubernetes.io/component",
		"LabelAppManagedBy": "app.kubernetes.io/managed-by",
	}
	got := map[string]string{
		"LabelCluster":      LabelCluster,
		"LabelShardIndex":   LabelShardIndex,
		"LabelNodeIndex":    LabelNodeIndex,
		"LabelComponent":    LabelComponent,
		"LabelAppName":      LabelAppName,
		"LabelAppInstance":  LabelAppInstance,
		"LabelAppComponent": LabelAppComponent,
		"LabelAppManagedBy": LabelAppManagedBy,
	}
	for name, want := range cases {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}

func TestFinalizerKeysMatchCharter(t *testing.T) {
	cases := map[string]string{
		"FinalizerDeletePodsInOrder": "valkey.percona.com/delete-pods-in-order",
		"FinalizerDeleteSSL":         "valkey.percona.com/delete-ssl",
		"FinalizerDeleteBackup":      "valkey.percona.com/delete-backup",
		"FinalizerPVCCleanup":        "valkey.percona.com/persistent-volume-cleanup",
	}
	got := map[string]string{
		"FinalizerDeletePodsInOrder": FinalizerDeletePodsInOrder,
		"FinalizerDeleteSSL":         FinalizerDeleteSSL,
		"FinalizerDeleteBackup":      FinalizerDeleteBackup,
		"FinalizerPVCCleanup":        FinalizerPVCCleanup,
	}
	for name, want := range cases {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}

func TestLabels(t *testing.T) {
	l := Labels("mycluster", "valkey")
	if l[LabelCluster] != "mycluster" {
		t.Errorf("Labels cluster = %q, want %q", l[LabelCluster], "mycluster")
	}
	if l[LabelAppInstance] != "mycluster" {
		t.Errorf("Labels app instance = %q, want %q", l[LabelAppInstance], "mycluster")
	}
	if l[LabelComponent] != "valkey" {
		t.Errorf("Labels component = %q, want %q", l[LabelComponent], "valkey")
	}
	if l[LabelAppManagedBy] != ManagedByValue {
		t.Errorf("Labels managed-by = %q, want %q", l[LabelAppManagedBy], ManagedByValue)
	}

	// Mutating the returned map must not affect a freshly built one.
	l["extra"] = "x"
	l2 := Labels("mycluster", "valkey")
	if _, ok := l2["extra"]; ok {
		t.Error("Labels() returned a shared/mutable map; want a fresh copy each call")
	}

	// Empty component must omit the component labels.
	noComp := Labels("c", "")
	if _, ok := noComp[LabelComponent]; ok {
		t.Error("empty component should omit LabelComponent")
	}
}
