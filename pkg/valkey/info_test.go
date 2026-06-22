package valkey

import "testing"

func TestParseInfoReplication(t *testing.T) {
	info := "# Replication\r\n" +
		"role:master\r\n" +
		"connected_slaves:1\r\n" +
		"slave0:ip=10.0.0.2,port=6379,state=online,offset=42,lag=0\r\n" +
		"master_repl_offset:42\r\n" +
		"\r\n" +
		"# Keyspace\r\n"
	got := ParseInfoReplication(info)
	if got[InfoKeyRole] != InfoRoleMaster {
		t.Errorf("role = %q, want master", got[InfoKeyRole])
	}
	if got["connected_slaves"] != "1" {
		t.Errorf("connected_slaves = %q", got["connected_slaves"])
	}
	if got["master_repl_offset"] != "42" {
		t.Errorf("master_repl_offset = %q", got["master_repl_offset"])
	}
	if _, ok := got["# Replication"]; ok {
		t.Error("comment header should be skipped")
	}
}

func TestParseInfoReplicationReplica(t *testing.T) {
	info := "role:slave\nmaster_link_status:up\nslave_repl_offset:100\n"
	got := ParseInfoReplication(info)
	if got[InfoKeyRole] != InfoRoleSlave {
		t.Errorf("role = %q, want slave", got[InfoKeyRole])
	}
	if got[InfoKeyMasterLinkStatus] != "up" {
		t.Errorf("master_link_status = %q, want up", got[InfoKeyMasterLinkStatus])
	}
}

func TestParseInfoReplicationMalformed(t *testing.T) {
	got := ParseInfoReplication("garbage-no-colon\n\nrole:master\n   \n")
	if len(got) != 1 || got[InfoKeyRole] != InfoRoleMaster {
		t.Errorf("malformed parse = %v, want only role:master", got)
	}
	if got := ParseInfoReplication(""); len(got) != 0 {
		t.Errorf("empty parse = %v, want empty", got)
	}
}

func TestAddress(t *testing.T) {
	if got := Address("10.0.0.5"); got != "10.0.0.5:6379" {
		t.Errorf("Address = %q, want 10.0.0.5:6379", got)
	}
}

func TestIsWrongPass(t *testing.T) {
	if !isWrongPass(errString("WRONGPASS invalid username-password pair")) {
		t.Error("expected WRONGPASS detection")
	}
	if isWrongPass(errString("connection refused")) {
		t.Error("non-WRONGPASS should not match")
	}
	if isWrongPass(nil) {
		t.Error("nil should not match")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
