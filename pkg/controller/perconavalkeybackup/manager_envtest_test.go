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
	"context"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	pvkbackup "valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeybackup"
)

// This spec exercises SetupWithManager (For/Owns/Watches + the cluster->backups
// map func) end-to-end: a manager-backed reconciler reconciles a backup to
// Running, and a Job status flip re-enqueues the owner (Owns), driving it to
// Succeeded without a manual reconcile call.
var _ = ginkgo.Describe("PerconaValkeyBackup SetupWithManager wiring", func() {
	ginkgo.It("reconciles a backup to Succeeded via the manager's Owns(Job)/Watches", func() {
		ns := "pvkb-mgr"
		gomega.Expect(k8sClient.Create(testCtx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(gomega.Succeed())

		clk := fakeNow
		fs := backup.NewFakeStore()

		mgr, err := ctrl.NewManager(restCfg, manager.Options{
			Scheme:  apiScheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		mr := pvkbackup.NewReconcilerForTest(mgr.GetClient(), mgr.GetScheme(), pvkbackup.FakeStoreFactory(fs), fixedClock(&clk))
		mr.RecorderForTest(mgr.GetEventRecorder("perconavalkeybackup-mgr"))
		gomega.Expect(mr.SetupWithManager(mgr)).To(gomega.Succeed())

		mgrCtx, mgrCancel := context.WithCancel(testCtx)
		defer mgrCancel()
		go func() {
			defer ginkgo.GinkgoRecover()
			_ = mgr.Start(mgrCtx)
		}()
		gomega.Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(gomega.BeTrue())

		// Ready cluster + creds + backup in the watched namespace.
		cluster := &valkeyv1alpha1.PerconaValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns},
			Spec: valkeyv1alpha1.PerconaValkeyClusterSpec{
				Mode: valkeyv1alpha1.ModeCluster, Shards: 3, Replicas: 1,
				Backup: valkeyv1alpha1.BackupSpec{
					Image: "percona/valkey-backup:9.0.0",
					Storages: map[string]valkeyv1alpha1.BackupStorageSpec{
						"s3-primary": {Type: valkeyv1alpha1.BackupStorageS3, S3: &valkeyv1alpha1.BackupStorageS3Spec{
							Bucket: "b", CredentialsSecret: "s3-creds",
						}},
					},
				},
			},
		}
		gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
		cluster.Status.State = valkeyv1alpha1.StateReady
		gomega.Expect(k8sClient.Status().Update(testCtx, cluster)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Create(testCtx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: ns},
			Data: map[string][]byte{
				"AWS_ACCESS_KEY_ID": []byte("k"), "AWS_SECRET_ACCESS_KEY": []byte("s"),
			},
		})).To(gomega.Succeed())
		gomega.Expect(k8sClient.Create(testCtx, &valkeyv1alpha1.PerconaValkeyBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "mbk", Namespace: ns},
			Spec:       valkeyv1alpha1.PerconaValkeyBackupSpec{ClusterName: "prod", StorageName: "s3-primary"},
		})).To(gomega.Succeed())

		// The manager drives it to Running and creates the Job.
		gomega.Eventually(func() valkeyv1alpha1.BackupState {
			bk := &valkeyv1alpha1.PerconaValkeyBackup{}
			_ = k8sClient.Get(testCtx, types.NamespacedName{Name: "mbk", Namespace: ns}, bk)
			return bk.Status.State
		}, timeout, interval).Should(gomega.Equal(valkeyv1alpha1.BackupStateRunning))

		// Flip the Job to Complete; the Owns enqueue re-reconciles -> Succeeded.
		writeCompleteManifest(fs, "mbk")
		completeJob(ns, "valkey-mbk-backup")

		gomega.Eventually(func() valkeyv1alpha1.BackupState {
			bk := &valkeyv1alpha1.PerconaValkeyBackup{}
			_ = k8sClient.Get(testCtx, types.NamespacedName{Name: "mbk", Namespace: ns}, bk)
			return bk.Status.State
		}, timeout, interval).Should(gomega.Equal(valkeyv1alpha1.BackupStateSucceeded))
	})
})
