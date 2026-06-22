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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	pvkbackup "valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeybackup"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

var _ = ginkgo.Describe("PerconaValkeyBackup finalizer cleanup", func() {
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

	ginkgo.It("adds the delete-backup finalizer on first reconcile", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-fin", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-fin")
		gomega.Expect(controllerutil.ContainsFinalizer(getBackup(ns, "bk-fin"), naming.FinalizerDeleteBackup)).To(gomega.BeTrue())
	})

	ginkgo.It("runs a cleanup Job on delete, removing the finalizer when the Job completes", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-del", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-del") // adds finalizer, creates backup Job

		// Mark the backup terminal (Succeeded) by completing its Job.
		writeCompleteManifest(fs, "bk-del")
		completeJob(ns, "valkey-bk-del-backup")
		reconcile("bk-del")
		gomega.Expect(getBackup(ns, "bk-del").Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateSucceeded))

		// Delete: the API server sets deletionTimestamp; the finalizer holds the CR.
		gomega.Expect(k8sClient.Delete(testCtx, getBackup(ns, "bk-del"))).To(gomega.Succeed())
		reconcile("bk-del") // spawns the cleanup Job
		// CR still present (Terminating) — finalizer not yet removed.
		gomega.Expect(getBackup(ns, "bk-del").DeletionTimestamp).NotTo(gomega.BeNil())

		// Complete the cleanup Job; next reconcile removes the finalizer -> GC.
		completeJob(ns, "valkey-bk-del-cleanup")
		reconcile("bk-del")

		gomega.Eventually(func() bool {
			bk := &valkeyv1alpha1.PerconaValkeyBackup{}
			err := k8sClient.Get(testCtx, types.NamespacedName{Name: "bk-del", Namespace: ns}, bk)
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(gomega.BeTrue())
	})

	ginkgo.It("retains the finalizer (stays Terminating) when the cleanup Job fails, then self-heals", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-stuck", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-stuck")
		writeCompleteManifest(fs, "bk-stuck")
		completeJob(ns, "valkey-bk-stuck-backup")
		reconcile("bk-stuck")

		gomega.Expect(k8sClient.Delete(testCtx, getBackup(ns, "bk-stuck"))).To(gomega.Succeed())
		reconcile("bk-stuck") // cleanup Job created

		// Cleanup Job fails: finalizer retained, CR stays Terminating.
		failJobEvicted(ns, "valkey-bk-stuck-cleanup")
		reconcile("bk-stuck") // observes failed cleanup; deletes the failed Job, retries
		gomega.Expect(getBackup(ns, "bk-stuck").DeletionTimestamp).NotTo(gomega.BeNil())
		gomega.Expect(controllerutil.ContainsFinalizer(getBackup(ns, "bk-stuck"), naming.FinalizerDeleteBackup)).To(gomega.BeTrue())

		// Wait for the failed Job to be reaped, then the finalizer-audit retry
		// re-creates a fresh cleanup Job; complete it -> GC.
		waitJobGone(ns, "valkey-bk-stuck-cleanup")
		reconcile("bk-stuck") // re-creates the cleanup Job (idempotent)
		completeJob(ns, "valkey-bk-stuck-cleanup")
		reconcile("bk-stuck")
		gomega.Eventually(func() bool {
			bk := &valkeyv1alpha1.PerconaValkeyBackup{}
			err := k8sClient.Get(testCtx, types.NamespacedName{Name: "bk-stuck", Namespace: ns}, bk)
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(gomega.BeTrue())
	})
})
