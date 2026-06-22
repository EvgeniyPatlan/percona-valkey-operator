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
	"fmt"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	pvkbackup "valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeybackup"
)

var nsCounter int

func freshNamespace() string {
	nsCounter++
	return makeNamespace(fmt.Sprintf("pvkb-%d", nsCounter))
}

// readyCluster creates the "prod" PerconaValkeyCluster with an s3 storage and
// marks it Ready so the backup's snapshot-readiness precondition passes.
func readyCluster(ns string) *valkeyv1alpha1.PerconaValkeyCluster {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns},
		Spec: valkeyv1alpha1.PerconaValkeyClusterSpec{
			Mode:     valkeyv1alpha1.ModeCluster,
			Shards:   3,
			Replicas: 1,
			Backup: valkeyv1alpha1.BackupSpec{
				Image: "percona/valkey-backup:9.0.0",
				Storages: map[string]valkeyv1alpha1.BackupStorageSpec{
					"s3-primary": {
						Type: valkeyv1alpha1.BackupStorageS3,
						S3: &valkeyv1alpha1.BackupStorageS3Spec{
							Bucket:            "valkey-backups",
							Prefix:            "prod/",
							Region:            "eu-central-1",
							CredentialsSecret: "s3-creds",
						},
					},
				},
			},
		},
	}
	gomega.Expect(k8sClient.Create(testCtx, cluster)).To(gomega.Succeed())
	cluster.Status.State = valkeyv1alpha1.StateReady
	gomega.Expect(k8sClient.Status().Update(testCtx, cluster)).To(gomega.Succeed())
	return cluster
}

// credsSecret creates the S3 credentials Secret the presence-check needs.
func credsSecret(ns string) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: ns},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("minio"),
			"AWS_SECRET_ACCESS_KEY": []byte("minio123"),
		},
	}
	gomega.Expect(k8sClient.Create(testCtx, s)).To(gomega.Succeed())
}

// makeBackup creates an on-demand PerconaValkeyBackup targeting the "prod"
// cluster (the single cluster name used across this suite).
func makeBackup(ns, name string, consistency valkeyv1alpha1.BackupConsistency) *valkeyv1alpha1.PerconaValkeyBackup {
	bk := &valkeyv1alpha1.PerconaValkeyBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: valkeyv1alpha1.PerconaValkeyBackupSpec{
			ClusterName: "prod",
			StorageName: "s3-primary",
			Consistency: consistency,
		},
	}
	gomega.Expect(k8sClient.Create(testCtx, bk)).To(gomega.Succeed())
	return bk
}

// getBackup re-reads a backup.
func getBackup(ns, name string) *valkeyv1alpha1.PerconaValkeyBackup {
	bk := &valkeyv1alpha1.PerconaValkeyBackup{}
	gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, bk)).To(gomega.Succeed())
	return bk
}

// completeJob patches the named Job's status to Complete (simulating the Job
// controller, which does not run under envtest). The apiserver enforces Job
// status invariants: a Complete=True condition needs SuccessCriteriaMet=True and
// startTime/completionTime set.
func completeJob(ns, jobName string) {
	job := &batchv1.Job{}
	gomega.Eventually(func() error {
		return k8sClient.Get(testCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)
	}, timeout, interval).Should(gomega.Succeed())
	start := metav1.NewTime(fakeNow)
	done := metav1.NewTime(fakeNow.Add(time.Minute))
	job.Status.StartTime = &start
	job.Status.CompletionTime = &done
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, Reason: "CompletionsReached"},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "CompletionsReached"},
	}
	job.Status.Succeeded = 1
	gomega.Expect(k8sClient.Status().Update(testCtx, job)).To(gomega.Succeed())
}

// failJobEvicted patches the named Job's status to a Failed/evicted condition.
// The apiserver requires startTime on a finished Job.
func failJobEvicted(ns, jobName string) {
	job := &batchv1.Job{}
	gomega.Eventually(func() error {
		return k8sClient.Get(testCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)
	}, timeout, interval).Should(gomega.Succeed())
	start := metav1.NewTime(fakeNow)
	job.Status.StartTime = &start
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
			Message: "Pod was evicted due to node pressure"},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
			Message: "Pod was evicted due to node pressure"},
	}
	job.Status.Failed = 1
	gomega.Expect(k8sClient.Status().Update(testCtx, job)).To(gomega.Succeed())
}

// failJobDeadline patches the named Job's status to a Failed/DeadlineExceeded
// condition (06 §9.3 activeDeadlineSeconds path).
func failJobDeadline(ns, jobName string) {
	job := &batchv1.Job{}
	gomega.Eventually(func() error {
		return k8sClient.Get(testCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)
	}, timeout, interval).Should(gomega.Succeed())
	start := metav1.NewTime(fakeNow)
	job.Status.StartTime = &start
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
			Message: "Job was active longer than specified deadline"},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
			Message: "Job was active longer than specified deadline"},
	}
	job.Status.Failed = 1
	gomega.Expect(k8sClient.Status().Update(testCtx, job)).To(gomega.Succeed())
}

// waitJobGone blocks until the named Job is fully removed from the apiserver.
func waitJobGone(ns, jobName string) {
	gomega.Eventually(func() bool {
		job := &batchv1.Job{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)
		return apierrors.IsNotFound(err)
	}, timeout, interval).Should(gomega.BeTrue())
}

// writeCompleteManifest writes a full-coverage manifest into the FakeStore at the
// backup-set manifest key so the controller hydrates Succeeded on Job complete.
func writeCompleteManifest(fs *backup.FakeStore, name string) {
	const cluster = "prod"
	man := backup.Manifest{
		Cluster:       cluster,
		BackupName:    name,
		Mode:          "cluster",
		EngineVersion: "9.0.0",
		Consistency:   "strict",
		SlotCoverage:  "complete",
		CreatedAt:     fakeNow.Format(time.RFC3339),
		Shards: []backup.ShardManifest{
			{Index: 0, SlotRanges: "0-5460", RDBKey: backup.ShardRDBRelKey(0), SizeBytes: 10, SHA256: "a"},
			{Index: 1, SlotRanges: "5461-10922", RDBKey: backup.ShardRDBRelKey(1), SizeBytes: 10, SHA256: "b"},
			{Index: 2, SlotRanges: "10923-16383", RDBKey: backup.ShardRDBRelKey(2), SizeBytes: 10, SHA256: "c"},
		},
	}
	gomega.Expect(backup.WriteManifest(testCtx, fs, backup.ManifestKey(cluster, name), man)).To(gomega.Succeed())
}

// writePartialManifest writes a partial-coverage manifest (best-effort).
func writePartialManifest(fs *backup.FakeStore, name string) {
	const cluster = "prod"
	man := backup.Manifest{
		Cluster:       cluster,
		BackupName:    name,
		EngineVersion: "9.0.0",
		Consistency:   "best-effort",
		SlotCoverage:  "partial",
		Shards: []backup.ShardManifest{
			{Index: 0, SlotRanges: "0-5460", RDBKey: backup.ShardRDBRelKey(0), SizeBytes: 10, SHA256: "a"},
		},
	}
	gomega.Expect(backup.WriteManifest(testCtx, fs, backup.ManifestKey(cluster, name), man)).To(gomega.Succeed())
}

var _ = ginkgo.Describe("PerconaValkeyBackup phase machine", func() {
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

	ginkgo.It("walks New -> Starting/Running -> Succeeded on a complete-coverage Job", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk1", valkeyv1alpha1.ConsistencyStrict)

		reconcile("bk1") // resolves storage, acquires Lease, creates Job -> Running
		gomega.Expect(getBackup(ns, "bk1").Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateRunning))
		gomega.Expect(getBackup(ns, "bk1").Status.Destination).To(gomega.HavePrefix(backup.SchemeS3))

		writeCompleteManifest(fs, "bk1")
		completeJob(ns, "valkey-bk1-backup")

		reconcile("bk1") // watches Job -> Succeeded
		bk := getBackup(ns, "bk1")
		gomega.Expect(bk.Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateSucceeded))
		gomega.Expect(bk.Status.SlotCoverage).To(gomega.Equal(valkeyv1alpha1.SlotCoverageComplete))
		gomega.Expect(bk.Status.Shards).To(gomega.HaveLen(3))
		gomega.Expect(bk.Status.ValkeyVersion).To(gomega.Equal("9.0.0"))
		gomega.Expect(bk.Status.Completed).NotTo(gomega.BeNil())
	})

	ginkgo.It("fails loudly on an unknown storageName", func() {
		readyCluster(ns)
		credsSecret(ns)
		bk := makeBackup(ns, "bk-badstore", valkeyv1alpha1.ConsistencyStrict)
		bk.Spec.StorageName = "typo"
		// recreate with the bad name (immutable in prod, but we just patch the spec here)
		gomega.Expect(k8sClient.Delete(testCtx, bk)).To(gomega.Succeed())
		bk2 := &valkeyv1alpha1.PerconaValkeyBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-typo", Namespace: ns},
			Spec:       valkeyv1alpha1.PerconaValkeyBackupSpec{ClusterName: "prod", StorageName: "typo"},
		}
		gomega.Expect(k8sClient.Create(testCtx, bk2)).To(gomega.Succeed())

		reconcile("bk-typo")
		gomega.Expect(getBackup(ns, "bk-typo").Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateFailed))
	})

	ginkgo.It("fails when the credentials Secret is missing a required key", func() {
		readyCluster(ns)
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: ns},
			Data:       map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("only-one")},
		}
		gomega.Expect(k8sClient.Create(testCtx, s)).To(gomega.Succeed())
		makeBackup(ns, "bk-nokey", valkeyv1alpha1.ConsistencyStrict)

		reconcile("bk-nokey")
		bk := getBackup(ns, "bk-nokey")
		gomega.Expect(bk.Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateFailed))
		gomega.Expect(bk.Status.StateDescription).To(gomega.ContainSubstring("AWS_SECRET_ACCESS_KEY"))
	})

	ginkgo.It("stays Starting while the source cluster is not Ready", func() {
		cluster := readyCluster(ns)
		cluster.Status.State = valkeyv1alpha1.StateInitializing
		gomega.Expect(k8sClient.Status().Update(testCtx, cluster)).To(gomega.Succeed())
		credsSecret(ns)
		makeBackup(ns, "bk-wait", valkeyv1alpha1.ConsistencyStrict)

		reconcile("bk-wait")
		bk := getBackup(ns, "bk-wait")
		gomega.Expect(bk.Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateStarting))
		// No Job created yet.
		job := &batchv1.Job{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-bk-wait-backup", Namespace: ns}, job)
		gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
		gomega.Expect(err).To(gomega.HaveOccurred())
	})

	ginkgo.It("marks Failed and emits eviction on an evicted Job, releasing the Lease", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-evict", valkeyv1alpha1.ConsistencyStrict)

		reconcile("bk-evict") // -> Running, Lease held
		failJobEvicted(ns, "valkey-bk-evict-backup")
		reconcile("bk-evict") // -> Failed + Lease released

		gomega.Expect(getBackup(ns, "bk-evict").Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateFailed))
		// Lease released (holder cleared) so the cluster resumes.
		lease := &coordinationv1.Lease{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: pvkbackup.LeaseName("prod"), Namespace: ns}, lease)).To(gomega.Succeed())
		gomega.Expect(lease.Spec.HolderIdentity).To(gomega.BeNil())
	})

	ginkgo.It("records partial coverage + Succeeded under best-effort", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-best", valkeyv1alpha1.ConsistencyBestEffort)

		reconcile("bk-best")
		writePartialManifest(fs, "bk-best")
		completeJob(ns, "valkey-bk-best-backup")
		reconcile("bk-best")

		bk := getBackup(ns, "bk-best")
		gomega.Expect(bk.Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateSucceeded))
		gomega.Expect(bk.Status.SlotCoverage).To(gomega.Equal(valkeyv1alpha1.SlotCoveragePartial))
	})

	ginkgo.It("fails on activeDeadlineSeconds exceeded", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-deadline", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-deadline")
		failJobDeadline(ns, "valkey-bk-deadline-backup")
		reconcile("bk-deadline")
		bk := getBackup(ns, "bk-deadline")
		gomega.Expect(bk.Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateFailed))
		gomega.Expect(bk.Status.StateDescription).To(gomega.ContainSubstring("DeadlineExceeded"))
	})

	ginkgo.It("fails strict when the Job completes but coverage is partial", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-strictpartial", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-strictpartial")
		writePartialManifest(fs, "bk-strictpartial")
		completeJob(ns, "valkey-bk-strictpartial-backup")
		reconcile("bk-strictpartial")
		gomega.Expect(getBackup(ns, "bk-strictpartial").Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateFailed))
	})

	ginkgo.It("fails when the Job completes but the manifest is missing", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-nomanifest", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-nomanifest")
		completeJob(ns, "valkey-bk-nomanifest-backup") // no manifest written
		reconcile("bk-nomanifest")
		bk := getBackup(ns, "bk-nomanifest")
		gomega.Expect(bk.Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateFailed))
		gomega.Expect(bk.Status.StateDescription).To(gomega.ContainSubstring("ManifestError"))
	})

	ginkgo.It("re-creates a vanished Job and renews the Lease across Running passes", func() {
		readyCluster(ns)
		credsSecret(ns)
		makeBackup(ns, "bk-readopt", valkeyv1alpha1.ConsistencyStrict)
		reconcile("bk-readopt") // Running, Job created

		// Simulate the Job vanishing (operator restart / GC): delete it, then a
		// Running reconcile must re-create it idempotently and renew the Lease.
		job := &batchv1.Job{}
		gomega.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-bk-readopt-backup", Namespace: ns}, job)).To(gomega.Succeed())
		bg := metav1.DeletePropagationBackground
		gomega.Expect(k8sClient.Delete(testCtx, job, &client.DeleteOptions{PropagationPolicy: &bg})).To(gomega.Succeed())
		waitJobGone(ns, "valkey-bk-readopt-backup")

		reconcile("bk-readopt") // re-creates the Job
		gomega.Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: "valkey-bk-readopt-backup", Namespace: ns}, &batchv1.Job{})
		}, timeout, interval).Should(gomega.Succeed())
		gomega.Expect(getBackup(ns, "bk-readopt").Status.State).To(gomega.Equal(valkeyv1alpha1.BackupStateRunning))
	})
})
