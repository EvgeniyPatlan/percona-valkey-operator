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
	"k8s.io/apimachinery/pkg/types"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// authCluster builds a cluster CR with an enabled auth block referencing the
// given Secret name; keys is optional (nil => default "password" key).
func authCluster(name, secretName string, keys []string) *valkeyv1alpha1.PerconaValkeyCluster {
	enabled := true
	c := aclTestCluster(name)
	c.Spec.Auth = &valkeyv1alpha1.AuthSpec{
		Enabled: &enabled,
		PasswordSecret: valkeyv1alpha1.UserPasswordSecret{
			Name: secretName,
			Keys: keys,
		},
	}
	return c
}

// defaultAuthCluster builds a cluster relying on the operator-default auth Secret
// (<cluster>-users + default "password" key, no explicit keys[]).
func defaultAuthCluster(name string) *valkeyv1alpha1.PerconaValkeyCluster {
	return authCluster(name, name+usersSecretSuffix, nil)
}

func boolp(b bool) *bool { return &b }

// TestResolveRequirepass covers the auth seam: operator-default auto-generation,
// bring-your-own read, disabled/absent => empty, and fail-closed on a missing BYO
// Secret/key.
func TestResolveRequirepass(t *testing.T) {
	t.Parallel()

	t.Run("operator-default auto-generates a password when absent", func(t *testing.T) {
		t.Parallel()
		cluster := defaultAuthCluster("c1")
		r, c := newACLReconciler(t, cluster) // no Secret seeded.

		pw, err := r.resolveRequirepass(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(pw) != passwordLength {
			t.Fatalf("expected generated password of length %d, got %q", passwordLength, pw)
		}
		// The generated password is persisted under the default key and is stable.
		sec := &corev1.Secret{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "c1-users", Namespace: aclTestNS}, sec); err != nil {
			t.Fatalf("expected the operator to create the Secret: %v", err)
		}
		if string(sec.Data[defaultUserPasswordKey]) != pw {
			t.Fatalf("persisted password does not match returned value")
		}
		pw2, err := r.resolveRequirepass(context.Background(), cluster)
		if err != nil || pw2 != pw {
			t.Fatalf("auto-generated password not stable across reconciles: %q vs %q (err=%v)", pw2, pw, err)
		}
	})

	t.Run("operator-default preserves an existing user-supplied password", func(t *testing.T) {
		t.Parallel()
		cluster := defaultAuthCluster("c2")
		sec := userSecret("c2-users", map[string]string{defaultUserPasswordKey: "user-chosen"})
		r, _ := newACLReconciler(t, cluster, sec)

		pw, err := r.resolveRequirepass(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pw != "user-chosen" {
			t.Fatalf("operator clobbered a user-supplied password: got %q", pw)
		}
	})

	t.Run("bring-your-own uses first key when keys[] set", func(t *testing.T) {
		t.Parallel()
		cluster := authCluster("c3", "byo-secret", []string{"current", "previous"})
		sec := userSecret("byo-secret", map[string]string{"current": "now", "previous": "old"})
		r, _ := newACLReconciler(t, cluster, sec)

		pw, err := r.resolveRequirepass(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pw != "now" {
			t.Fatalf("expected first key value, got %q", pw)
		}
	})

}

// TestResolveRequirepassEdgeCases covers the disabled/nil short circuits and the
// bring-your-own fail-closed paths (split out so each test stays under the gocyclo
// budget).
func TestResolveRequirepassEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("auth disabled returns empty (no generation)", func(t *testing.T) {
		t.Parallel()
		cluster := defaultAuthCluster("c4")
		cluster.Spec.Auth.Enabled = boolp(false)
		r, c := newACLReconciler(t, cluster)

		pw, err := r.resolveRequirepass(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pw != "" {
			t.Fatalf("auth disabled must return empty, got %q", pw)
		}
		// No Secret should have been created.
		sec := &corev1.Secret{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "c4-users", Namespace: aclTestNS}, sec); err == nil {
			t.Fatal("auth disabled must not create the password Secret")
		}
	})

	t.Run("nil auth block returns empty", func(t *testing.T) {
		t.Parallel()
		cluster := aclTestCluster("c5")
		r, _ := newACLReconciler(t, cluster)

		pw, err := r.resolveRequirepass(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pw != "" {
			t.Fatalf("nil auth must return empty, got %q", pw)
		}
	})

	t.Run("bring-your-own fails closed on missing Secret", func(t *testing.T) {
		t.Parallel()
		cluster := authCluster("c6", "byo-missing", nil)
		r, _ := newACLReconciler(t, cluster) // Secret not seeded.

		_, err := r.resolveRequirepass(context.Background(), cluster)
		if err == nil {
			t.Fatal("expected error for missing BYO Secret")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("error should mention missing Secret: %v", err)
		}
	})

	t.Run("bring-your-own fails closed on missing key", func(t *testing.T) {
		t.Parallel()
		cluster := authCluster("c7", "byo-badkey", []string{"password"})
		sec := userSecret("byo-badkey", map[string]string{"other": "x"})
		r, _ := newACLReconciler(t, cluster, sec)

		_, err := r.resolveRequirepass(context.Background(), cluster)
		if err == nil {
			t.Fatal("expected error for missing key")
		}
		if !strings.Contains(err.Error(), "missing key") {
			t.Fatalf("error should mention missing key: %v", err)
		}
	})

	t.Run("fails closed on empty secret name", func(t *testing.T) {
		t.Parallel()
		cluster := authCluster("c8", "", nil)
		r, _ := newACLReconciler(t, cluster)

		_, err := r.resolveRequirepass(context.Background(), cluster)
		if err == nil {
			t.Fatal("expected error for empty passwordSecret.name")
		}
	})

	t.Run("does not leak the password value in errors", func(t *testing.T) {
		t.Parallel()
		cluster := authCluster("c9", "byo-leak", []string{"password"})
		sec := userSecret("byo-leak", map[string]string{"wrong": "do-not-leak"})
		r, _ := newACLReconciler(t, cluster, sec)

		_, err := r.resolveRequirepass(context.Background(), cluster)
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), "do-not-leak") {
			t.Fatalf("password value leaked into error: %v", err)
		}
	})
}

// TestTLSHardeningInput covers spec.tls -> directive-input mapping, including the
// DH-params file path resolution and the TLS-off short circuit.
func TestTLSHardeningInput(t *testing.T) {
	t.Parallel()

	t.Run("tls off returns all empty", func(t *testing.T) {
		t.Parallel()
		cluster := aclTestCluster("t1")
		ac, ci, cs, dh := tlsHardeningInput(cluster)
		if ac != "" || ci != "" || cs != "" || dh != "" {
			t.Fatalf("expected all empty for TLS off, got %q %q %q %q", ac, ci, cs, dh)
		}
	})

	t.Run("maps fields and resolves dh-params path with default key", func(t *testing.T) {
		t.Parallel()
		cluster := aclTestCluster("t2")
		cluster.Spec.TLS = &valkeyv1alpha1.TLSConfig{
			AuthClients:    valkeyv1alpha1.TLSAuthClientsRequire,
			Ciphers:        "DEFAULT:!MEDIUM",
			CipherSuites:   "TLS_AES_128_GCM_SHA256",
			DHParamsSecret: &valkeyv1alpha1.SecretRef{Name: "dh-secret"},
		}
		ac, ci, cs, dh := tlsHardeningInput(cluster)
		if ac != "require" {
			t.Fatalf("authClients: got %q", ac)
		}
		if ci != "DEFAULT:!MEDIUM" || cs != "TLS_AES_128_GCM_SHA256" {
			t.Fatalf("ciphers/cipherSuites: got %q %q", ci, cs)
		}
		if dh != dhParamsMountPath+"/"+defaultDHParamsKey {
			t.Fatalf("dh-params path: got %q", dh)
		}
	})

	t.Run("dh-params uses explicit key when set", func(t *testing.T) {
		t.Parallel()
		cluster := aclTestCluster("t3")
		cluster.Spec.TLS = &valkeyv1alpha1.TLSConfig{
			DHParamsSecret: &valkeyv1alpha1.SecretRef{Name: "dh-secret", Key: "custom.pem"},
		}
		_, _, _, dh := tlsHardeningInput(cluster)
		if dh != dhParamsMountPath+"/custom.pem" {
			t.Fatalf("dh-params path with explicit key: got %q", dh)
		}
	})

	t.Run("dh-params empty when secret name empty", func(t *testing.T) {
		t.Parallel()
		cluster := aclTestCluster("t4")
		cluster.Spec.TLS = &valkeyv1alpha1.TLSConfig{DHParamsSecret: &valkeyv1alpha1.SecretRef{}}
		_, _, _, dh := tlsHardeningInput(cluster)
		if dh != "" {
			t.Fatalf("expected empty dh path for empty secret name, got %q", dh)
		}
	})
}

// TestApplySecurityConfigInput verifies the seam enriches a base ConfigInput with
// the resolved requirepass, the disableCommands copy, and the TLS hardening
// fields, and fails closed when the requirepass Secret is missing.
func TestApplySecurityConfigInput(t *testing.T) {
	t.Parallel()

	t.Run("enriches base input end to end", func(t *testing.T) {
		t.Parallel()
		cluster := defaultAuthCluster("a1")
		cluster.Spec.DisableCommands = []string{"FLUSHALL", "FLUSHDB"}
		cluster.Spec.TLS = &valkeyv1alpha1.TLSConfig{AuthClients: valkeyv1alpha1.TLSAuthClientsRequire}
		sec := userSecret("a1-users", map[string]string{defaultUserPasswordKey: "pw"})
		r, _ := newACLReconciler(t, cluster, sec)

		base := valkey.ConfigInput{ACL: true, TLS: true}
		got, err := r.applySecurityConfigInput(context.Background(), cluster, base)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Requirepass != "pw" {
			t.Fatalf("requirepass not wired: %q", got.Requirepass)
		}
		if len(got.DisableCommands) != 2 {
			t.Fatalf("disableCommands not copied: %v", got.DisableCommands)
		}
		if got.TLSAuthClients != "require" {
			t.Fatalf("tls authClients not wired: %q", got.TLSAuthClients)
		}
		// Base fields preserved.
		if !got.ACL || !got.TLS {
			t.Fatalf("base fields lost: %+v", got)
		}
		// The enriched input renders a full, consistent valkey.conf.
		out := valkey.RenderServerConfig(got)
		for _, want := range []string{"requirepass pw", `rename-command FLUSHALL ""`, "tls-auth-clients yes"} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q in rendered config:\n%s", want, out)
			}
		}
	})

	t.Run("does not mutate caller input", func(t *testing.T) {
		t.Parallel()
		cluster := defaultAuthCluster("a2")
		sec := userSecret("a2-users", map[string]string{defaultUserPasswordKey: "pw"})
		r, _ := newACLReconciler(t, cluster, sec)

		base := valkey.ConfigInput{ACL: true}
		_, err := r.applySecurityConfigInput(context.Background(), cluster, base)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if base.Requirepass != "" || base.DisableCommands != nil {
			t.Fatalf("caller input was mutated: %+v", base)
		}
	})

	t.Run("fails closed when bring-your-own requirepass Secret missing", func(t *testing.T) {
		t.Parallel()
		cluster := authCluster("a3", "byo-a3", nil)
		r, _ := newACLReconciler(t, cluster) // no Secret.

		_, err := r.applySecurityConfigInput(context.Background(), cluster, valkey.ConfigInput{ACL: true})
		if err == nil {
			t.Fatal("expected error when auth password Secret missing")
		}
	})
}
