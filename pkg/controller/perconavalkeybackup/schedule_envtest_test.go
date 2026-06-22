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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	pvkbackup "valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeybackup"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
)

func scheduleBackupName(i int) string { return fmt.Sprintf("prod-nightly-r%d", i) }

func metav1Time(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

// scheduledCluster creates a Ready "prod" cluster carrying a nightly schedule.
func scheduledCluster(ns, cron string, keep int) *valkeyv1alpha1.PerconaValkeyCluster {
	cluster := readyCluster(ns)
	cluster.Spec.Backup.Schedule = []valkeyv1alpha1.BackupScheduleSpec{{
		Name:        "nightly",
		Schedule:    cron,
		StorageName: "s3-primary",
		Keep:        keep,
		Type:        valkeyv1alpha1.BackupTypeFull,
	}}
	gomega.Expect(k8sClient.Update(testCtx, cluster)).To(gomega.Succeed())
	return cluster
}

func listScheduleBackups(ns string) []valkeyv1alpha1.PerconaValkeyBackup {
	list := &valkeyv1alpha1.PerconaValkeyBackupList{}
	gomega.Expect(k8sClient.List(testCtx, list,
		client.InNamespace(ns),
		client.MatchingLabels{naming.LabelCluster: "prod", pvkbackup.LabelBackupSchedule: "nightly"},
	)).To(gomega.Succeed())
	return list.Items
}

var _ = ginkgo.Describe("PerconaValkeyBackup scheduler", func() {
	var (
		ns    string
		sched *pvkbackup.Scheduler
	)

	ginkgo.BeforeEach(func() {
		ns = freshNamespace()
		sched = pvkbackup.NewScheduler(k8sClient, apiScheme, events.NewFakeRecorder(200))
	})

	ginkgo.It("creates an owned, labelled backup on a due fire", func() {
		base := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
		cluster := scheduledCluster(ns, "*/5 * * * *", 0)
		gomega.Expect(sched.Sync(cluster, base)).To(gomega.Succeed())

		// Advance to 00:06 — the 00:05 slot has elapsed.
		created, err := sched.Tick(testCtx, cluster, base.Add(6*time.Minute))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(created).To(gomega.Equal(1))

		backups := listScheduleBackups(ns)
		gomega.Expect(backups).To(gomega.HaveLen(1))
		bk := backups[0]
		gomega.Expect(bk.Spec.ClusterName).To(gomega.Equal("prod"))
		gomega.Expect(bk.Spec.StorageName).To(gomega.Equal("s3-primary"))
		gomega.Expect(bk.OwnerReferences).To(gomega.HaveLen(1))
		gomega.Expect(bk.OwnerReferences[0].Name).To(gomega.Equal("prod"))
		gomega.Expect(bk.Annotations).To(gomega.HaveKey(pvkbackup.AnnScheduledAt))
	})

	ginkgo.It("fires at most one catch-up for many missed slots", func() {
		base := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
		cluster := scheduledCluster(ns, "*/5 * * * *", 0)
		gomega.Expect(sched.Sync(cluster, base)).To(gomega.Succeed())

		// Jump an hour: 12 slots elapsed, but only one catch-up fires.
		created, err := sched.Tick(testCtx, cluster, base.Add(time.Hour))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(created).To(gomega.Equal(1))
		gomega.Expect(listScheduleBackups(ns)).To(gomega.HaveLen(1))
	})

	ginkgo.It("skips an overlapping fire while a previous run is still active (Forbid)", func() {
		base := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
		cluster := scheduledCluster(ns, "*/5 * * * *", 0)
		gomega.Expect(sched.Sync(cluster, base)).To(gomega.Succeed())

		created, err := sched.Tick(testCtx, cluster, base.Add(6*time.Minute))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(created).To(gomega.Equal(1))

		// The first run is still New/active. Next slot must be skipped (Forbid).
		created, err = sched.Tick(testCtx, cluster, base.Add(11*time.Minute))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(created).To(gomega.Equal(0))
		gomega.Expect(listScheduleBackups(ns)).To(gomega.HaveLen(1))
	})

	ginkgo.It("removes schedules when backup is disabled", func() {
		base := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
		cluster := scheduledCluster(ns, "*/5 * * * *", 0)
		gomega.Expect(sched.Sync(cluster, base)).To(gomega.Succeed())
		cluster.Spec.Backup.Schedule = nil
		gomega.Expect(sched.Sync(cluster, base)).To(gomega.Succeed())

		created, err := sched.Tick(testCtx, cluster, base.Add(time.Hour))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(created).To(gomega.Equal(0))
	})

	ginkgo.It("rejects an invalid cron expression loudly", func() {
		cluster := scheduledCluster(ns, "not a cron", 0)
		gomega.Expect(sched.Sync(cluster, time.Now())).NotTo(gomega.Succeed())
	})
})

var _ = ginkgo.Describe("PerconaValkeyBackup retention GC", func() {
	var (
		ns    string
		sched *pvkbackup.Scheduler
	)

	ginkgo.BeforeEach(func() {
		ns = freshNamespace()
		sched = pvkbackup.NewScheduler(k8sClient, apiScheme, events.NewFakeRecorder(200))
	})

	ginkgo.It("deletes the surplus oldest Succeeded backups beyond keep", func() {
		base := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
		cluster := scheduledCluster(ns, "0 2 * * *", 2)

		// Seed four Succeeded backups labelled with the schedule, distinct completed.
		for i := 0; i < 4; i++ {
			bk := &valkeyv1alpha1.PerconaValkeyBackup{}
			bk.Name = scheduleBackupName(i)
			bk.Namespace = ns
			bk.Labels = naming.Labels("prod", "backup")
			bk.Labels[pvkbackup.LabelBackupSchedule] = "nightly"
			bk.Spec = valkeyv1alpha1.PerconaValkeyBackupSpec{ClusterName: "prod", StorageName: "s3-primary"}
			gomega.Expect(k8sClient.Create(testCtx, bk)).To(gomega.Succeed())
			bk.Status.State = valkeyv1alpha1.BackupStateSucceeded
			bk.Status.Completed = metav1Time(base.Add(time.Duration(i) * time.Hour))
			gomega.Expect(k8sClient.Status().Update(testCtx, bk)).To(gomega.Succeed())
		}

		deleted, err := sched.RunRetention(testCtx, cluster, "nightly", base.Add(10*time.Hour))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(deleted).To(gomega.Equal(2))
		// The two oldest are now being deleted (deletionTimestamp set; finalizer-free
		// here so they are GC'd). The two newest remain.
		gomega.Eventually(func() int {
			return len(listScheduleBackups(ns))
		}, timeout, interval).Should(gomega.Equal(2))
	})
})
