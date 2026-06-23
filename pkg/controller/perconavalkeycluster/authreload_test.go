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

package perconavalkeycluster_test

import (
	"fmt"
	"strings"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeycluster"
)

// callsOfTypeWithArg returns the recorded calls of cmd whose arg contains sub —
// used to count CONFIG SET masterauth specifically (CONFIGSET arg = "key=value").
func (fc *fakeCluster) callsOfTypeWithArg(cmd, sub string) []recordedCall {
	var out []recordedCall
	for _, c := range fc.callsOfType(cmd) {
		if sub == "" || strings.Contains(c.arg, sub) {
			out = append(out, c)
		}
	}
	return out
}

// addUserWithSecret adds an enabled ACL user backed by a freshly-created password
// Secret to the cluster spec (a genuine auth/ACL change that must reload live).
func addUserWithSecret(ns string, key types.NamespacedName, user, secretName, password string) {
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"password": []byte(password)},
	}
	gomega.Expect(k8sClient.Create(testCtx, creds)).To(gomega.Succeed())

	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	gomega.Expect(k8sClient.Get(testCtx, key, cluster)).To(gomega.Succeed())
	cluster.Spec.Users = append(cluster.Spec.Users, valkeyv1alpha1.UserACLSpec{
		Name:           user,
		Enabled:        true,
		PasswordSecret: valkeyv1alpha1.UserPasswordSecret{Name: secretName, Keys: []string{"password"}},
		Commands:       &valkeyv1alpha1.UserCommands{Allow: []string{"@read"}},
	})
	gomega.Expect(k8sClient.Update(testCtx, cluster)).To(gomega.Succeed())
}

var _ = ginkgo.Describe("PerconaValkeyCluster in-place auth reload (LEG C)", func() {
	var (
		ns      string
		fc      *fakeCluster
		r       *perconavalkeycluster.Reconciler
		nsIndex int
	)

	ginkgo.BeforeEach(func() {
		nsIndex++
		ns = makeNamespace(fmt.Sprintf("pvk-authreload-%d", nsIndex))
		fc = newFakeCluster()
		r = perconavalkeycluster.NewReconcilerForTest(k8sClient, apiScheme, &fakeClientFactory{fc: fc})
	})

	ginkgo.It("does NOT issue ACL LOAD on the steady (no auth change) reconcile path", func() {
		cluster := makeCluster("noop", ns, 2)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		// Several extra steady passes with no spec change: the rendered auth is
		// identical, so the in-place reload must stay a no-op (no ACL LOAD ever).
		for i := 0; i < 5; i++ {
			reconcileOnce(r, key)
		}
		gomega.Expect(fc.callsOfType("ACLLOAD")).To(gomega.BeEmpty(),
			"steady reconcile must not reload ACLs (no auth change)")
		gomega.Expect(fc.callsOfTypeWithArg("CONFIGSET", "masterauth")).To(gomega.BeEmpty(),
			"steady reconcile must not re-set masterauth (no auth change)")
	})

	ginkgo.It("applies a user/ACL change live (ACL LOAD + CONFIG SET masterauth) without a roll", func() {
		cluster := makeCluster("reload", ns, 2)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		// Capture the per-node config hash before the auth change: an auth-only
		// change must NOT roll the pods (the hash is unchanged).
		hashBefore := firstNodeConfigHash(ns)
		gomega.Expect(hashBefore).NotTo(gomega.BeEmpty())

		aclLoadsBefore := len(fc.callsOfType("ACLLOAD"))
		gomega.Expect(aclLoadsBefore).To(gomega.Equal(0), "no reload before any auth change")

		// A genuine ACL change: add a new enabled user.
		addUserWithSecret(ns, key, "app", "app-creds", "s3cret-value")

		// Drive a few passes so the change is rendered and pushed live.
		for i := 0; i < 5; i++ {
			reconcileOnce(r, key)
		}

		// The live reload fired: ACL LOAD on every reachable node, and CONFIG SET
		// masterauth alongside it (so replicas keep authenticating to their primary).
		gomega.Expect(len(fc.callsOfType("ACLLOAD"))).To(gomega.BeNumerically(">", 0),
			"a real ACL change must issue ACL LOAD live")
		gomega.Expect(len(fc.callsOfTypeWithArg("CONFIGSET", "masterauth"))).To(gomega.BeNumerically(">", 0),
			"a real auth change must re-set masterauth live")

		// ACL LOAD reached BOTH shards' nodes (4 nodes total: 2 shards * (1+1)).
		addrs := map[string]bool{}
		for _, c := range fc.callsOfType("ACLLOAD") {
			addrs[c.addr] = true
		}
		gomega.Expect(len(addrs)).To(gomega.BeNumerically(">=", 4),
			"the reload must reach every reachable node, got %d distinct: %v", len(addrs), addrs)

		// The pods were NOT rolled: an auth-only change leaves serverConfigHash intact
		// (requirepass/masterauth/aclfile are excluded from the roll hash by design).
		gomega.Expect(firstNodeConfigHash(ns)).To(gomega.Equal(hashBefore),
			"an auth-only change must not roll the pods")
	})

	ginkgo.It("is idempotent: a second unchanged reconcile after a reload issues no further ACL LOAD", func() {
		cluster := makeCluster("idem", ns, 2)
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		key := client.ObjectKeyFromObject(cluster)
		reconcileUntilReady(r, key, 40)

		addUserWithSecret(ns, key, "app", "app-creds2", "another-pass")
		for i := 0; i < 4; i++ {
			reconcileOnce(r, key)
		}
		afterChange := len(fc.callsOfType("ACLLOAD"))
		gomega.Expect(afterChange).To(gomega.BeNumerically(">", 0))

		// No further spec change: subsequent passes must not re-reload (signature
		// stamped, so the reconcile is a genuine no-op).
		for i := 0; i < 5; i++ {
			reconcileOnce(r, key)
		}
		gomega.Expect(len(fc.callsOfType("ACLLOAD"))).To(gomega.Equal(afterChange),
			"unchanged reconciles after a reload must not re-issue ACL LOAD")
	})
})

// firstNodeConfigHash returns the serverConfigHash of the first ValkeyNode in the
// namespace (all nodes share the cluster's rendered config hash), or "".
func firstNodeConfigHash(namespace string) string {
	nodes := &valkeyv1alpha1.ValkeyNodeList{}
	if err := k8sClient.List(testCtx, nodes, client.InNamespace(namespace)); err != nil {
		return ""
	}
	if len(nodes.Items) == 0 {
		return ""
	}
	return nodes.Items[0].Spec.ServerConfigHash
}
