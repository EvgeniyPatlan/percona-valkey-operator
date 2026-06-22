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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// aclTestScheme builds a scheme with core + the operator API for the fake client.
func aclTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := valkeyv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add valkey scheme: %v", err)
	}
	return s
}

// newACLReconciler builds a Reconciler wired with a fake client (seeded with the
// given objects), the operator scheme, and a buffered fake recorder. The cluster
// CR itself must be among objs so CreateOrUpdate can set an owner reference.
func newACLReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	s := aclTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Reconciler{
		Client:   c,
		scheme:   s,
		recorder: events.NewFakeRecorder(200),
	}, c
}

// aclTestNS is the single namespace all ACL test fixtures live in.
const aclTestNS = "ns"

// userSecret builds an Opaque Secret holding the given key->password entries.
func userSecret(name string, data map[string]string) *corev1.Secret {
	d := map[string][]byte{}
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: aclTestNS},
		Type:       corev1.SecretTypeOpaque,
		Data:       d,
	}
}

func aclTestCluster(name string) *valkeyv1alpha1.PerconaValkeyCluster {
	c := &valkeyv1alpha1.PerconaValkeyCluster{}
	c.Name, c.Namespace = name, aclTestNS
	c.Generation = 1
	return c
}

// TestFetchUserPasswordHashesMultiKey verifies multi-password rotation: each key
// in passwordSecret.keys (in declared order) yields one #<sha256-hex> token.
func TestFetchUserPasswordHashesMultiKey(t *testing.T) {
	t.Parallel()
	sec := userSecret("creds", map[string]string{"password": "p1", "password-next": "p2"})
	r, _ := newACLReconciler(t, sec)

	u := valkeyv1alpha1.UserACLSpec{
		Name:           "app",
		Enabled:        true,
		PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: "creds", Keys: []string{"password", "password-next"}},
	}
	hashes, err := r.fetchUserPasswordHashes(context.Background(), "ns", u)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	want := []string{valkey.HashACLPassword("p1"), valkey.HashACLPassword("p2")}
	if len(hashes) != 2 || hashes[0] != want[0] || hashes[1] != want[1] {
		t.Fatalf("hashes = %v, want %v", hashes, want)
	}
}

// TestFetchUserPasswordHashesDefaultKey verifies that an empty keys list defaults
// to a single key named after the user (03 §2.7).
func TestFetchUserPasswordHashesDefaultKey(t *testing.T) {
	t.Parallel()
	sec := userSecret("creds", map[string]string{"app": "secret"})
	r, _ := newACLReconciler(t, sec)

	u := valkeyv1alpha1.UserACLSpec{
		Name:           "app",
		Enabled:        true,
		PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: "creds"},
	}
	hashes, err := r.fetchUserPasswordHashes(context.Background(), "ns", u)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(hashes) != 1 || hashes[0] != valkey.HashACLPassword("secret") {
		t.Fatalf("default-key hashes = %v", hashes)
	}
}

// TestFetchUserPasswordHashesFailClosed verifies fail-closed behaviour: a missing
// Secret or a missing/empty key returns an error (so the user never renders a
// passwordless line) and the error never echoes a password value (07 §1, §8.2).
func TestFetchUserPasswordHashesFailClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		secrets []client.Object
		user    valkeyv1alpha1.UserACLSpec
		wantErr string
	}{
		{
			name:    "missing secret",
			secrets: nil,
			user: valkeyv1alpha1.UserACLSpec{
				Name: "app", Enabled: true,
				PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: "absent", Keys: []string{"password"}},
			},
			wantErr: "not found",
		},
		{
			name:    "missing key",
			secrets: []client.Object{userSecret("creds", map[string]string{"other": "v"})},
			user: valkeyv1alpha1.UserACLSpec{
				Name: "app", Enabled: true,
				PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: "creds", Keys: []string{"password"}},
			},
			wantErr: "missing key",
		},
		{
			name:    "empty key value",
			secrets: []client.Object{userSecret("creds", map[string]string{"password": ""})},
			user: valkeyv1alpha1.UserACLSpec{
				Name: "app", Enabled: true,
				PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: "creds", Keys: []string{"password"}},
			},
			wantErr: "missing key",
		},
		{
			name:    "empty secret name",
			secrets: nil,
			user: valkeyv1alpha1.UserACLSpec{
				Name: "app", Enabled: true,
				PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Keys: []string{"password"}},
			},
			wantErr: "passwordSecret.name is empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := newACLReconciler(t, tc.secrets...)
			_, err := r.fetchUserPasswordHashes(context.Background(), "ns", tc.user)
			if err == nil {
				t.Fatalf("expected fail-closed error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
			// The error must never leak a password value.
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaked a password value: %s", err.Error())
			}
		})
	}
}

// TestBuildUserDefinedACLLinesSkipsDisabledAndReserved verifies that disabled
// users add no line and that reserved (_-prefixed) names are skipped defensively
// even though CEL rejects them at admission.
func TestBuildUserDefinedACLLinesSkipsDisabledAndReserved(t *testing.T) {
	t.Parallel()
	cluster := aclTestCluster("c")
	cluster.Spec.Users = []valkeyv1alpha1.UserACLSpec{
		{Name: "enabled", Enabled: true, Nopass: true, Commands: &valkeyv1alpha1.UserCommands{Allow: []string{"@read"}}},
		{Name: "disabled", Enabled: false, Nopass: true},
		{Name: "_operator", Enabled: true, Nopass: true}, // reserved; must be skipped.
	}
	r, _ := newACLReconciler(t, cluster)

	lines, err := r.buildUserDefinedACLLines(context.Background(), cluster)
	if err != nil {
		t.Fatalf("build lines: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (enabled only), got %d: %v", len(lines), lines)
	}
	if lines[0] != "user enabled on nopass +@read" {
		t.Fatalf("unexpected line: %q", lines[0])
	}
}

// TestReconcileUsersACLCreatesSecretAndRenders is the integration test: it
// renders the internal-<cluster>-acl Secret (type valkey.io/acl) containing the
// system users (incl. the M5 _operator backup grants) plus a full user-defined
// ACL line with hashed passwords sourced from the user's Secret.
func TestReconcileUsersACLCreatesSecretAndRenders(t *testing.T) {
	t.Parallel()
	cluster := aclTestCluster("c1")
	cluster.Spec.Exporter.Enabled = true
	cluster.Spec.Users = []valkeyv1alpha1.UserACLSpec{{
		Name:           "app",
		Enabled:        true,
		PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: "app-creds", Keys: []string{"password"}},
		Commands:       &valkeyv1alpha1.UserCommands{Allow: []string{"@read", "@write"}, Deny: []string{"@admin"}},
		Keys:           &valkeyv1alpha1.UserKeys{ReadWrite: []string{"app:*"}},
		Channels:       &valkeyv1alpha1.UserChannels{Patterns: []string{"app.*"}},
	}}
	creds := userSecret("app-creds", map[string]string{"password": "s3cret"})
	r, c := newACLReconciler(t, cluster, creds)

	if err := r.reconcileUsersACL(context.Background(), cluster); err != nil {
		t.Fatalf("reconcileUsersACL: %v", err)
	}

	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: naming.ACLSecretName("c1"), Namespace: "ns"}, got); err != nil {
		t.Fatalf("get ACL Secret: %v", err)
	}
	if got.Type != aclSecretType {
		t.Fatalf("ACL Secret type = %q, want %q", got.Type, aclSecretType)
	}
	acl := string(got.Data[valkey.ACLFileKey])

	// System users present, with the M5 _operator backup grants (M4 unblock).
	if !strings.Contains(acl, "user _operator on #") {
		t.Fatalf("missing _operator line:\n%s", acl)
	}
	if !strings.Contains(acl, "+bgsave +sync +psync +replconf") {
		t.Fatalf("_operator missing M5 backup grants (M4 backup blocked):\n%s", acl)
	}
	if !strings.Contains(acl, "user _exporter on #") || !strings.Contains(acl, "user _backup on #") {
		t.Fatalf("missing system users:\n%s", acl)
	}

	// User-defined line rendered with the full grammar and the hashed password.
	wantUser := "user app on " + valkey.HashACLPassword("s3cret") +
		" ~app:* resetchannels &app.* +@read +@write -@admin"
	if !strings.Contains(acl, wantUser) {
		t.Fatalf("user-defined ACL line wrong:\nwant: %s\ngot:\n%s", wantUser, acl)
	}
	// Cleartext password must never appear.
	if strings.Contains(acl, "s3cret") {
		t.Fatalf("cleartext password leaked into ACL Secret:\n%s", acl)
	}

	// The system-passwords Secret was created with the three system users.
	sysPw := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: naming.SystemPasswordsSecretName("c1"), Namespace: "ns"}, sysPw); err != nil {
		t.Fatalf("get system-passwords Secret: %v", err)
	}
	for _, u := range []string{naming.SystemUserOperator, naming.SystemUserExporter, naming.SystemUserBackup} {
		if len(sysPw.Data[u]) != passwordLength {
			t.Fatalf("system password %q length = %d, want %d", u, len(sysPw.Data[u]), passwordLength)
		}
	}
}

// TestReconcileUsersACLRotation verifies the zero-downtime multi-password
// rotation scenario: adding a second password key re-renders the ACL with two
// #hashes for the user, and the rendered content changes (a real ACL change).
func TestReconcileUsersACLRotation(t *testing.T) {
	t.Parallel()
	cluster := aclTestCluster("rot")
	cluster.Spec.Users = []valkeyv1alpha1.UserACLSpec{{
		Name:           "app",
		Enabled:        true,
		PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: "app-creds", Keys: []string{"password"}},
		Commands:       &valkeyv1alpha1.UserCommands{Allow: []string{"@read"}},
	}}
	creds := userSecret("app-creds", map[string]string{"password": "old", "password-next": "new"})
	r, c := newACLReconciler(t, cluster, creds)

	// First reconcile: single password.
	if err := r.reconcileUsersACL(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	before := aclContent(t, c, "rot")
	if strings.Count(beforeUserLine(before), "#") != 1 {
		t.Fatalf("expected one password hash before rotation:\n%s", before)
	}

	// Add the second key to the user's rotation list and re-reconcile.
	cluster.Spec.Users[0].PasswordSecret.Keys = []string{"password", "password-next"}
	if err := r.reconcileUsersACL(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	after := aclContent(t, c, "rot")

	if after == before {
		t.Fatal("ACL content did not change after adding a rotation password")
	}
	line := beforeUserLine(after)
	if strings.Count(line, "#") != 2 {
		t.Fatalf("expected two password hashes after rotation:\n%s", line)
	}
	wantBoth := valkey.HashACLPassword("old") + " " + valkey.HashACLPassword("new")
	if !strings.Contains(line, wantBoth) {
		t.Fatalf("rotation line missing both hashes in order:\n%s", line)
	}
}

// TestReconcileUsersACLFailClosed verifies that a user referencing a missing
// password Secret fails the reconcile (the caller maps this to Degraded/
// UsersAclError), rather than silently rendering a passwordless ACL.
func TestReconcileUsersACLFailClosed(t *testing.T) {
	t.Parallel()
	cluster := aclTestCluster("fc")
	cluster.Spec.Users = []valkeyv1alpha1.UserACLSpec{{
		Name:           "app",
		Enabled:        true,
		PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: "absent", Keys: []string{"password"}},
	}}
	r, _ := newACLReconciler(t, cluster)

	err := r.reconcileUsersACL(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected fail-closed error for missing user Secret, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// aclContent fetches the rendered users.acl from the cluster's ACL Secret.
func aclContent(t *testing.T, c client.Client, cluster string) string {
	t.Helper()
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: naming.ACLSecretName(cluster), Namespace: "ns"}, got); err != nil {
		t.Fatalf("get ACL Secret: %v", err)
	}
	return string(got.Data[valkey.ACLFileKey])
}

// beforeUserLine returns the rendered `user app ...` line from a users.acl blob.
func beforeUserLine(acl string) string {
	for _, line := range strings.Split(acl, "\n") {
		if strings.HasPrefix(line, "user app ") {
			return line
		}
	}
	return ""
}
