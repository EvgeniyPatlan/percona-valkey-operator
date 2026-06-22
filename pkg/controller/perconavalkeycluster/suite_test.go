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

package perconavalkeycluster_test

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
	"valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeycluster"
)

// The envtest harness brings up a real apiserver+etcd with the generated CRDs
// installed. There is no kubelet/pod and no live Valkey, so the cluster
// controller's "connect to the live pod" path is driven through an injected
// scripted fake ClusterClientFactory (fakeCluster) and the ValkeyNode status is
// driven manually (the node controller is not running here). CR-18 mitigation.
var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config
	k8sClient client.Client
	testCtx   context.Context
	cancel    context.CancelFunc
	apiScheme = runtime.NewScheme()

	// mgrFC is the shared manager's injected fake cluster; the manager-backed
	// spec uses it to exercise the SetupWithManager Owns/Watches wiring E2E.
	mgrFC *fakeCluster
)

const (
	timeout  = 20 * time.Second
	interval = 200 * time.Millisecond
	// mgrNamespace isolates the shared-manager spec from the direct-Reconcile
	// specs so the two reconcile paths never race over the same objects.
	mgrNamespace = "pvk-mgr-ns"
)

func TestPerconaValkeyClusterController(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "pkg/controller/perconavalkeycluster suite")
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

	// A shared manager wired via SetupWithManager exercises the Owns/Watches +
	// registration path E2E (a ValkeyNode status flip re-enqueues the owner). Its
	// cache is scoped to mgrNamespace so it never reconciles the direct-spec
	// objects.
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme:  apiScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{mgrNamespace: {}}},
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	mgrFC = newFakeCluster()
	mr := perconavalkeycluster.NewReconcilerForTest(mgr.GetClient(), mgr.GetScheme(), &fakeClientFactory{fc: mgrFC})
	mr.RecorderForTest(mgr.GetEventRecorder("perconavalkeycluster-mgr"))
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
