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
	"path/filepath"
	"testing"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// The envtest harness brings up a real apiserver+etcd with the generated CRDs
// installed. There is no kubelet/pod and no live Valkey, so the backup
// controller's Job is driven by manually patching batchv1.Job status, and the
// ArtifactStore is an injected backup.FakeStore (no real object store). A fixed
// clock makes Lease expiry / completion stamps deterministic.
var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config
	k8sClient client.Client
	testCtx   context.Context
	cancel    context.CancelFunc
	apiScheme = runtime.NewScheme()

	// fakeNow is the fixed clock the reconciler reads; specs advance it directly.
	fakeNow = time.Date(2026, 6, 22, 2, 0, 0, 0, time.UTC)
)

const (
	timeout  = 20 * time.Second
	interval = 200 * time.Millisecond
)

func TestPerconaValkeyBackupController(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "pkg/controller/perconavalkeybackup suite")
}

var _ = ginkgo.BeforeSuite(func() {
	logf.SetLogger(crzap.New(crzap.WriteTo(ginkgo.GinkgoWriter), crzap.UseDevMode(true)))
	testCtx, cancel = context.WithCancel(context.Background())

	// Register a fake S3 backend so BackendRegistered(s3) is true in
	// CheckNSetDefaults (the real backends land in the storage leg). The factory
	// the reconciler actually uses is injected per-spec (a shared FakeStore), so
	// this constructor is only consulted by the fail-fast presence check.
	backup.RegisterBackend(valkeyv1alpha1.BackupStorageS3, func(_ context.Context, _ backup.StorageConfig) (backup.ArtifactStore, error) {
		return backup.NewFakeStore(), nil
	})

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	restCfg, err = testEnv.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(restCfg).NotTo(gomega.BeNil())

	gomega.Expect(clientgoscheme.AddToScheme(apiScheme)).To(gomega.Succeed())
	gomega.Expect(valkeyv1alpha1.AddToScheme(apiScheme)).To(gomega.Succeed())

	k8sClient, err = client.New(restCfg, client.Options{Scheme: apiScheme})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(k8sClient).NotTo(gomega.BeNil())
})

var _ = ginkgo.AfterSuite(func() {
	if cancel != nil {
		cancel()
	}
	if testEnv != nil {
		gomega.Expect(testEnv.Stop()).To(gomega.Succeed())
	}
})

// makeNamespace creates a fresh namespace and returns its name.
func makeNamespace(name string) string {
	gomega.Expect(k8sClient.Create(testCtx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	})).To(gomega.Succeed())
	return name
}

// fixedClock returns a clock function reading a mutable time pointer so specs
// can advance the reconciler's clock deterministically.
func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}
