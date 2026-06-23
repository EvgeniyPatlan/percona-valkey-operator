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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

// EventAuthReloaded is emitted when an in-place auth/ACL reload is applied to a
// running cluster (ACL LOAD + CONFIG SET masterauth on each reachable node),
// avoiding a pod roll for a user/password/grant change (07 §3, ADR-008).
const EventAuthReloaded = "AuthReloaded"

// annAuthSignature records the SHA-256 of the rendered auth material (the
// users.acl content + the default-user requirepass) that was LAST applied live to
// the running cluster. The in-place reload fires only when the freshly-rendered
// signature differs from this stamped value, so an unchanged reconcile is a
// genuine no-op (idempotent live reload). It is a controller-internal hint (not a
// pod-template annotation), so it lives here rather than in pkg/naming, mirroring
// the gateAnnotation pattern.
const annAuthSignature = "valkey.percona.com/auth-signature"

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

	// Resolve the default-user password (same value rendered as `requirepass` in
	// valkey.conf). The aclfile MUST carry a matching `default` line: once an
	// aclfile is loaded the engine ignores requirepass for the default user, so
	// without this the default user stays nopass and auth is silently not enforced
	// (07 §3 / gap §2.3). Empty => the renderer emits `user default on nopass ...`
	// so the file agrees with the absent requirepass line.
	defaultUserPassword, err := r.resolveRequirepass(ctx, cluster)
	if err != nil {
		return err
	}

	content := valkey.RenderUsersACL(cluster.Spec.Exporter.Enabled, passwords, userLines, defaultUserPassword)

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

	// Apply the rendered ACL/requirepass/masterauth live to a RUNNING cluster so a
	// user/password/grant change does not require a pod roll (07 §3, ADR-008). The
	// aclfile + requirepass + masterauth are deliberately excluded from the
	// config-roll hash (see ServerConfigRollHash), so without this in-place reload a
	// pure auth change would never reach the engine until the next unrelated roll.
	if err := r.reconcileAuthReload(ctx, cluster, content, defaultUserPassword); err != nil {
		return err
	}
	return nil
}

// reconcileAuthReload applies a changed ACL/requirepass/masterauth to a running
// cluster IN PLACE — issuing ACL LOAD (reload the just-rewritten aclfile) and
// CONFIG SET masterauth <pw> on each reachable node — instead of relying on a pod
// roll (07 §3, ADR-008). requirepass/masterauth and the aclfile are excluded from
// the config-roll hash precisely so the operator can rotate them live, so this is
// the seam that makes a pure auth change take effect without churn.
//
// It is idempotent and safe:
//
//   - It fires ONLY when the rendered auth signature (sha256 of the users.acl
//     content + the default-user password) differs from the value last applied,
//     stamped on annAuthSignature. An unchanged reconcile is a no-op (no commands).
//   - The FIRST time a signature is observed (annotation absent) it stamps the
//     signature WITHOUT issuing live commands: a freshly-created cluster's pods boot
//     with the correct mounted aclfile/config, so there is nothing to reload — only
//     a SUBSEQUENT change needs the live push.
//   - Nodes that are not reachable/ready (no podIP, dial/scrape failure) are
//     skipped, never failing the reconcile — the next pass retries the still-pending
//     signature for the nodes that have since come up.
//
// The signature is persisted via a metadata-only MergeFrom PATCH (never a full
// Update) so the omitempty-stripped spec is left byte-for-byte untouched (the
// charter's omitempty+defaults round-trip footgun, mirrored from ensureFinalizers).
func (r *Reconciler) reconcileAuthReload(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, aclContent, requirepass string,
) error {
	// No live client wired (unit-test path that exercises only the render): skip the
	// live reload entirely. The signature stamping below is also skipped so a later
	// integration pass with a real factory performs the first-observation stamp.
	if r.clientFactory == nil {
		return nil
	}

	signature := authSignature(aclContent, requirepass)
	last := cluster.Annotations[annAuthSignature]
	if last == signature {
		return nil // auth unchanged — nothing to reload.
	}

	// First observation: record the signature without a live push (pods already
	// booted with the correct material). Only a later change triggers a reload.
	firstObservation := last == ""
	if !firstObservation {
		switch err := r.liveReloadAuth(ctx, cluster, requirepass); {
		case errors.Is(err, errAuthReloadPending):
			// No node reachable yet — do NOT stamp; retry next pass. Not an error.
			return nil
		case err != nil:
			return err
		}
	}
	return r.stampAuthSignature(ctx, cluster, signature)
}

// liveReloadAuth issues ACL LOAD + CONFIG SET masterauth (and requirepass, kept in
// lockstep) against every reachable node of the cluster, skipping nodes that are
// not ready or unreachable. A per-node dial/command failure is logged and skipped
// rather than failing the reconcile, so one lagging pod never blocks the rest; the
// next reconcile retries because the signature stays unstamped until at least one
// node succeeds. Returns an error only when NO node could be reloaded AND nodes
// were expected (so a genuinely-down cluster is surfaced, not silently ignored).
func (r *Reconciler) liveReloadAuth(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, requirepass string,
) error {
	log := logf.FromContext(ctx)

	nodes, err := r.listClusterNodes(ctx, cluster)
	if err != nil {
		return fmt.Errorf("auth reload: list nodes: %w", err)
	}

	reachable, reloaded := 0, 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Status.PodIP == "" {
			continue // not ready yet; the next pass reloads it.
		}
		reachable++
		_, c, derr := r.clientFactory.ForNode(ctx, node)
		if derr != nil {
			log.V(1).Info("auth reload: dial failed, skipping node", "node", node.Name, "err", derr.Error())
			continue
		}
		if rerr := reloadNodeAuth(ctx, c, requirepass); rerr != nil {
			log.V(1).Info("auth reload: command failed, skipping node", "node", node.Name, "err", rerr.Error())
			_ = c.Close()
			continue
		}
		_ = c.Close()
		reloaded++
	}

	if reloaded == 0 {
		if reachable == 0 {
			// No node is reachable yet (cluster still coming up). Not an error: leave
			// the signature unstamped so the next pass retries when pods are ready.
			return errAuthReloadPending
		}
		return fmt.Errorf("auth reload: no reachable node accepted the live ACL/auth reload")
	}
	r.recorder.Eventf(cluster, nil, eventNormal, EventAuthReloaded, "AuthReload",
		"Applied auth/ACL change live to %d/%d node(s) (ACL LOAD + CONFIG SET masterauth)", reloaded, reachable)
	return nil
}

// errAuthReloadPending is the sentinel reconcileAuthReload uses to mean "no node
// is reachable yet": the signature is deliberately NOT stamped so the next pass
// retries the live reload once pods are up, without failing the reconcile.
var errAuthReloadPending = fmt.Errorf("auth reload pending: no reachable node")

// reloadNodeAuth applies the live auth change to ONE node: CONFIG SET masterauth
// (so a replica can still authenticate to its primary after the rotation) and
// ACL LOAD (re-read the just-rewritten aclfile, applying user/grant/password
// changes for every ACL user including the default user). masterauth is set
// BEFORE ACL LOAD so the replica link never observes a window where the default
// user requires a password the replica has not yet been told to present. An empty
// requirepass (auth disabled) clears masterauth to "" so the directive matches the
// nopass default user. Kept as a package function (no receiver) so it is unit-
// testable against a mock ClusterClient.
func reloadNodeAuth(ctx context.Context, c valkey.ClusterClient, requirepass string) error {
	if err := c.ConfigSet(ctx, "masterauth", requirepass); err != nil {
		return fmt.Errorf("CONFIG SET masterauth: %w", err)
	}
	if err := c.ACLLoad(ctx); err != nil {
		return err
	}
	return nil
}

// authSignature is the SHA-256 (hex) of the rendered auth material — the users.acl
// content plus the default-user requirepass — separated by a NUL so the two fields
// can never collide. It changes only on a real auth change (the aclfile bytes or
// the default password), so it is the trigger the in-place reload keys off.
func authSignature(aclContent, requirepass string) string {
	sum := sha256.Sum256([]byte(aclContent + "\x00" + requirepass))
	return hex.EncodeToString(sum[:])
}

// stampAuthSignature persists the last-applied auth signature onto the cluster via
// a metadata-only MergeFrom PATCH (never a full Update — that would round-trip the
// omitempty-stripped spec and re-default it; see ensureFinalizers). The in-memory
// object is updated too so the rest of the pass sees the stamped value.
func (r *Reconciler) stampAuthSignature(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, signature string,
) error {
	base := cluster.DeepCopy()
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[annAuthSignature] = signature

	patchTarget := cluster.DeepCopy()
	if err := r.Patch(ctx, patchTarget, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("stamp auth signature: %w", err)
	}
	cluster.ResourceVersion = patchTarget.ResourceVersion
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
