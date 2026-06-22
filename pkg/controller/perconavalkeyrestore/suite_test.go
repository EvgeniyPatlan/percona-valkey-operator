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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
)

// The envtest harness brings up a real apiserver+etcd with the generated CRDs
// installed. There is no kubelet/pod and no live Valkey, so the restore controller's
// downstream dependencies (the target cluster reaching Ready, the seed init
// container running) are driven manually: the restore is reconciled directly and the
// target PerconaValkeyCluster status/conditions and ValkeyNode statuses are written
// by the test to script each phase transition.
var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config
	k8sClient client.Client
	testCtx   context.Context
	cancel    context.CancelFunc
	apiScheme = runtime.NewScheme()
)

const (
	timeout  = 20 * time.Second
	interval = 200 * time.Millisecond
	// mgrNamespace isolates the manager-backed spec (which exercises
	// SetupWithManager's Owns/Watches wiring E2E) from the direct-Reconcile specs so
	// the two reconcile paths never race over the same objects.
	mgrNamespace = "pvk-rst-mgr-ns"
)

func TestPerconaValkeyRestoreController(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "pkg/controller/perconavalkeyrestore suite")
}

var _ = ginkgo.BeforeSuite(func() {
	logf.SetLogger(crzap.New(crzap.WriteTo(ginkgo.GinkgoWriter), crzap.UseDevMode(true)))
	testCtx, cancel = context.WithCancel(context.Background())

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

	gomega.Expect(k8sClient.Create(testCtx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: mgrNamespace},
	})).To(gomega.Succeed())

	// A shared manager wired via SetupWithManager exercises the For/Owns/Watches +
	// store-factory defaulting path E2E. Its cache is scoped to mgrNamespace so it
	// never reconciles the direct-spec objects. The injected storeFactory yields an
	// empty FakeStore so a restore reconciled here fails cleanly at ReadManifest
	// (which is enough to prove the manager wires and drives Reconcile).
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme:  apiScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{mgrNamespace: {}}},
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	mr := newReconcilerForTest(mgr.GetClient(), mgr.GetScheme(), fixedStoreFactory(backup.NewFakeStore()))
	gomega.Expect(mr.SetupWithManager(mgr)).To(gomega.Succeed())

	go func() {
		defer ginkgo.GinkgoRecover()
		if startErr := mgr.Start(testCtx); startErr != nil {
			logf.Log.Error(startErr, "shared manager Start returned error")
		}
	}()
	gomega.Expect(mgr.GetCache().WaitForCacheSync(testCtx)).To(gomega.BeTrue())
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
