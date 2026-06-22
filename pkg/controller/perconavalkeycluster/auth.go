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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// Default-user auth / TLS-hardening constants (07 §3, ADR-008). These wire the
// frozen API fields (spec.auth, spec.tls.{ciphers,cipherSuites,dhParamsSecret})
// into the rendered valkey.conf. All secret material is Secret-ref only.
const (
	// defaultUserPasswordKey is the Secret key read for the default user's
	// requirepass when spec.auth.passwordSecret.keys is empty. The auth Secret is
	// a single-password Secret for the default user (multi-password rotation for
	// the default user is applied live via ACL SETUSER, not rendered here).
	defaultUserPasswordKey = "password"

	// defaultDHParamsKey is the Secret key holding the Diffie-Hellman params PEM
	// when spec.tls.dhParamsSecret.key is empty (07 §3.2 / shared_types SecretRef
	// doc). It is also the filename mounted under dhParamsMountPath and wired to
	// tls-dh-params-file.
	defaultDHParamsKey = "dh-params.pem"

	// dhParamsMountPath is the read-only mount point for the DH-params Secret in
	// every Valkey pod. The rendered tls-dh-params-file directive points under this
	// path; the matching VolumeMount is created by the node/resources builder leg
	// (see needsIntegrateWiring in the leg report). Kept distinct from the cert
	// mount (naming.TLSMountPath) so DH params and the cert family rotate
	// independently.
	dhParamsMountPath = "/etc/valkey/tls-dhparams"

	// usersSecretSuffix forms the operator-default default-user Secret name
	// (<cluster>-users) — mirrored from CheckNSetDefaults.setAuthDefaults (built
	// inline per the leaf rule; pkg/naming has no UsersSecretName helper). The auth
	// seam auto-generates the default-user password into this Secret only when the
	// cluster relies on this default name + default key (see ensureDefaultUserPassword).
	usersSecretSuffix = "-users"
)

// resolveRequirepass returns the cleartext default-user password the renderer
// emits as `requirepass` (07 §3 / gap §2.3). It implements the auth seam:
//
//   - auth disabled (spec.auth.enabled=false or block absent on the off path) =>
//     returns "" so no requirepass line is rendered (the default user is
//     passwordless / nopass, governed by the mounted aclfile).
//   - auth enabled, operator-default Secret (<cluster>-users + default "password"
//     key, no explicit keys[]) => the operator AUTO-GENERATES a strong password
//     into <cluster>-users if the key is absent, then reads it. This is what makes
//     "auth on by default" work out-of-the-box (matching the chart) without the
//     user pre-creating a Secret. An existing key is preserved (idempotent).
//   - auth enabled, bring-your-own Secret (custom name or explicit keys[]) => the
//     operator does NOT generate; it reads the user-owned Secret. When keys[] is
//     set the FIRST key is used (requirepass takes a single password; default-user
//     multi-password rotation is applied live via ACL SETUSER, not via this render).
//
// Fails CLOSED (returns a non-nil error) when auth is enabled but a BYO Secret or
// key is missing/empty, so a misconfigured cluster never silently renders a
// passwordless default user (07 §1, §8.2). Error messages reference Secret
// names/keys only — never the password value.
func (r *Reconciler) resolveRequirepass(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) (string, error) {
	auth := cluster.Spec.Auth
	if auth == nil || auth.Enabled == nil || !*auth.Enabled {
		return "", nil
	}

	secretName := auth.PasswordSecret.Name
	if secretName == "" {
		// CheckNSetDefaults normally fills this in; fail closed defensively rather
		// than read an empty name and render a passwordless default user.
		return "", fmt.Errorf("auth enabled but passwordSecret.name is empty")
	}

	key := defaultUserPasswordKey
	if len(auth.PasswordSecret.Keys) > 0 {
		key = auth.PasswordSecret.Keys[0]
	}

	// When the cluster relies on the operator-managed default Secret + default key,
	// auto-generate the password so auth-on-by-default needs no pre-created Secret.
	if r.usesOperatorDefaultAuthSecret(cluster, secretName, key) {
		pw, err := r.ensureDefaultUserPassword(ctx, cluster, secretName, key)
		if err != nil {
			return "", err
		}
		return pw, nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cluster.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("auth password Secret %q not found", secretName)
		}
		return "", fmt.Errorf("get auth password Secret %q: %w", secretName, err)
	}

	val, ok := secret.Data[key]
	if !ok || len(val) == 0 {
		return "", fmt.Errorf("auth password Secret %q missing key %q", secretName, key)
	}
	return string(val), nil
}

// usesOperatorDefaultAuthSecret reports whether the default-user auth uses the
// operator-managed default Secret (<cluster>-users) and default key ("password")
// with no explicit keys[] override. Only in that case does the operator
// auto-generate the password; any custom name or explicit keys[] means the user
// owns the Secret (bring-your-own) and the operator must read, never write, it.
func (r *Reconciler) usesOperatorDefaultAuthSecret(cluster *valkeyv1alpha1.PerconaValkeyCluster, secretName, key string) bool {
	return secretName == cluster.Name+usersSecretSuffix &&
		key == defaultUserPasswordKey &&
		len(cluster.Spec.Auth.PasswordSecret.Keys) == 0
}

// ensureDefaultUserPassword reads (or generates) the default-user password under
// key in the operator-managed <cluster>-users Secret, returning the cleartext for
// the requirepass render. An existing key value is preserved across reconciles so
// the rendered config is byte-stable and a user-supplied password is never
// clobbered; a new strong password (crypto/rand, passwordLength) is generated and
// persisted only when the key is absent/empty. The Secret is owner-referenced to
// the cluster. Cleartext is never logged. Mirrors ensureSystemPasswords (07 §4.3)
// but for the default user / requirepass.
func (r *Reconciler) ensureDefaultUserPassword(
	ctx context.Context,
	cluster *valkeyv1alpha1.PerconaValkeyCluster,
	secretName, key string,
) (string, error) {
	secret := &corev1.Secret{}
	secret.Name, secret.Namespace = secretName, cluster.Namespace

	var out string
	var genErr error
	_, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = naming.Labels(cluster.Name, naming.ComponentValkey)
		}
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if pw, ok := secret.Data[key]; ok && len(pw) > 0 {
			out = string(pw)
			return nil
		}
		pw, perr := randomPassword(passwordLength)
		if perr != nil {
			genErr = perr
			return perr
		}
		secret.Data[key] = []byte(pw)
		out = pw
		return nil
	})
	if genErr != nil {
		return "", genErr
	}
	if err != nil {
		return "", fmt.Errorf("ensure default-user password Secret %q: %w", secretName, err)
	}
	return out, nil
}

// tlsHardeningInput returns the TLS-hardening slice of the ConfigInput derived
// purely from spec.tls (no Secret read): authClients enum, cipher policy, and the
// resolved tls-dh-params-file path. Returns the zero value when TLS is off so the
// existing tls-*-file block (and the M1 minimal cluster) is unaffected. The DH
// params file path is the mount point + key (defaulted to dh-params.pem); the
// matching VolumeMount is the node/resources leg's follow-up (see leg report).
func tlsHardeningInput(cluster *valkeyv1alpha1.PerconaValkeyCluster) (authClients, ciphers, cipherSuites, dhParamsFile string) {
	tls := cluster.Spec.TLS
	if tls == nil {
		return "", "", "", ""
	}
	authClients = string(tls.AuthClients)
	ciphers = tls.Ciphers
	cipherSuites = tls.CipherSuites
	if tls.DHParamsSecret != nil && tls.DHParamsSecret.Name != "" {
		key := tls.DHParamsSecret.Key
		if key == "" {
			key = defaultDHParamsKey
		}
		dhParamsFile = dhParamsMountPath + "/" + key
	}
	return authClients, ciphers, cipherSuites, dhParamsFile
}

// applySecurityConfigInput enriches a base ConfigInput (built by configInput from
// the non-secret spec slice) with the security fields this leg owns: the resolved
// default-user requirepass, the dangerous-command disable list, and the TLS
// hardening directives. It is the single auth/security seam the ConfigMap builder
// calls so the requirepass Secret read and the spec->directive mapping stay in
// one place. Returns the enriched copy (ConfigInput is a value type; the input is
// not mutated) so the caller renders one consistent valkey.conf.
//
// The caller (upsertConfigMap) re-applies CheckNSetDefaults before building the
// base ConfigInput so cluster.Spec carries the fully-materialized security
// defaults (see ensureConfigDefaults for the round-trip rationale); this seam can
// therefore read cluster.Spec.{Auth,DisableCommands,TLS} directly.
func (r *Reconciler) applySecurityConfigInput(
	ctx context.Context,
	cluster *valkeyv1alpha1.PerconaValkeyCluster,
	in valkey.ConfigInput,
) (valkey.ConfigInput, error) {
	requirepass, err := r.resolveRequirepass(ctx, cluster)
	if err != nil {
		return valkey.ConfigInput{}, err
	}
	in.Requirepass = requirepass

	in.DisableCommands = append([]string(nil), cluster.Spec.DisableCommands...)

	in.TLSAuthClients, in.TLSCiphers, in.TLSCipherSuites, in.TLSDHParamsFile = tlsHardeningInput(cluster)

	return in, nil
}

// ensureConfigDefaults re-applies CheckNSetDefaults on the cluster before the
// config render. Reconcile defaults the CR in-memory at entry, but the earlier
// ensureFinalizers PATCH overwrites the in-memory object with the server's
// PERSISTED spec (controller-runtime writes the PATCH response back), and the new
// security fields (auth, disableCommands) use Go-side defaulting (not CRD markers)
// plus omitempty, so the persisted spec drops them back to nil — the round-trip
// footgun the charter warns about. Re-defaulting here makes the config-roll hash
// reflect the fully-defaulted spec deterministically (e.g. disableCommands =>
// [FLUSHALL,FLUSHDB]) so a node stamped on the first reconcile carries the SAME
// hash as every later pass and never enters an endless roll. CheckNSetDefaults is
// idempotent, so re-running it is a safe no-op for the already-defaulted subsystems.
func (r *Reconciler) ensureConfigDefaults(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	if err := cluster.CheckNSetDefaults(ctx, r.platform); err != nil {
		return fmt.Errorf("re-apply defaults for config render: %w", err)
	}
	return nil
}
