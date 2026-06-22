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

package perconavalkeybackup_test

import (
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	pvkbackup "valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeybackup"
)

var _ = ginkgo.Describe("PerconaValkeyBackup per-cluster Lease", func() {
	var (
		ns    string
		clk   time.Time
		fs    *backup.FakeStore
		recon *pvkbackup.Reconciler
	)

	ginkgo.BeforeEach(func() {
		ns = freshNamespace()
		clk = fakeNow
		fs = backup.NewFakeStore()
		recon = pvkbackup.NewReconcilerForTest(k8sClient, apiScheme, pvkbackup.FakeStoreFactory(fs), fixedClock(&clk))
	})

	reconcile := func(name string) {
		ginkgo.GinkgoHelper()
		_, err := recon.ReconcileForTest(testCtx, name, ns)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.It("leaves a second backup Starting while another holder owns the Lease", func() {
		readyCluster(ns)
		credsSecret(ns)

		// Pre-create a fresh Lease held by a "restore" actor.
		holder := "restore/" + ns + "/r1"
		dur := int32(30)
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: pvkbackup.LeaseName("prod"), Namespace: ns},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &dur,
				AcquireTime:          &metav1.MicroTime{Time: clk},
				RenewTime:            &metav1.MicroTime{Time: clk},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, lease)).To(gomega.Succeed())

		makeBackup(ns, "bk-second", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-second")

		bk := getBackup(ns, "bk-second")
		gomega.Expect(bk.Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateStarting))
		gomega.Expect(bk.Status.StateDescription).To(gomega.ContainSubstring("WaitingForLock"))

		// No Job created while blocked.
		job := &batchv1.Job{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-bk-second-backup", Namespace: ns}, job)
		gomega.Expect(err).To(gomega.HaveOccurred())

		// The Lease is still held by the restore actor (we never stole it).
		got := &coordinationv1.Lease{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: pvkbackup.LeaseName("prod"), Namespace: ns}, got)).To(gomega.Succeed())
		gomega.Expect(*got.Spec.HolderIdentity).To(gomega.Equal(holder))
	})

	ginkgo.It("acquires the Lease and proceeds once a stale holder's Lease expires (fail-open)", func() {
		readyCluster(ns)
		credsSecret(ns)

		// Pre-create a Lease whose renewTime is stale beyond leaseDurationSeconds.
		holder := "restore/" + ns + "/dead"
		dur := int32(30)
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: pvkbackup.LeaseName("prod"), Namespace: ns},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &dur,
				RenewTime:            &metav1.MicroTime{Time: clk.Add(-time.Hour)},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, lease)).To(gomega.Succeed())

		makeBackup(ns, "bk-failopen", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-failopen")

		gomega.Expect(getBackup(ns, "bk-failopen").Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateRunning))
		// The backup reclaimed the expired Lease.
		got := &coordinationv1.Lease{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: pvkbackup.LeaseName("prod"), Namespace: ns}, got)).To(gomega.Succeed())
		gomega.Expect(*got.Spec.HolderIdentity).To(gomega.ContainSubstring("backup/"))
	})

	ginkgo.It("IsBackupRunning reports true for a live backup Lease and false when absent", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-running", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-running") // acquires the Lease

		running, err := pvkbackup.IsBackupRunning(testCtx, k8sClient, ns, "prod", clk)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(running).To(gomega.BeTrue())

		// A cluster with no Lease fails open to "not running".
		notRunning, err := pvkbackup.IsBackupRunning(testCtx, k8sClient, ns, "no-such-cluster", clk)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(notRunning).To(gomega.BeFalse())
	})
})
