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

package valkeynode_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

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
	"valkey.percona.com/percona-valkey-operator/pkg/controller/valkeynode"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// The envtest harness brings up a real apiserver+etcd with the generated CRDs
// installed. There is no kubelet/pod, so the controller's "connect to the live
// pod" path is exercised through an injected fake ClientFactory (see specs).
var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config
	k8sClient client.Client
	testCtx   context.Context
	cancel    context.CancelFunc
	apiScheme = runtime.NewScheme()

	// mgrFactory is the shared manager's injected fake ClientFactory. Manager-
	// driven specs program its returned ConfigClient before triggering events.
	mgrFactory *valkey.MockClientFactory
	mgrVC      *valkey.MockConfigClient
	mgrMockCtl *gomock.Controller
)

const (
	timeout  = 30 * time.Second
	interval = 250 * time.Millisecond
	// mgrNamespace isolates the shared-manager specs from the direct-Reconcile
	// specs (which use "default") so the two reconcile paths never race.
	mgrNamespace = "mgr-ns"
)

func TestValkeyNodeController(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "pkg/controller/valkeynode suite")
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

	// Namespaces: manager-driven specs use mgrNamespace (the shared manager only
	// watches it); direct-Reconcile specs use "default" so the two do not race
	// over the same objects.
	gomega.Expect(k8sClient.Create(testCtx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: mgrNamespace},
	})).To(gomega.Succeed())

	// Start a single shared manager wired with a fake factory so the Owns/Watches
	// wiring (E10) is exercised end-to-end without re-registering controllers. Its
	// cache is scoped to mgrNamespace so it never reconciles the direct-spec
	// objects in "default".
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme:  apiScheme,
		Metrics: metricsDisabled(),
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{mgrNamespace: {}}},
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	mgrMockCtl = gomock.NewController(ginkgo.GinkgoT())
	mgrFactory = valkey.NewMockClientFactory(mgrMockCtl)
	mgrVC = valkey.NewMockConfigClient(mgrMockCtl)
	mgrFactory.EXPECT().For(gomock.Any(), gomock.Any()).Return(mgrVC, nil).AnyTimes()
	mgrVC.EXPECT().InfoReplication(gomock.Any()).Return(map[string]string{valkey.InfoKeyRole: valkey.InfoRoleMaster}, nil).AnyTimes()
	mgrVC.EXPECT().ConfigSet(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mgrVC.EXPECT().Close().Return(nil).AnyTimes()

	r := valkeynode.NewReconcilerForTest(mgr.GetClient(), mgr.GetScheme(), mgrFactory)
	r.RecorderForTest(mgr.GetEventRecorder("valkeynode-mgr"))
	gomega.Expect(r.SetupWithManager(mgr)).To(gomega.Succeed())

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
	if mgrMockCtl != nil {
		mgrMockCtl.Finish()
	}
	if testEnv != nil {
		gomega.Expect(testEnv.Stop()).To(gomega.Succeed())
	}
})

// ensure ctrl import survives spec churn.
var _ = ctrl.GetConfigOrDie

// metricsDisabled returns a metrics-server option that binds nowhere, so
// parallel manager-based specs do not contend for the metrics port.
func metricsDisabled() metricsserver.Options {
	return metricsserver.Options{BindAddress: "0"}
}
