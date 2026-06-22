package naming

import "testing"

func TestNodeNameBuilders(t *testing.T) {
	cases := []struct {
		fn   string
		got  string
		want string
	}{
		{"NodeWorkloadName", NodeWorkloadName("mycluster-0-1"), "valkey-mycluster-0-1"},
		{"NodePVCName", NodePVCName("mycluster-0-1"), "valkey-mycluster-0-1-data"},
		{"NodeConfigMapName", NodeConfigMapName("mycluster-0-1"), "valkey-mycluster-0-1"},
		{"ClusterConfigMapName", ClusterConfigMapName("mycluster"), "valkey-mycluster"},
		{"HeadlessServiceName", HeadlessServiceName("mycluster"), "valkey-mycluster"},
		{"ACLSecretName", ACLSecretName("mycluster"), "internal-mycluster-acl"},
		{"SystemPasswordsSecretName", SystemPasswordsSecretName("mycluster"), "internal-mycluster-system-passwords"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.fn, c.got, c.want)
		}
	}
}

func TestAnnServerConfigHash(t *testing.T) {
	if AnnServerConfigHash != "valkey.percona.com/server-config-hash" {
		t.Errorf("AnnServerConfigHash = %q", AnnServerConfigHash)
	}
}

func TestNodeLabels(t *testing.T) {
	nodeLabels := map[string]string{
		LabelCluster:    "mycluster",
		LabelShardIndex: "0",
		LabelNodeIndex:  "1",
	}
	l := NodeLabels("mycluster-0-1", nodeLabels)
	if l[LabelCluster] != "mycluster" {
		t.Errorf("cluster label = %q", l[LabelCluster])
	}
	if l[LabelShardIndex] != "0" || l[LabelNodeIndex] != "1" {
		t.Errorf("shard/node labels = %q/%q", l[LabelShardIndex], l[LabelNodeIndex])
	}
	if l[LabelAppInstance] != "mycluster-0-1" {
		t.Errorf("app instance = %q, want node name", l[LabelAppInstance])
	}
	if l[LabelComponent] != ComponentValkey {
		t.Errorf("component = %q, want %q", l[LabelComponent], ComponentValkey)
	}
	// Mutating the returned map must not leak into a fresh build.
	l["x"] = "y"
	if _, ok := NodeLabels("mycluster-0-1", nodeLabels)["x"]; ok {
		t.Error("NodeLabels returned a shared map")
	}
}

func TestNodeLabelsFallbackToNodeName(t *testing.T) {
	l := NodeLabels("standalone", nil)
	if l[LabelCluster] != "standalone" {
		t.Errorf("cluster fallback = %q, want node name", l[LabelCluster])
	}
}

func TestNodeCluster(t *testing.T) {
	if got := NodeCluster("n", map[string]string{LabelCluster: "c"}); got != "c" {
		t.Errorf("NodeCluster label = %q, want c", got)
	}
	if got := NodeCluster("standalone", nil); got != "standalone" {
		t.Errorf("NodeCluster fallback = %q, want standalone", got)
	}
}

func TestSystemUserConstants(t *testing.T) {
	cases := map[string]string{
		"_operator": SystemUserOperator,
		"_exporter": SystemUserExporter,
		"_backup":   SystemUserBackup,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("system user const = %q, want %q", got, want)
		}
	}
}
