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

package perconavalkeyrestore

import (
	"fmt"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

var _ = ginkgo.Describe("PerconaValkeyRestore controller phase machine", func() {
	var (
		ns      string
		nsIndex int
	)

	ginkgo.BeforeEach(func() {
		nsIndex++
		ns = makeNamespace(fmt.Sprintf("pvk-rst-%d-%d", time.Now().UnixNano()%100000, nsIndex))
	})

	const srcCluster = "src"

	// seedCompleteManifest writes a complete 3-shard manifest into a FakeStore at the
	// canonical manifest key for the source cluster/backup "bk".
	seedCompleteManifest := func(store *backup.FakeStore, bk string) {
		man := backup.Manifest{
			Cluster:       srcCluster,
			BackupName:    bk,
			Mode:          "cluster",
			EngineVersion: "9.0.0",
			Consistency:   "strict",
			SlotCoverage:  coverageComplete,
			Shards: []backup.ShardManifest{
				{Index: 0, SlotRanges: "0-5460", RDBKey: backup.ShardRDBRelKey(0), SHA256: "a"},
				{Index: 1, SlotRanges: "5461-10922", RDBKey: backup.ShardRDBRelKey(1), SHA256: "b"},
				{Index: 2, SlotRanges: "10923-16383", RDBKey: backup.ShardRDBRelKey(2), SHA256: "c"},
			},
		}
		gomega.Expect(backup.WriteManifest(testCtx, store, backup.ManifestKey(srcCluster, bk), man)).To(gomega.Succeed())
	}

	// newBackupCR creates a Succeeded PerconaValkeyBackup whose status.destination
	// points the restore at the FakeStore-seeded set under "src"/"bk".
	newBackupCR := func(name, srcCluster string) *valkeyv1alpha1.PerconaValkeyBackup {
		bk := &valkeyv1alpha1.PerconaValkeyBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyBackupSpec{
				ClusterName: srcCluster,
				StorageName: "s3-primary",
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, bk)).To(gomega.Succeed())
		bk.Status.State = valkeyv1alpha1.BackupStateSucceeded
		bk.Status.StorageName = "s3-primary"
		bk.Status.Destination = backup.FormatDestination(backup.SchemeS3, "my-bucket", "prefix", srcCluster, name)
		bk.Status.S3 = &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "my-bucket", Prefix: "prefix"}
		gomega.Expect(k8sClient.Status().Update(testCtx, bk)).To(gomega.Succeed())
		return bk
	}

	newRestore := func(name, targetCluster, backupName string) *valkeyv1alpha1.PerconaValkeyRestore {
		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName: targetCluster,
				BackupName:  backupName,
				Strategy:    valkeyv1alpha1.RestoreStrategyNewCluster,
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())
		return rst
	}

	reconcileOnce := func(r *Reconciler, rst *valkeyv1alpha1.PerconaValkeyRestore) (ctrl.Result, error) {
		return r.Reconcile(testCtx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: rst.Name, Namespace: rst.Namespace},
		})
	}

	getRestore := func(name string) *valkeyv1alpha1.PerconaValkeyRestore {
		out := &valkeyv1alpha1.PerconaValkeyRestore{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, out)).To(gomega.Succeed())
		return out
	}

	ginkgo.It("fails cleanly when the manifest is missing", func() {
		store := backup.NewFakeStore() // intentionally empty: no manifest written.
		newBackupCR("bk-missing", "src")
		rst := newRestore("rst-missing", "tgt-missing", "bk-missing")

		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))
		_, err := reconcileOnce(r, rst)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		out := getRestore("rst-missing")
		gomega.Expect(CurrentOf(out)).To(gomega.Equal(phaseFailed))
		gomega.Expect(out.Status.State).To(gomega.Equal(valkeyv1alpha1.RestoreStateFailed))
		gomega.Expect(out.Status.StateDescription).To(gomega.ContainSubstring("incomplete or was deleted"))
	})

	ginkgo.It("fails cleanly when neither backupName nor backupSource is set is impossible via CEL; backupSource with bad destination fails", func() {
		store := backup.NewFakeStore()
		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst-badsrc", Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName:  "tgt-badsrc",
				BackupSource: &valkeyv1alpha1.BackupSource{Destination: "not-a-scheme/x"},
				Strategy:     valkeyv1alpha1.RestoreStrategyNewCluster,
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())

		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))
		_, err := reconcileOnce(r, rst)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(CurrentOf(getRestore("rst-badsrc"))).To(gomega.Equal(phaseFailed))
	})

	ginkgo.It("walks Pending->Provisioning->Seeding->Forming->Validating->Succeeded against a FakeStore-seeded manifest and a manually-driven target cluster", func() {
		store := backup.NewFakeStore()
		seedCompleteManifest(store, "bk-happy")
		newBackupCR("bk-happy", "src")
		rst := newRestore("rst-happy", "tgt-happy", "bk-happy")
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))

		// 1) Pending -> Provisioning (source resolved + coverage validated).
		_, err := reconcileOnce(r, rst)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		out := getRestore("rst-happy")
		gomega.Expect(CurrentOf(out)).To(gomega.Equal(phaseProvisioning))
		gomega.Expect(out.Annotations[annTargetCluster]).To(gomega.Equal("tgt-happy"))
		gomega.Expect(out.Annotations[annRestoredSlots]).To(gomega.ContainSubstring("16384"))

		// 2) Provisioning -> Seeding (target cluster created with the seed markers).
		_, err = reconcileOnce(r, out)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		out = getRestore("rst-happy")
		gomega.Expect(CurrentOf(out)).To(gomega.Equal(phaseSeeding))

		cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-happy", Namespace: ns}, cluster)).To(gomega.Succeed())
		gomega.Expect(cluster.Spec.Shards).To(gomega.Equal(int32(3)))
		// CR-8 / R3: the seed boot MUST override appendonly to no so dump.rdb loads.
		gomega.Expect(cluster.Annotations[annSeedAppendonly]).To(gomega.Equal(seedAppendonlyNo))
		gomega.Expect(cluster.Annotations).To(gomega.HaveKey(annRestoreMarker))

		// 3) Seeding -> Forming (markers in place).
		_, err = reconcileOnce(r, out)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(CurrentOf(getRestore("rst-happy"))).To(gomega.Equal(phaseForming))

		// While the cluster has NOT formed, Forming must hold (requeue, no advance).
		res, err := reconcileOnce(r, getRestore("rst-happy"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.RequeueAfter).To(gomega.BeNumerically(">", 0))
		gomega.Expect(CurrentOf(getRestore("rst-happy"))).To(gomega.Equal(phaseForming))

		// Drive the target cluster to ClusterFormed (the cluster controller re-forms
		// topology in production; here we script its status).
		setClusterCondition(cluster, condClusterFormed)
		gomega.Expect(k8sClient.Status().Update(testCtx, cluster)).To(gomega.Succeed())

		// 4) Forming -> Validating (cluster re-formed).
		_, err = reconcileOnce(r, getRestore("rst-happy"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(CurrentOf(getRestore("rst-happy"))).To(gomega.Equal(phaseValidating))

		// Validating holds until 16384 slots assigned AND cluster Ready.
		res, err = reconcileOnce(r, getRestore("rst-happy"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.RequeueAfter).To(gomega.BeNumerically(">", 0))
		gomega.Expect(CurrentOf(getRestore("rst-happy"))).To(gomega.Equal(phaseValidating))

		// Drive SlotsAssigned + Ready: full slot coverage proven.
		gomega.Expect(k8sClient.Get(testCtx, client.ObjectKeyFromObject(cluster), cluster)).To(gomega.Succeed())
		setClusterCondition(cluster, condSlotsAssigned)
		setClusterCondition(cluster, condClusterReady)
		cluster.Status.State = valkeyv1alpha1.StateReady
		gomega.Expect(k8sClient.Status().Update(testCtx, cluster)).To(gomega.Succeed())

		// 5) Validating -> Succeeded.
		_, err = reconcileOnce(r, getRestore("rst-happy"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		out = getRestore("rst-happy")
		gomega.Expect(CurrentOf(out)).To(gomega.Equal(phaseSucceeded))
		gomega.Expect(out.Status.State).To(gomega.Equal(valkeyv1alpha1.RestoreStateSucceeded))
		gomega.Expect(out.Status.Completed).NotTo(gomega.BeNil())

		// Terminal: a further reconcile is a no-op (sticky).
		_, err = reconcileOnce(r, out)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(CurrentOf(getRestore("rst-happy"))).To(gomega.Equal(phaseSucceeded))
	})

	ginkgo.It("gates Succeeded on slot coverage: a target cluster that never reaches SlotsAssigned stays in Validating", func() {
		store := backup.NewFakeStore()
		seedCompleteManifest(store, "bk-gate")
		newBackupCR("bk-gate", "src")
		_ = newRestore("rst-gate", "tgt-gate", "bk-gate")
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))

		// Drive through to Validating.
		gomega.Eventually(func() restorePhase {
			_, _ = reconcileOnce(r, getRestore("rst-gate"))
			cur := getRestore("rst-gate")
			if CurrentOf(cur) == phaseForming {
				cl := &valkeyv1alpha1.PerconaValkeyCluster{}
				if k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-gate", Namespace: ns}, cl) == nil {
					setClusterCondition(cl, condClusterFormed)
					_ = k8sClient.Status().Update(testCtx, cl)
				}
			}
			return CurrentOf(getRestore("rst-gate"))
		}, timeout, interval).Should(gomega.Equal(phaseValidating))

		// Cluster reports formed but only PARTIAL slots (SlotsAssigned never True):
		// the restore must NOT advance to Succeeded.
		cl := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-gate", Namespace: ns}, cl)).To(gomega.Succeed())
		gomega.Consistently(func() restorePhase {
			_, _ = reconcileOnce(r, getRestore("rst-gate"))
			return CurrentOf(getRestore("rst-gate"))
		}, 2*time.Second, interval).Should(gomega.Equal(phaseValidating))
	})

	ginkgo.It("fails Validating loudly when the target cluster reports Failed (cluster left for inspection)", func() {
		store := backup.NewFakeStore()
		seedCompleteManifest(store, "bk-fail")
		newBackupCR("bk-fail", "src")
		_ = newRestore("rst-fail", "tgt-fail", "bk-fail")
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))

		// Advance to Forming, then mark the cluster Formed and then Failed.
		gomega.Eventually(func() restorePhase {
			_, _ = reconcileOnce(r, getRestore("rst-fail"))
			return CurrentOf(getRestore("rst-fail"))
		}, timeout, interval).Should(gomega.Equal(phaseForming))

		cl := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-fail", Namespace: ns}, cl)).To(gomega.Succeed())
		setClusterCondition(cl, condClusterFormed)
		gomega.Expect(k8sClient.Status().Update(testCtx, cl)).To(gomega.Succeed())

		_, _ = reconcileOnce(r, getRestore("rst-fail")) // Forming -> Validating
		gomega.Expect(CurrentOf(getRestore("rst-fail"))).To(gomega.Equal(phaseValidating))

		gomega.Expect(k8sClient.Get(testCtx, client.ObjectKeyFromObject(cl), cl)).To(gomega.Succeed())
		cl.Status.State = valkeyv1alpha1.StateFailed
		cl.Status.Reason = "QuorumLost"
		gomega.Expect(k8sClient.Status().Update(testCtx, cl)).To(gomega.Succeed())

		_, err := reconcileOnce(r, getRestore("rst-fail"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		out := getRestore("rst-fail")
		gomega.Expect(CurrentOf(out)).To(gomega.Equal(phaseFailed))
		gomega.Expect(out.Status.StateDescription).To(gomega.ContainSubstring("left for inspection"))

		// The partially-built target cluster is NOT auto-deleted.
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-fail", Namespace: ns}, &valkeyv1alpha1.PerconaValkeyCluster{})).To(gomega.Succeed())
	})

	ginkgo.It("rejects a partial-coverage source at validation, before provisioning any cluster", func() {
		store := backup.NewFakeStore()
		// Write a manifest missing the last shard => gap.
		man := backup.Manifest{
			Cluster: "src", BackupName: "bk-partial", SlotCoverage: coveragePartial,
			Shards: []backup.ShardManifest{
				{Index: 0, SlotRanges: "0-5460"},
				{Index: 1, SlotRanges: "5461-10922"},
			},
		}
		gomega.Expect(backup.WriteManifest(testCtx, store, backup.ManifestKey("src", "bk-partial"), man)).To(gomega.Succeed())
		newBackupCR("bk-partial", "src")
		rst := newRestore("rst-partial", "tgt-partial", "bk-partial")
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))

		_, err := reconcileOnce(r, rst)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		out := getRestore("rst-partial")
		gomega.Expect(CurrentOf(out)).To(gomega.Equal(phaseFailed))
		gomega.Expect(out.Status.StateDescription).To(gomega.ContainSubstring("partial slot coverage"))
		// No cluster was provisioned.
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-partial", Namespace: ns}, &valkeyv1alpha1.PerconaValkeyCluster{})).NotTo(gomega.Succeed())
	})

	ginkgo.It("allows a partial-coverage source when the allow-partial-restore annotation is set", func() {
		store := backup.NewFakeStore()
		man := backup.Manifest{
			Cluster: "src", BackupName: "bk-ovr", SlotCoverage: coveragePartial,
			Shards: []backup.ShardManifest{
				{Index: 0, SlotRanges: "0-5460"},
				{Index: 1, SlotRanges: "5461-10922"},
			},
		}
		gomega.Expect(backup.WriteManifest(testCtx, store, backup.ManifestKey("src", "bk-ovr"), man)).To(gomega.Succeed())
		newBackupCR("bk-ovr", "src")
		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{
				Name: "rst-ovr", Namespace: ns,
				Annotations: map[string]string{annAllowPartial: "true"},
			},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName: "tgt-ovr", BackupName: "bk-ovr",
				Strategy: valkeyv1alpha1.RestoreStrategyNewCluster,
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))

		_, err := reconcileOnce(r, rst)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		out := getRestore("rst-ovr")
		gomega.Expect(CurrentOf(out)).To(gomega.Equal(phaseProvisioning), "partial source with override should pass validation")
	})

	ginkgo.It("resolves from an inline backupSource (Backup CR gone) and provisions", func() {
		store := backup.NewFakeStore()
		seedCompleteManifest(store, "bk-inline")
		dest := backup.FormatDestination(backup.SchemeS3, "my-bucket", "prefix", "src", "bk-inline")
		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst-inline", Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName: "tgt-inline",
				BackupSource: &valkeyv1alpha1.BackupSource{
					Destination: dest,
					StorageName: "s3-primary",
					S3:          &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "my-bucket", Prefix: "prefix"},
				},
				Strategy: valkeyv1alpha1.RestoreStrategyNewCluster,
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))

		_, err := reconcileOnce(r, rst)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(CurrentOf(getRestore("rst-inline"))).To(gomega.Equal(phaseProvisioning))

		_, err = reconcileOnce(r, getRestore("rst-inline"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(CurrentOf(getRestore("rst-inline"))).To(gomega.Equal(phaseSeeding))
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-inline", Namespace: ns}, &valkeyv1alpha1.PerconaValkeyCluster{})).To(gomega.Succeed())
	})
})

var _ = ginkgo.Describe("PerconaValkeyRestore adopt / InPlace / marker paths", func() {
	var ns string

	ginkgo.BeforeEach(func() {
		ns = makeNamespace(fmt.Sprintf("pvk-rst-adopt-%d", time.Now().UnixNano()%1000000))
	})

	seedManifest := func(store *backup.FakeStore, cluster, bk string) {
		man := backup.Manifest{
			Cluster: cluster, BackupName: bk, SlotCoverage: coverageComplete,
			Shards: []backup.ShardManifest{
				{Index: 0, SlotRanges: "0-8191"},
				{Index: 1, SlotRanges: "8192-16383"},
			},
		}
		gomega.Expect(backup.WriteManifest(testCtx, store, backup.ManifestKey(cluster, bk), man)).To(gomega.Succeed())
	}

	ginkgo.It("adopts a pre-existing target cluster and stamps the seed markers (InPlace)", func() {
		store := backup.NewFakeStore()
		seedManifest(store, "src", "bk-inplace")

		// A pre-existing target cluster (InPlace target must already exist).
		existing := &valkeyv1alpha1.PerconaValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "tgt-inplace", Namespace: ns},
			Spec:       valkeyv1alpha1.PerconaValkeyClusterSpec{Shards: 2},
		}
		gomega.Expect(k8sClient.Create(testCtx, existing)).To(gomega.Succeed())

		bk := &valkeyv1alpha1.PerconaValkeyBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-inplace", Namespace: ns},
			Spec:       valkeyv1alpha1.PerconaValkeyBackupSpec{ClusterName: "src", StorageName: "s3-primary"},
		}
		gomega.Expect(k8sClient.Create(testCtx, bk)).To(gomega.Succeed())
		bk.Status.Destination = backup.FormatDestination(backup.SchemeS3, "b", "p", "src", "bk-inplace")
		bk.Status.S3 = &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "b", Prefix: "p"}
		gomega.Expect(k8sClient.Status().Update(testCtx, bk)).To(gomega.Succeed())

		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst-inplace", Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName: "tgt-inplace", BackupName: "bk-inplace",
				Strategy: valkeyv1alpha1.RestoreStrategyInPlace,
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "rst-inplace", Namespace: ns}}
		_, err := r.Reconcile(testCtx, req) // Pending -> Provisioning
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = r.Reconcile(testCtx, req) // Provisioning -> adopt existing -> Seeding
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// The pre-existing cluster was adopted (not re-created) and stamped with the
		// appendonly-no seed marker.
		adopted := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-inplace", Namespace: ns}, adopted)).To(gomega.Succeed())
		gomega.Expect(adopted.Spec.Shards).To(gomega.Equal(int32(2)), "InPlace must not resize the existing cluster")
		gomega.Expect(adopted.Annotations[annSeedAppendonly]).To(gomega.Equal(seedAppendonlyNo))
		gomega.Expect(adopted.Annotations).To(gomega.HaveKey(annRestoreMarker))
	})

	ginkgo.It("fails an InPlace restore whose target cluster does not exist", func() {
		store := backup.NewFakeStore()
		seedManifest(store, "src", "bk-noexist")
		bk := &valkeyv1alpha1.PerconaValkeyBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-noexist", Namespace: ns},
			Spec:       valkeyv1alpha1.PerconaValkeyBackupSpec{ClusterName: "src", StorageName: "s3-primary"},
		}
		gomega.Expect(k8sClient.Create(testCtx, bk)).To(gomega.Succeed())
		bk.Status.Destination = backup.FormatDestination(backup.SchemeS3, "b", "p", "src", "bk-noexist")
		bk.Status.S3 = &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "b", Prefix: "p"}
		gomega.Expect(k8sClient.Status().Update(testCtx, bk)).To(gomega.Succeed())

		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst-noexist", Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName: "tgt-noexist", BackupName: "bk-noexist",
				Strategy: valkeyv1alpha1.RestoreStrategyInPlace,
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "rst-noexist", Namespace: ns}}
		_, err := r.Reconcile(testCtx, req) // -> Provisioning
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = r.Reconcile(testCtx, req) // Provisioning fails: no existing cluster
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		out := &valkeyv1alpha1.PerconaValkeyRestore{}
		gomega.Expect(k8sClient.Get(testCtx, req.NamespacedName, out)).To(gomega.Succeed())
		gomega.Expect(currentPhase(out)).To(gomega.Equal(phaseFailed))
	})

	ginkgo.It("re-stamps the seed override during Seeding if the markers were stripped", func() {
		store := backup.NewFakeStore()
		seedManifest(store, "src", "bk-restamp")
		bk := &valkeyv1alpha1.PerconaValkeyBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-restamp", Namespace: ns},
			Spec:       valkeyv1alpha1.PerconaValkeyBackupSpec{ClusterName: "src", StorageName: "s3-primary"},
		}
		gomega.Expect(k8sClient.Create(testCtx, bk)).To(gomega.Succeed())
		bk.Status.Destination = backup.FormatDestination(backup.SchemeS3, "b", "p", "src", "bk-restamp")
		bk.Status.S3 = &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "b", Prefix: "p"}
		gomega.Expect(k8sClient.Status().Update(testCtx, bk)).To(gomega.Succeed())

		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst-restamp", Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName: "tgt-restamp", BackupName: "bk-restamp",
				Strategy: valkeyv1alpha1.RestoreStrategyNewCluster,
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())
		r := newReconcilerForTest(k8sClient, apiScheme, fixedStoreFactory(store))
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "rst-restamp", Namespace: ns}}
		_, _ = r.Reconcile(testCtx, req) // Pending->Provisioning
		_, _ = r.Reconcile(testCtx, req) // Provisioning->Seeding (creates cluster w/ markers)

		// Strip the seed markers off the cluster to simulate them being lost.
		cl := &valkeyv1alpha1.PerconaValkeyCluster{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-restamp", Namespace: ns}, cl)).To(gomega.Succeed())
		base := cl.DeepCopy()
		cl.Annotations = map[string]string{}
		gomega.Expect(k8sClient.Patch(testCtx, cl, client.MergeFrom(base))).To(gomega.Succeed())

		// Seeding reconcile must re-stamp the appendonly-no override and hold.
		_, err := r.Reconcile(testCtx, req)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "tgt-restamp", Namespace: ns}, cl)).To(gomega.Succeed())
		gomega.Expect(cl.Annotations[annSeedAppendonly]).To(gomega.Equal(seedAppendonlyNo))

		out := &valkeyv1alpha1.PerconaValkeyRestore{}
		gomega.Expect(k8sClient.Get(testCtx, req.NamespacedName, out)).To(gomega.Succeed())
		gomega.Expect(currentPhase(out)).To(gomega.Equal(phaseSeeding), "still seeding until markers settle")
	})
})

var _ = ginkgo.Describe("PerconaValkeyRestore SetupWithManager wiring", func() {
	ginkgo.It("the shared manager reconciles a restore created in its namespace (For wiring)", func() {
		// The shared manager (suite_test) injects an EMPTY FakeStore, so a restore
		// it reconciles fails cleanly at ReadManifest — which proves the manager's
		// For(PerconaValkeyRestore) wiring drives Reconcile end-to-end.
		bk := &valkeyv1alpha1.PerconaValkeyBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-mgr", Namespace: mgrNamespace},
			Spec:       valkeyv1alpha1.PerconaValkeyBackupSpec{ClusterName: "src", StorageName: "s3-primary"},
		}
		gomega.Expect(k8sClient.Create(testCtx, bk)).To(gomega.Succeed())
		bk.Status.Destination = backup.FormatDestination(backup.SchemeS3, "b", "p", "src", "bk-mgr")
		bk.Status.S3 = &valkeyv1alpha1.BackupStorageS3Spec{Bucket: "b", Prefix: "p"}
		gomega.Expect(k8sClient.Status().Update(testCtx, bk)).To(gomega.Succeed())

		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst-mgr", Namespace: mgrNamespace},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName: "tgt-mgr", BackupName: "bk-mgr",
				Strategy: valkeyv1alpha1.RestoreStrategyNewCluster,
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())

		gomega.Eventually(func() valkeyv1alpha1.RestoreState {
			out := &valkeyv1alpha1.PerconaValkeyRestore{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: "rst-mgr", Namespace: mgrNamespace}, out); err != nil {
				return ""
			}
			return out.Status.State
		}, timeout, interval).Should(gomega.Equal(valkeyv1alpha1.RestoreStateFailed))
	})

	ginkgo.It("wires For/Owns/Watches and maps a cluster to its restores", func() {
		r := &Reconciler{storeFactory: fixedStoreFactory(backup.NewFakeStore()), skipNameValidation: true}
		// The manager-backed wiring is exercised by the shared envtest manager in
		// the cluster suite; here we directly verify the mapping fan-out, which is
		// the data-dependent part of SetupWithManager.
		r.Client = k8sClient
		r.scheme = apiScheme

		ns := makeNamespace(fmt.Sprintf("pvk-rst-map-%d", time.Now().UnixNano()%1000000))
		rst := &valkeyv1alpha1.PerconaValkeyRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst-map", Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyRestoreSpec{
				ClusterName:  "tgt-map",
				BackupSource: &valkeyv1alpha1.BackupSource{Destination: "s3://b/src/bk"},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, rst)).To(gomega.Succeed())

		cluster := &valkeyv1alpha1.PerconaValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "tgt-map", Namespace: ns},
		}
		reqs := r.mapClusterToRestores(testCtx, cluster)
		gomega.Expect(reqs).To(gomega.HaveLen(1))
		gomega.Expect(reqs[0].Name).To(gomega.Equal("rst-map"))

		// A non-cluster object maps to nothing.
		gomega.Expect(r.mapClusterToRestores(testCtx, rst)).To(gomega.BeNil())
		// A cluster with no matching restore maps to nothing.
		other := &valkeyv1alpha1.PerconaValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "no-match", Namespace: ns}}
		gomega.Expect(r.mapClusterToRestores(testCtx, other)).To(gomega.BeEmpty())
	})
})

// CurrentOf is a test accessor for the conceptual restore phase.
func CurrentOf(rst *valkeyv1alpha1.PerconaValkeyRestore) restorePhase { return currentPhase(rst) }

// setClusterCondition sets the named condition True on the cluster status (used to
// script the cluster controller's re-form progress in envtest). It uses
// meta.SetStatusCondition so lastTransitionTime is populated (the apiserver requires
// it on every condition).
func setClusterCondition(cluster *valkeyv1alpha1.PerconaValkeyCluster, condType string) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             "Test",
		ObservedGeneration: cluster.Generation,
	})
}
