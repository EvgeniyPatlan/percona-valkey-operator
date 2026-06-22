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

package perconavalkeybackup

import (
	"context"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// Backup Job conventions (06 §4.1, §4.8, §8.7).
const (
	// backupComponent is the app.kubernetes.io/component value on backup Jobs.
	backupComponent = "backup"
	// backupContainerName is the name of the cmd/valkey-backup container.
	backupContainerName = "valkey-backup"
	// labelBackup ties a Job to its owning PerconaValkeyBackup so the controller
	// re-adopts it by label on operator restart (06 §9.3) and the Owns enqueue
	// re-reconciles the backup on a Job phase change.
	labelBackup = "valkey.percona.com/backup"
	// defaultBackupImage is the fallback cmd/valkey-backup image when neither the
	// cluster spec.backup.image nor an operator default is set.
	defaultBackupImage = "percona/valkey-backup:latest"
	// defaultActiveDeadlineSeconds caps backup Job runtime (06 §3.1).
	defaultActiveDeadlineSeconds int64 = 3600
)

// backupJobName returns the Job name for a backup: valkey-<backup>-backup. It is
// deterministic so re-adoption by name/label is idempotent on restart.
func backupJobName(bk *valkeyv1alpha1.PerconaValkeyBackup) string {
	return naming.ResourcePrefix + bk.Name + "-backup"
}

// createBackupJob builds and creates the single backup Job (06 §4.1), records the
// Running condition, and emits BackupStarted. It is idempotent: an already-present
// Job (re-adopted on restart) is left untouched. The caller has already acquired
// the Lease and validated storage/creds.
func (r *Reconciler) createBackupJob(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, rs *resolvedStorage) error {
	existing, err := r.getBackupJob(ctx, bk)
	if err != nil {
		return err
	}
	if existing != nil {
		// Already created (re-adopt on restart) — nothing to do.
		return nil
	}
	job, err := r.desiredBackupJob(ctx, bk, rs)
	if err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create backup job %q: %w", job.Name, err)
	}
	r.recorder.Eventf(bk, job, eventNormal, EventBackupStarted, "Backup",
		"created backup Job %s for cluster %s storage %s", job.Name, bk.Spec.ClusterName, rs.name)
	if bk.Status.Start == nil {
		now := metav1.NewTime(r.now())
		bk.Status.Start = &now
	}
	return nil
}

// getBackupJob fetches this backup's Job by its deterministic name, returning
// (nil, nil) when absent.
func (r *Reconciler) getBackupJob(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	key := types.NamespacedName{Name: backupJobName(bk), Namespace: bk.Namespace}
	if err := r.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get backup job %q: %w", key.Name, err)
	}
	return job, nil
}

// desiredBackupJob renders the streaming backup Job (06 §4.8, §8.7): modest
// requests, NO limits (so a healthy stream is never OOM-killed), the credentials
// Secret mounted as env, restartPolicy Never, and activeDeadlineSeconds from spec.
// The cluster CR owns the Job (Owns enqueue) but the backup CR is the controller
// owner-ref so a Job phase change re-reconciles the backup.
func (r *Reconciler) desiredBackupJob(
	ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, rs *resolvedStorage,
) (*batchv1.Job, error) {
	cluster := &valkeyv1alpha1.PerconaValkeyCluster{}
	key := types.NamespacedName{Name: bk.Spec.ClusterName, Namespace: bk.Namespace}
	if err := r.Get(ctx, key, cluster); err != nil {
		return nil, fmt.Errorf("get cluster %q for job spec: %w", bk.Spec.ClusterName, err)
	}

	labels := naming.Labels(bk.Spec.ClusterName, backupComponent)
	labels[labelBackup] = bk.Name

	backoff := int32(0)
	deadline := defaultActiveDeadlineSeconds
	if bk.Spec.ActiveDeadlineSeconds != nil {
		deadline = *bk.Spec.ActiveDeadlineSeconds
	}

	// The sidecar takes NO --backup/--cluster/--storage flags (06 §4.1, §8.7): it
	// runs in backup mode by default and reads every operation input from the
	// VALKEY_BACKUP_* env. The default args carry only any spec.containerOptions.args.
	container := corev1.Container{
		Name:      backupContainerName,
		Image:     backupImage(cluster),
		Args:      containerExtraArgs(bk),
		Env:       append(backupJobEnv(bk, cluster, rs), containerExtraEnv(bk)...),
		Resources: backupJobResources(),
	}
	if rs.credsSecret != "" {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: rs.credsSecret},
			},
		}}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupJobName(bk),
			Namespace: bk.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cluster.Spec.Backup.ServiceAccountName,
					Containers:         []corev1.Container{container},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(bk, job, r.scheme); err != nil {
		return nil, fmt.Errorf("set backup job owner-ref: %w", err)
	}
	return job, nil
}

// backupJobEnv builds the VALKEY_BACKUP_* env the sidecar reads in backup mode
// (06 §4.1, §8.7): the storage type + coordinates, the cluster/backup names that
// derive object keys, the manifest-populating fields, and the engine-connection
// inputs (seed node, _backup user, TLS). The _backup password is injected via a
// SecretKeyRef into VALKEY_BACKUP_AUTH_PASSWORD from the cluster's system-passwords
// Secret so the value never transits the operator process or the Job spec
// (06 §8.2 / 8.3). The storage-credential VALUES arrive separately via the
// credentials Secret mounted as EnvFrom (set by the caller).
//
// M6 SECURITY REFACTOR (07 §10): the Job authenticates as _backup, not _operator.
// _backup carries the snapshot + SYNC-as-replica replication grants (see
// pkg/valkey/acl.go backupRules); _operator was narrowed back to orchestration-only.
func backupJobEnv(
	bk *valkeyv1alpha1.PerconaValkeyBackup, cluster *valkeyv1alpha1.PerconaValkeyCluster, rs *resolvedStorage,
) []corev1.EnvVar {
	consistency := string(valkeyv1alpha1.ConsistencyStrict)
	if bk.Spec.Consistency != "" {
		consistency = string(bk.Spec.Consistency)
	}
	mode := string(valkeyv1alpha1.ModeCluster)
	if cluster.Spec.Mode != "" {
		mode = string(cluster.Spec.Mode)
	}
	env := backupEnvFromParams(backup.JobEnvParams{
		Cluster:     bk.Spec.ClusterName,
		Backup:      bk.Name,
		Spec:        rs.spec,
		Mode:        mode,
		CRVersion:   cluster.Spec.CrVersion,
		Consistency: consistency,
		SeedNode:    backupSeedNode(cluster.Name),
		AuthUser:    naming.SystemUserBackup,
		TLSEnabled:  cluster.Spec.TLS != nil,
		TLSCAFile:   tlsCAFilePath(cluster),
	})
	// The _backup password value comes from the cluster's system-passwords Secret
	// keyed by the username (see acl.go) — referenced, never read by the operator.
	env = append(env, corev1.EnvVar{
		Name: backup.EnvAuthPassword,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: naming.SystemPasswordsSecretName(cluster.Name),
				},
				Key:      naming.SystemUserBackup,
				Optional: ptrBool(true),
			},
		},
	})
	return env
}

// backupEnvFromParams maps the backend-agnostic backup.JobEnv key/value list to
// corev1.EnvVar (pkg/backup deliberately has no core/v1 dependency at the seam).
func backupEnvFromParams(p backup.JobEnvParams) []corev1.EnvVar {
	kvs := backup.JobEnv(p)
	out := make([]corev1.EnvVar, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, corev1.EnvVar{Name: kv.Name, Value: kv.Value})
	}
	return out
}

// backupSeedNode returns a reachable Valkey endpoint the backup Job scrapes
// CLUSTER NODES from: the cluster's headless Service on the client port. The Job
// resolves each shard's LIVE primary from that scrape (never labels — 06 §1.3).
func backupSeedNode(cluster string) string {
	return naming.HeadlessServiceName(cluster) + ":" + strconv.Itoa(valkey.ClientPort)
}

// tlsCAFilePath returns the in-pod CA bundle path the Job uses for engine TLS, or
// "" when TLS is off. The CA is mounted from the cluster TLS material (M5 wires
// the volume mount; the env contract is fixed here).
func tlsCAFilePath(cluster *valkeyv1alpha1.PerconaValkeyCluster) string {
	if cluster.Spec.TLS == nil {
		return ""
	}
	return tlsCAMountPath
}

// tlsCAMountPath is the conventional mount path for the engine-TLS CA bundle in a
// backup/cleanup Job pod.
const tlsCAMountPath = "/etc/valkey/tls/ca.crt"

// ptrBool returns a pointer to b (for optional Secret key refs).
func ptrBool(b bool) *bool { return &b }

// backupImage returns the cmd/valkey-backup image: the cluster's
// spec.backup.image when set, else a conservative default.
func backupImage(cluster *valkeyv1alpha1.PerconaValkeyCluster) string {
	if cluster.Spec.Backup.Image != "" {
		return cluster.Spec.Backup.Image
	}
	return defaultBackupImage
}

// backupJobResources returns the conservative requests-only resources for the
// streaming backup Job: modest requests and NO limits (06 §4.8).
func backupJobResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

// containerExtraArgs returns any spec.containerOptions.args appended to the Job.
func containerExtraArgs(bk *valkeyv1alpha1.PerconaValkeyBackup) []string {
	if bk.Spec.ContainerOptions == nil {
		return nil
	}
	return bk.Spec.ContainerOptions.Args
}

// containerExtraEnv returns any spec.containerOptions.env for the Job container.
func containerExtraEnv(bk *valkeyv1alpha1.PerconaValkeyBackup) []corev1.EnvVar {
	if bk.Spec.ContainerOptions == nil {
		return nil
	}
	return bk.Spec.ContainerOptions.Env
}

// watchJob inspects the backup Job and advances the phase machine. It returns
// done=true when a terminal verdict was reached this pass (the caller writes
// status and stops). A still-running Job leaves the backup Running. The verdict
// taxonomy (06 §4.8, §9.3): Complete -> hydrate from manifest + Succeeded/partial;
// evicted -> Failed + BackupJobEvicted; deadline exceeded -> Failed; other Failed
// -> Failed.
func (r *Reconciler) watchJob(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, rs *resolvedStorage) (done bool, err error) {
	job, err := r.getBackupJob(ctx, bk)
	if err != nil {
		return false, err
	}
	if job == nil {
		// Job vanished mid-run (deleted/GC'd) — re-create it (idempotent re-drive,
		// 06 §9.3). The caller treats done=false as "still working".
		if cerr := r.createBackupJob(ctx, bk, rs); cerr != nil {
			return false, cerr
		}
		return false, nil
	}

	switch {
	case jobConditionTrue(job, batchv1.JobComplete):
		r.handleJobComplete(ctx, bk, rs)
		return true, nil
	case jobEvicted(job):
		r.failBackup(bk, ReasonJobEvicted, "backup Job evicted (node pressure/preemption/drain)")
		r.recorder.Eventf(bk, job, eventWarning, EventBackupJobEvicted, "Backup",
			"backup Job %s evicted; backup failed, cluster will resume when the Lease expires", job.Name)
		return true, nil
	case jobConditionTrue(job, batchv1.JobFailed):
		reason, msg := jobFailureReason(job)
		r.failBackup(bk, reason, msg)
		return true, nil
	default:
		// Still running — keep Running and renew the Lease.
		setState(bk, valkeyv1alpha1.BackupStateRunning, ReasonJobRunning+": backup Job in progress")
		if rerr := r.renewClusterLease(ctx, bk); rerr != nil {
			return false, rerr
		}
		return false, nil
	}
}

// handleJobComplete reads the manifest the completed Job wrote, hydrates status
// from it, gates on slot coverage, and records the terminal verdict (06 §4.5,
// §4.6). A complete-coverage set is Succeeded; a partial set under best-effort is
// Succeeded with slotCoverage=partial + BackupDegraded (no out-of-enum Degraded
// state, see status.go). A missing manifest (Job said Complete but wrote nothing)
// is a loud Failed.
func (r *Reconciler) handleJobComplete(ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, rs *resolvedStorage) {
	man, err := r.readManifest(ctx, bk, rs)
	if err != nil {
		r.failBackup(bk, ReasonManifestError, fmt.Sprintf("backup Job completed but manifest unreadable: %v", err))
		return
	}
	hydrateFromManifest(bk, man)

	now := metav1.NewTime(r.now())
	bk.Status.Completed = &now

	if bk.Status.SlotCoverage == valkeyv1alpha1.SlotCoverageComplete {
		setState(bk, valkeyv1alpha1.BackupStateSucceeded, ReasonSucceeded+": all 16384 slots covered")
		r.recorder.Eventf(bk, nil, eventNormal, EventBackupSucceeded, "Backup",
			"backup %s succeeded with complete slot coverage", bk.Name)
		return
	}

	// Partial coverage. Strict consistency must NOT reach here with a Complete
	// Job (the Job fails the whole backup under strict per 06 §4.4); if it does,
	// treat as a loud failure. Best-effort records a Succeeded-with-warning
	// (slotCoverage=partial) plus a BackupDegraded Event — the v1alpha1 enum has
	// no Degraded state (status.go, OPEN QUESTION Q3), so the warning is loud, not
	// silent, via the Event and partial coverage.
	if bk.Spec.Consistency == valkeyv1alpha1.ConsistencyStrict {
		r.failBackup(bk, ReasonIncompleteCover, "strict backup with partial slot coverage")
		return
	}
	setState(bk, valkeyv1alpha1.BackupStateSucceeded, ReasonDegraded+": partial slot coverage (best-effort)")
	r.recorder.Eventf(bk, nil, eventWarning, EventBackupDegraded, "Backup",
		"backup %s degraded: partial slot coverage under best-effort", bk.Name)
}

// readManifest opens an ArtifactStore for the resolved storage and reads the
// backup-set manifest. In the operator process the StorageConfig carries no
// credential values (presence-checked only, 06 §8.2); tests inject a FakeStore
// via storeFactory so this is exercised hermetically.
func (r *Reconciler) readManifest(
	ctx context.Context, bk *valkeyv1alpha1.PerconaValkeyBackup, rs *resolvedStorage,
) (backup.Manifest, error) {
	store, err := r.storeFactory(ctx, backup.StorageConfigFromSpec(rs.spec, nil))
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("open store: %w", err)
	}
	return backup.ReadManifest(ctx, store, backup.ManifestKey(bk.Spec.ClusterName, bk.Name))
}

// hydrateFromManifest copies the manifest's coverage/version fields and per-shard
// fragments into the backup status (06 §3.2, §4.6).
func hydrateFromManifest(bk *valkeyv1alpha1.PerconaValkeyBackup, man backup.Manifest) {
	bk.Status.ValkeyVersion = man.EngineVersion
	switch man.SlotCoverage {
	case string(valkeyv1alpha1.SlotCoverageComplete):
		bk.Status.SlotCoverage = valkeyv1alpha1.SlotCoverageComplete
	default:
		bk.Status.SlotCoverage = valkeyv1alpha1.SlotCoveragePartial
	}
	shards := make([]valkeyv1alpha1.ShardBackupStatus, 0, len(man.Shards))
	for _, s := range man.Shards {
		shards = append(shards, valkeyv1alpha1.ShardBackupStatus{
			ShardIndex: int32(s.Index),
			SlotRange:  s.SlotRanges,
			RDBObject:  s.RDBKey,
			SizeBytes:  s.SizeBytes,
			Checksum:   s.SHA256,
		})
	}
	bk.Status.Shards = shards
}

// failBackup records a terminal failure on the backup (state=Failed with the
// reason), sets the completion time, and emits BackupFailed. The Lease release is
// performed by the caller's terminal path.
func (r *Reconciler) failBackup(bk *valkeyv1alpha1.PerconaValkeyBackup, reason, msg string) {
	now := metav1.NewTime(r.now())
	if bk.Status.Completed == nil {
		bk.Status.Completed = &now
	}
	setState(bk, valkeyv1alpha1.BackupStateFailed, reason+": "+msg)
	r.recorder.Eventf(bk, nil, eventWarning, EventBackupFailed, "Backup", "backup %s failed: %s", bk.Name, msg)
}

// jobConditionTrue reports whether the Job has the given condition set True.
func jobConditionTrue(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobEvicted reports whether the Job failed due to an eviction. The kube
// eviction surfaces as a Failed Job whose reason/message names DeadlineExceeded
// vs an eviction; we treat a Failed condition whose reason contains "Evicted" or
// whose message mentions eviction as an eviction (06 §4.8).
func jobEvicted(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Reason == jobReasonEvicted || containsFold(c.Message, "evict") {
				return true
			}
		}
	}
	return false
}

// jobReasonEvicted is the conventional reason stamped on an eviction-failed Job.
const jobReasonEvicted = "Evicted"

// jobFailureReason maps a Failed Job's condition to a backup failure reason/msg.
func jobFailureReason(job *batchv1.Job) (reason, msg string) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Reason == batchv1.JobReasonDeadlineExceeded {
				return ReasonDeadlineExceeded, "backup Job exceeded activeDeadlineSeconds"
			}
			if c.Message != "" {
				return ReasonJobCreateError, "backup Job failed: " + c.Message
			}
			return ReasonJobCreateError, "backup Job failed: " + c.Reason
		}
	}
	return ReasonJobCreateError, "backup Job failed (failed pods: " + strconv.Itoa(int(job.Status.Failed)) + ")"
}

// containsFold reports whether s contains substr, case-insensitively, without
// pulling in strings.Contains-fold gymnastics at every call site.
func containsFold(s, substr string) bool {
	if substr == "" {
		return true
	}
	ls, lsub := toLowerASCII(s), toLowerASCII(substr)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return true
		}
	}
	return false
}

// toLowerASCII lowercases ASCII letters in s (sufficient for matching the fixed
// English "evict" token in a Job reason/message).
func toLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
