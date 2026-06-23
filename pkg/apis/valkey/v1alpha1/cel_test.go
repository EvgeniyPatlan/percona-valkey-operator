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

package v1alpha1_test

import (
	"fmt"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// ns is a per-spec unique namespace counter so create/update objects don't
// collide across Ginkgo specs.
var nsCounter int

func uniqueNS() string {
	nsCounter++
	return fmt.Sprintf("vk-cel-%d", nsCounter)
}

// newCluster builds a minimal valid cluster object in a fresh namespace. The
// caller mutates it before Create.
func newCluster(name string) *v1.PerconaValkeyCluster {
	return &v1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1.PerconaValkeyClusterSpec{
			Mode:         v1.ModeCluster,
			Shards:       3,
			Replicas:     1,
			WorkloadType: v1.WorkloadStatefulSet,
		},
	}
}

func ptr[T any](v T) *T { return &v }

var _ = ginkgo.Describe("Generated CRDs", func() {
	ginkgo.It("install and reach Established for all four kinds", func() {
		names := []string{
			"perconavalkeyclusters.valkey.percona.com",
			"valkeynodes.valkey.percona.com",
			"perconavalkeybackups.valkey.percona.com",
			"perconavalkeyrestores.valkey.percona.com",
		}
		for _, n := range names {
			gomega.Eventually(func(g gomega.Gomega) {
				crd := &apiextv1.CustomResourceDefinition{}
				g.Expect(k8s.Get(ctx, types.NamespacedName{Name: n}, crd)).To(gomega.Succeed())
				established := false
				for _, c := range crd.Status.Conditions {
					if c.Type == apiextv1.Established && c.Status == apiextv1.ConditionTrue {
						established = true
					}
				}
				g.Expect(established).To(gomega.BeTrue(), "CRD %s should be Established", n)
			}, timeout, interval).Should(gomega.Succeed())
		}
	})

	ginkgo.It("expose the locked short names and printer columns", func() {
		type want struct {
			name      string
			shortName string
			columns   []string
		}
		cases := []want{
			{"perconavalkeyclusters.valkey.percona.com", "pvk", []string{"State", "Reason", "Shards", "Ready", "Host", "Age"}},
			{"valkeynodes.valkey.percona.com", "vkn", []string{"Ready", "Role", "Pod", "IP", "Age"}},
			{"perconavalkeybackups.valkey.percona.com", "pvk-backup", []string{"Cluster", "Storage", "State", "Coverage", "Destination", "Completed", "Age"}},
			{"perconavalkeyrestores.valkey.percona.com", "pvk-restore", []string{"Cluster", "Backup", "State", "Completed", "Age"}},
		}
		for _, tc := range cases {
			crd := &apiextv1.CustomResourceDefinition{}
			gomega.Expect(k8s.Get(ctx, types.NamespacedName{Name: tc.name}, crd)).To(gomega.Succeed())
			gomega.Expect(crd.Spec.Names.ShortNames).To(gomega.ContainElement(tc.shortName))
			var got []string
			for _, v := range crd.Spec.Versions {
				for _, col := range v.AdditionalPrinterColumns {
					got = append(got, col.Name)
				}
			}
			for _, c := range tc.columns {
				gomega.Expect(got).To(gomega.ContainElement(c), "CRD %s should print column %s", tc.name, c)
			}
		}
	})
})

var _ = ginkgo.Describe("Cluster defaulting via the API server", func() {
	ginkgo.It("fills marker defaults on a minimal cluster (has()-guards permit no persistence/tls/backup)", func() {
		c := newCluster("defaults-min")
		c.Spec = v1.PerconaValkeyClusterSpec{Mode: v1.ModeCluster, Shards: 3} // omit everything optional
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		gomega.Expect(got.Spec.WorkloadType).To(gomega.Equal(v1.WorkloadStatefulSet))
		gomega.Expect(got.Spec.Replicas).To(gomega.Equal(int32(1)))
		gomega.Expect(got.Spec.PodDisruptionBudget).To(gomega.Equal(v1.PDBManaged))
		gomega.Expect(got.Spec.Pause).To(gomega.BeFalse())
		// exporter.enabled defaulting is verified in the unstructured spec below: a
		// typed bool (no omitempty) always serializes, so the typed zero value sends
		// enabled=false explicitly rather than letting the API server default it.
	})

	ginkgo.It("defaults persistence.reclaimPolicy on a nested cluster", func() {
		c := newCluster("defaults-nested")
		c.Spec.WorkloadType = v1.WorkloadStatefulSet
		c.Spec.Persistence = &v1.PersistenceSpec{Size: resource.MustParse("10Gi")}
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		gomega.Expect(got.Spec.Persistence.ReclaimPolicy).To(gomega.Equal(v1.ReclaimRetain))
	})

	ginkgo.It("defaults absent exporter/user enabled to true but preserves an explicit false", func() {
		// (a) ABSENT enabled -> the API server applies +kubebuilder:default=true. Built
		// via unstructured because a typed bool (no omitempty, by design) cannot omit
		// the field — only a genuinely-absent value triggers server-side defaulting.
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(v1.GroupVersion.WithKind("PerconaValkeyCluster"))
		u.SetName("defaults-enabled-absent")
		u.SetNamespace("default")
		u.Object["spec"] = map[string]interface{}{
			"mode":     string(v1.ModeCluster),
			"shards":   int64(3),
			"exporter": map[string]interface{}{},                             // enabled absent
			"users":    []interface{}{map[string]interface{}{"name": "app"}}, // enabled absent
		}
		gomega.Expect(k8s.Create(ctx, u)).To(gomega.Succeed())
		gotAbsent := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, types.NamespacedName{Name: "defaults-enabled-absent", Namespace: "default"}, gotAbsent)).To(gomega.Succeed())
		gomega.Expect(gotAbsent.Spec.Exporter.Enabled).To(gomega.BeTrue(), "absent exporter.enabled defaults to true")
		gomega.Expect(gotAbsent.Spec.Users[0].Enabled).To(gomega.BeTrue(), "absent users[].enabled defaults to true")

		// (b) EXPLICIT false is preserved (the omitempty fix): a defaulted-true bool set
		// to false must NOT be silently re-defaulted to true on round-trip.
		c := newCluster("defaults-enabled-false")
		c.Spec.Exporter = v1.ExporterSpec{Enabled: false}
		c.Spec.Users = []v1.UserACLSpec{{Name: "app", Enabled: false}}
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		gotFalse := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), gotFalse)).To(gomega.Succeed())
		gomega.Expect(gotFalse.Spec.Exporter.Enabled).To(gomega.BeFalse(), "explicit exporter.enabled=false is preserved")
		gomega.Expect(gotFalse.Spec.Users[0].Enabled).To(gomega.BeFalse(), "explicit users[].enabled=false is preserved")
	})

	ginkgo.It("applies the new gap-analysis field marker defaults", func() {
		c := newCluster("defaults-gap")
		// auth/exporter.tls are pointer sub-structs: their inner marker defaults
		// only materialize once the parent object is present, so set them.
		c.Spec.Auth = &v1.AuthSpec{}
		c.Spec.TLS = &v1.TLSConfig{SecretName: "tls"}
		c.Spec.Exporter = v1.ExporterSpec{Enabled: true, TLS: &v1.ExporterTLSSpec{}}
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		// auth.enabled defaults true (the CRITICAL default-user auth knob is on).
		gomega.Expect(got.Spec.Auth.Enabled).NotTo(gomega.BeNil())
		gomega.Expect(*got.Spec.Auth.Enabled).To(gomega.BeTrue())
		// SA token automount hardened-off by default (per the chart).
		gomega.Expect(got.Spec.AutomountServiceAccountToken).NotTo(gomega.BeNil())
		gomega.Expect(*got.Spec.AutomountServiceAccountToken).To(gomega.BeFalse())
		// TLS mTLS policy defaults to optional.
		gomega.Expect(got.Spec.TLS.AuthClients).To(gomega.Equal(v1.TLSAuthClientsOptional))
		// Exporter wiring defaults: port 9121, 20s scrape, metrics TLS off.
		gomega.Expect(got.Spec.Exporter.Port).NotTo(gomega.BeNil())
		gomega.Expect(*got.Spec.Exporter.Port).To(gomega.Equal(int32(9121)))
		gomega.Expect(got.Spec.Exporter.ScrapeInterval).To(gomega.Equal("20s"))
		gomega.Expect(got.Spec.Exporter.TLS.Enabled).To(gomega.BeFalse())
	})

	ginkgo.It("keeps the M1 minimal cluster applying with all new fields omitted", func() {
		c := newCluster("defaults-minimal-invariant")
		c.Spec = v1.PerconaValkeyClusterSpec{Mode: v1.ModeCluster, Shards: 3}
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		// No expose/networkPolicy/auth/tls block was forced on the minimal CR by
		// the API server (those are nil pointers until the user opts in).
		gomega.Expect(got.Spec.Expose).To(gomega.BeNil())
		gomega.Expect(got.Spec.NetworkPolicy).To(gomega.BeNil())
		gomega.Expect(got.Spec.TLS).To(gomega.BeNil())
	})
})

var _ = ginkgo.Describe("Cluster CEL rules", func() {
	// Rule 1: persistence requires StatefulSet (persistence + Deployment forbidden).
	ginkgo.Describe("persistence requires StatefulSet", func() {
		ginkgo.It("accepts persistence with StatefulSet (positive)", func() {
			c := newCluster("cel1-ok")
			c.Spec.WorkloadType = v1.WorkloadStatefulSet
			c.Spec.Persistence = &v1.PersistenceSpec{Size: resource.MustParse("5Gi")}
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
		ginkgo.It("rejects persistence with Deployment (negative)", func() {
			c := newCluster("cel1-bad")
			c.Spec.WorkloadType = v1.WorkloadDeployment
			c.Spec.Persistence = &v1.PersistenceSpec{Size: resource.MustParse("5Gi")}
			err := k8s.Create(ctx, c)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("persistence requires workloadType StatefulSet"))
		})
	})

	// Rule 2: persistence cannot be removed once set.
	ginkgo.It("rejects removing persistence once set (negative) + permits keeping it (positive)", func() {
		c := newCluster("cel2")
		c.Spec.WorkloadType = v1.WorkloadStatefulSet
		c.Spec.Persistence = &v1.PersistenceSpec{Size: resource.MustParse("5Gi")}
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		// positive: an unrelated update that keeps persistence is fine.
		got.Spec.Replicas = 2
		gomega.Expect(k8s.Update(ctx, got)).To(gomega.Succeed())
		// negative: removing persistence is rejected.
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.Persistence = nil
		err := k8s.Update(ctx, got)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("persistence cannot be removed once set"))
	})

	// Rule 3: persistence cannot be added after creation.
	ginkgo.It("rejects adding persistence after creation (negative) + permits staying without it (positive)", func() {
		c := newCluster("cel3")
		c.Spec.WorkloadType = v1.WorkloadDeployment // no persistence at create
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		// positive: update without adding persistence.
		got.Spec.Replicas = 2
		gomega.Expect(k8s.Update(ctx, got)).To(gomega.Succeed())
		// negative: adding persistence after creation is rejected.
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.WorkloadType = v1.WorkloadDeployment
		got.Spec.Persistence = &v1.PersistenceSpec{Size: resource.MustParse("5Gi")}
		err := k8s.Update(ctx, got)
		gomega.Expect(err).To(gomega.HaveOccurred())
		// Either "added" or "requires StatefulSet" may fire; assert the add rule.
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("persistence cannot be added after creation"))
	})

	// Rule 4: persistence.size expand-only.
	ginkgo.It("permits expanding persistence.size (positive) and rejects shrinking it (negative)", func() {
		c := newCluster("cel4")
		c.Spec.WorkloadType = v1.WorkloadStatefulSet
		c.Spec.Persistence = &v1.PersistenceSpec{Size: resource.MustParse("10Gi")}
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.Persistence.Size = resource.MustParse("20Gi")
		gomega.Expect(k8s.Update(ctx, got)).To(gomega.Succeed()) // positive: grow

		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.Persistence.Size = resource.MustParse("5Gi")
		err := k8s.Update(ctx, got) // negative: shrink
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("persistence.size may only be expanded"))
	})

	// Rule 5: storageClassName immutable.
	ginkgo.It("permits keeping storageClassName (positive) and rejects changing it (negative)", func() {
		c := newCluster("cel5")
		c.Spec.WorkloadType = v1.WorkloadStatefulSet
		c.Spec.Persistence = &v1.PersistenceSpec{Size: resource.MustParse("10Gi"), StorageClassName: ptr("fast")}
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.Replicas = 2 // positive: unrelated change, SC kept
		gomega.Expect(k8s.Update(ctx, got)).To(gomega.Succeed())

		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.Persistence.StorageClassName = ptr("slow")
		err := k8s.Update(ctx, got) // negative: change SC
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("persistence.storageClassName is immutable"))
	})

	// Rule 6: mode immutable.
	ginkgo.It("permits keeping mode (positive) and rejects changing it (negative)", func() {
		c := newCluster("cel6")
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.Replicas = 2 // positive
		gomega.Expect(k8s.Update(ctx, got)).To(gomega.Succeed())

		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.Mode = v1.ModeReplication
		got.Spec.Shards = 1 // keep rule 7 satisfied so we isolate the mode rule
		err := k8s.Update(ctx, got)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("mode is immutable"))
	})

	// Rule 7: non-cluster modes require shards==1.
	ginkgo.Describe("non-cluster modes are single-shard", func() {
		ginkgo.It("accepts replication mode with shards=1 (positive)", func() {
			c := newCluster("cel7-ok")
			c.Spec.Mode = v1.ModeReplication
			c.Spec.Shards = 1
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
		ginkgo.It("rejects replication mode with shards=3 (negative)", func() {
			c := newCluster("cel7-bad")
			c.Spec.Mode = v1.ModeReplication
			c.Spec.Shards = 3
			err := k8s.Create(ctx, c)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("shards must be 1 unless mode is cluster"))
		})
		ginkgo.It("accepts replication mode with shards omitted thanks to has() guard (positive)", func() {
			c := newCluster("cel7-omit")
			c.Spec.Mode = v1.ModeReplication
			c.Spec.Shards = 0 // omitted -> has(self.shards) is false -> rule short-circuits true
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
	})

	// workloadType field-level immutability.
	ginkgo.It("permits keeping workloadType (positive) and rejects changing it (negative)", func() {
		c := newCluster("wt")
		c.Spec.WorkloadType = v1.WorkloadStatefulSet
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())

		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.Replicas = 2 // positive
		gomega.Expect(k8s.Update(ctx, got)).To(gomega.Succeed())

		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Spec.WorkloadType = v1.WorkloadDeployment
		err := k8s.Update(ctx, got)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("workloadType is immutable"))
	})

	// users[].name system-user reservation.
	ginkgo.Describe("users[].name reservation", func() {
		ginkgo.It("accepts a normal username (positive)", func() {
			c := newCluster("user-ok")
			c.Spec.Users = []v1.UserACLSpec{{Name: "app"}}
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
		ginkgo.It("rejects a username starting with _ (negative)", func() {
			c := newCluster("user-bad")
			c.Spec.Users = []v1.UserACLSpec{{Name: "_operator"}}
			err := k8s.Create(ctx, c)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("reserved for system users"))
		})
	})

	// commands item pattern.
	ginkgo.Describe("users[].commands item pattern", func() {
		ginkgo.It("accepts valid command tokens incl. categories and container|subcommand (positive)", func() {
			c := newCluster("cmd-ok")
			c.Spec.Users = []v1.UserACLSpec{{
				Name:     "app",
				Commands: &v1.UserCommands{Allow: []string{"@read", "get", "config|get"}},
			}}
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
		ginkgo.It("rejects an invalid command token (negative)", func() {
			c := newCluster("cmd-bad")
			c.Spec.Users = []v1.UserACLSpec{{
				Name:     "app",
				Commands: &v1.UserCommands{Allow: []string{"BAD TOKEN!"}},
			}}
			err := k8s.Create(ctx, c)
			gomega.Expect(err).To(gomega.HaveOccurred())
		})
	})

	// TLS mutual exclusion.
	ginkgo.Describe("tls.secretName XOR tls.certManager", func() {
		ginkgo.It("accepts secretName alone (positive)", func() {
			c := newCluster("tls-secret")
			c.Spec.TLS = &v1.TLSConfig{SecretName: "my-tls"}
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
		ginkgo.It("accepts certManager alone (positive)", func() {
			c := newCluster("tls-cm")
			c.Spec.TLS = &v1.TLSConfig{CertManager: &v1.CertManagerSpec{IssuerRef: v1.IssuerRef{Name: "ca"}}}
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
		ginkgo.It("rejects both secretName and certManager (negative)", func() {
			c := newCluster("tls-both")
			c.Spec.TLS = &v1.TLSConfig{
				SecretName:  "my-tls",
				CertManager: &v1.CertManagerSpec{IssuerRef: v1.IssuerRef{Name: "ca"}},
			}
			err := k8s.Create(ctx, c)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("set at most one of tls.secretName or tls.certManager"))
		})
	})

	// tls.authClients enum (off|optional|require).
	ginkgo.Describe("tls.authClients enum", func() {
		ginkgo.It("accepts require (positive)", func() {
			c := newCluster("authclients-ok")
			c.Spec.TLS = &v1.TLSConfig{SecretName: "tls", AuthClients: v1.TLSAuthClientsRequire}
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
		ginkgo.It("rejects an unknown value (negative)", func() {
			c := newCluster("authclients-bad")
			c.Spec.TLS = &v1.TLSConfig{SecretName: "tls", AuthClients: v1.TLSAuthClients("bogus")}
			err := k8s.Create(ctx, c)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("authClients"))
		})
	})

	// expose.type enum (ClusterIP|NodePort|LoadBalancer).
	ginkgo.Describe("expose.type enum", func() {
		ginkgo.It("accepts LoadBalancer (positive)", func() {
			c := newCluster("expose-ok")
			c.Spec.Expose = &v1.ExposeSpec{Type: "LoadBalancer"}
			gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		})
		ginkgo.It("rejects an unknown service type (negative)", func() {
			c := newCluster("expose-bad")
			c.Spec.Expose = &v1.ExposeSpec{Type: "Bogus"}
			err := k8s.Create(ctx, c)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("type"))
		})
	})

	// expose has()-guard: an absent expose block must not be over-required (the
	// minimal cluster has no expose and still applies).
	ginkgo.It("permits omitting the expose block entirely (positive)", func() {
		c := newCluster("expose-omitted")
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
	})

	ginkgo.It("status subresource is independent of spec (positive)", func() {
		c := newCluster("status-sub")
		gomega.Expect(k8s.Create(ctx, c)).To(gomega.Succeed())
		got := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), got)).To(gomega.Succeed())
		got.Status.State = v1.StateReady
		got.Status.Shards = 3
		gomega.Expect(k8s.Status().Update(ctx, got)).To(gomega.Succeed())

		fresh := &v1.PerconaValkeyCluster{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(c), fresh)).To(gomega.Succeed())
		gomega.Expect(fresh.Status.State).To(gomega.Equal(v1.StateReady))
		gomega.Expect(fresh.Status.Shards).To(gomega.Equal(int32(3)))
	})
})

var _ = ginkgo.Describe("Restore CEL (backupName xor backupSource)", func() {
	newRestore := func(name string) *v1.PerconaValkeyRestore {
		return &v1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       v1.PerconaValkeyRestoreSpec{ClusterName: "target"},
		}
	}
	ginkgo.It("accepts backupName alone (positive)", func() {
		r := newRestore("restore-name")
		r.Spec.BackupName = "b1"
		gomega.Expect(k8s.Create(ctx, r)).To(gomega.Succeed())
	})
	ginkgo.It("accepts backupSource alone (positive)", func() {
		r := newRestore("restore-source")
		r.Spec.BackupSource = &v1.BackupSource{Destination: "s3://b/p"}
		gomega.Expect(k8s.Create(ctx, r)).To(gomega.Succeed())
	})
	ginkgo.It("rejects both set (negative)", func() {
		r := newRestore("restore-both")
		r.Spec.BackupName = "b1"
		r.Spec.BackupSource = &v1.BackupSource{Destination: "s3://b/p"}
		err := k8s.Create(ctx, r)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("set exactly one of backupName or backupSource"))
	})
	ginkgo.It("rejects neither set (negative)", func() {
		r := newRestore("restore-neither")
		err := k8s.Create(ctx, r)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("set exactly one of backupName or backupSource"))
	})
})

var _ = ginkgo.Describe("Backup CR", func() {
	ginkgo.It("applies the §11.3 sample and fills marker defaults", func() {
		b := &v1.PerconaValkeyBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "ondemand", Namespace: "default"},
			Spec:       v1.PerconaValkeyBackupSpec{ClusterName: "prod", StorageName: "s3-primary"},
		}
		gomega.Expect(k8s.Create(ctx, b)).To(gomega.Succeed())
		got := &v1.PerconaValkeyBackup{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(b), got)).To(gomega.Succeed())
		gomega.Expect(got.Spec.Type).To(gomega.Equal(v1.BackupTypeFull))
		gomega.Expect(got.Spec.Consistency).To(gomega.Equal(v1.ConsistencyStrict))
		gomega.Expect(got.Spec.ActiveDeadlineSeconds).NotTo(gomega.BeNil())
		gomega.Expect(*got.Spec.ActiveDeadlineSeconds).To(gomega.Equal(int64(3600)))
	})
})

var _ = ginkgo.Describe("ValkeyNode CRD persistence CEL mirror", func() {
	ginkgo.It("rejects shrinking node persistence.size (negative)", func() {
		n := &v1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-0-0", Namespace: "default"},
			Spec: v1.ValkeyNodeSpec{
				WorkloadType: v1.WorkloadStatefulSet,
				Persistence:  &v1.PersistenceSpec{Size: resource.MustParse("10Gi")},
			},
		}
		gomega.Expect(k8s.Create(ctx, n)).To(gomega.Succeed())
		got := &v1.ValkeyNode{}
		gomega.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(n), got)).To(gomega.Succeed())
		got.Spec.Persistence.Size = resource.MustParse("5Gi")
		err := k8s.Update(ctx, got)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("persistence.size may only be expanded"))
	})
})

// keep uniqueNS referenced (reserved for future per-spec namespace isolation).
var _ = uniqueNS
