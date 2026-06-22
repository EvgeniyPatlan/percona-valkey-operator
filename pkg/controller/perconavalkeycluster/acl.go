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
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/k8s"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// aclSecretType is the Secret type for operator-managed ACL Secrets (04 §2.1
// step3 / 07 §4.3). The Secret carries one users.acl data key.
const aclSecretType = corev1.SecretType("valkey.io/acl")

// passwordLength is the length of a generated system-user password (07 §4.3).
const passwordLength = 26

// passwordAlphabet is the alphanumeric set for generated system passwords.
const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// systemUserPrefix marks reserved system-user names. user-defined names starting
// with it are rejected (CEL already enforces this; the renderer double-checks so
// a spec that slips past admission can never shadow a system user, 07 §4.3).
const systemUserPrefix = "_"

// EventUsersACLUpdated is emitted when the rendered ACL Secret's content changes
// on a subsequent reconcile (a real user/password/grant change). Declared here
// in the ACL leg (not status.go) to keep the M5 seams disjoint; the create-time
// event EventUsersACLCreated lives in status.go (M3 vocabulary).
const EventUsersACLUpdated = "UsersAclUpdated"

// reconcileUsersACL renders the internal-<cluster>-acl Secret (type
// valkey.io/acl) from the canonical _operator/_exporter/_backup system users
// (verbatim least-privilege, 07 §4.3) plus the full user-defined spec.users[]
// grammar (commands/keys/channels/passwords), filling each #<sha256-hex> slot
// from internal-<cluster>-system-passwords (created here with 26-char random
// passwords if absent). _exporter is rendered only when the exporter is enabled.
// 04 §2.1 step3.
//
// Fails CLOSED (returns a non-nil error → Degraded/UsersAclError via the caller)
// when a user's referenced password Secret/key is missing, so a misconfigured
// user never silently renders a passwordless ACL line (07 §1, §8.2). Error
// messages reference Secret names/keys only, never password values.
func (r *Reconciler) reconcileUsersACL(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	passwords, err := r.ensureSystemPasswords(ctx, cluster)
	if err != nil {
		return err
	}

	userLines, err := r.buildUserDefinedACLLines(ctx, cluster)
	if err != nil {
		return err
	}

	content := valkey.RenderUsersACL(cluster.Spec.Exporter.Enabled, passwords, userLines)

	secret := &corev1.Secret{}
	secret.Name, secret.Namespace = naming.ACLSecretName(cluster.Name), cluster.Namespace
	res, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, secret, func() error {
		secret.Labels = naming.Labels(cluster.Name, naming.ComponentValkey)
		secret.Type = aclSecretType
		secret.Data = map[string][]byte{valkey.ACLFileKey: []byte(content)}
		return nil
	})
	if err != nil {
		return err
	}
	switch res {
	case controllerutil.OperationResultCreated:
		r.recorder.Eventf(cluster, secret, eventNormal, EventUsersACLCreated, "CreateUsersAcl", "Created ACL Secret %s", secret.Name)
	case controllerutil.OperationResultUpdated:
		r.recorder.Eventf(cluster, secret, eventNormal, EventUsersACLUpdated, "UpdateUsersAcl", "Updated ACL Secret %s", secret.Name)
	}
	return nil
}

// buildUserDefinedACLLines renders the full `user <name> ...` line per enabled
// spec.users entry via valkey.BuildUserACL, fetching each user's password(s)
// from its passwordSecret and SHA-256-hashing them for multi-password rotation.
// Disabled users are skipped (they are not rendered at all rather than emitted
// `off`, matching M3 behaviour so a disabled user adds no line). Names starting
// with "_" are skipped defensively (reserved for system users; CEL rejects them
// at admission). The lines are sorted deterministically by RenderUsersACL.
func (r *Reconciler) buildUserDefinedACLLines(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) ([]string, error) {
	lines := make([]string, 0, len(cluster.Spec.Users))
	for i := range cluster.Spec.Users {
		u := cluster.Spec.Users[i]
		if !u.Enabled {
			continue
		}
		if strings.HasPrefix(u.Name, systemUserPrefix) {
			// Reserved for system users; CEL rejects this at admission. Skip rather
			// than render so a spec that slipped past admission cannot shadow a
			// system user line.
			continue
		}

		var pwHashes []string
		// resetpass/nopass do not consult the password Secret; only the default
		// (hashed-password) path needs to fetch credentials.
		if !u.Resetpass && !u.Nopass {
			hashes, err := r.fetchUserPasswordHashes(ctx, cluster.Namespace, u)
			if err != nil {
				return nil, err
			}
			pwHashes = hashes
		}

		lines = append(lines, valkey.BuildUserACL(u, pwHashes))
	}
	return lines, nil
}

// fetchUserPasswordHashes reads the user's password Secret and returns the
// "#<sha256-hex>" hash tokens for each referenced key, in the user's declared
// key order (deterministic) for multi-password rotation (07 §4.4). When
// passwordSecret.keys is empty it defaults to the single key equal to the
// secret name's user — per 03 §2.7 the keys default to [name]; here name = the
// user name. Fails CLOSED if the Secret or any referenced key is missing/empty
// so a misconfigured user never renders a passwordless line (07 §1, §8.2).
func (r *Reconciler) fetchUserPasswordHashes(ctx context.Context, namespace string, u valkeyv1alpha1.UserACLSpec) ([]string, error) {
	secretName := u.PasswordSecret.Name
	if secretName == "" {
		// Defaulting normally fills this in (CheckNSetDefaults), but fail closed
		// defensively rather than read an empty name.
		return nil, fmt.Errorf("user %q: passwordSecret.name is empty", u.Name)
	}

	keys := u.PasswordSecret.Keys
	if len(keys) == 0 {
		// Default to a single key named after the user (03 §2.7 keys default).
		keys = []string{u.Name}
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("user %q: password Secret %q not found", u.Name, secretName)
		}
		return nil, fmt.Errorf("user %q: get password Secret %q: %w", u.Name, secretName, err)
	}

	hashes := make([]string, 0, len(keys))
	for _, key := range keys {
		val, ok := secret.Data[key]
		if !ok || len(val) == 0 {
			return nil, fmt.Errorf("user %q: password Secret %q missing key %q", u.Name, secretName, key)
		}
		hashes = append(hashes, valkey.HashACLPassword(string(val)))
	}
	return hashes, nil
}

// ensureSystemPasswords reads (or creates) the internal-<cluster>-system-passwords
// Secret holding the random _operator/_exporter/_backup passwords, returning a
// name->cleartext map for the ACL renderer (which hashes them; cleartext is
// never written to the ACL Secret and never logged). Existing passwords are
// preserved across reconciles so the rendered ACL is byte-stable. 07 §4.3.
func (r *Reconciler) ensureSystemPasswords(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) (map[string]string, error) {
	secret := &corev1.Secret{}
	secret.Name = naming.SystemPasswordsSecretName(cluster.Name)
	secret.Namespace = cluster.Namespace

	out := map[string]string{}
	var genErr error
	_, err := k8s.CreateOrUpdate(ctx, r.Client, r.scheme, cluster, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = naming.Labels(cluster.Name, naming.ComponentValkey)
		}
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		for _, user := range []string{naming.SystemUserOperator, naming.SystemUserExporter, naming.SystemUserBackup} {
			if pw, ok := secret.Data[user]; ok && len(pw) > 0 {
				out[user] = string(pw)
				continue
			}
			pw, perr := randomPassword(passwordLength)
			if perr != nil {
				genErr = perr
				return perr
			}
			secret.Data[user] = []byte(pw)
			out[user] = pw
		}
		return nil
	})
	if genErr != nil {
		return nil, genErr
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// randomPassword returns a cryptographically random alphanumeric password of
// length n using crypto/rand (never math/rand for credentials).
func randomPassword(n int) (string, error) {
	buf := make([]byte, n)
	alphaLen := big.NewInt(int64(len(passwordAlphabet)))
	for i := range buf {
		idx, err := rand.Int(rand.Reader, alphaLen)
		if err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		buf[i] = passwordAlphabet[idx.Int64()]
	}
	return string(buf), nil
}
