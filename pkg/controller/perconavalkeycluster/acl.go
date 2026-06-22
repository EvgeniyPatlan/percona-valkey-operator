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

	corev1 "k8s.io/api/core/v1"
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

// reconcileUsersACL renders the internal-<cluster>-acl Secret (type
// valkey.io/acl) from the canonical _operator/_exporter/_backup system users
// (verbatim least-privilege, 07 §4.3) plus a passthrough of spec.users, filling
// each #<sha256-hex> slot from internal-<cluster>-system-passwords (created here
// with 26-char random passwords if absent). _exporter is rendered only when the
// exporter is enabled. 04 §2.1 step3.
//
// Wave 2a passes spec.users through minimally (enabled users + their verbatim
// `permissions` rule string). The full user-defined ACL grammar (keys/channels/
// commands, multi-password rotation) is M5.
func (r *Reconciler) reconcileUsersACL(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	passwords, err := r.ensureSystemPasswords(ctx, cluster)
	if err != nil {
		return err
	}

	content := valkey.RenderUsersACL(cluster.Spec.Exporter.Enabled, passwords, userDefinedACLLines(cluster))

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
	if res == controllerutil.OperationResultCreated {
		r.recorder.Eventf(cluster, secret, eventNormal, EventUsersACLCreated, "CreateUsersAcl", "Created ACL Secret %s", secret.Name)
	}
	return nil
}

// userDefinedACLLines renders a minimal `user <name> on ... <permissions>` line
// per enabled spec.users entry (M5 will render the full grammar). Disabled users
// are skipped. The lines are sorted deterministically by RenderUsersACL.
func userDefinedACLLines(cluster *valkeyv1alpha1.PerconaValkeyCluster) []string {
	lines := make([]string, 0, len(cluster.Spec.Users))
	for i := range cluster.Spec.Users {
		u := cluster.Spec.Users[i]
		if !u.Enabled {
			continue
		}
		line := "user " + u.Name + " on"
		if u.Nopass {
			line += " nopass"
		}
		if u.Permissions != "" {
			line += " " + u.Permissions
		}
		lines = append(lines, line)
	}
	return lines
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
