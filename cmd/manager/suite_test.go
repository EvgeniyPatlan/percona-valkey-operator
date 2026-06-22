package main

import (
	"testing"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// testEnv is the shared envtest control plane (apiserver + etcd) brought up
// once for the manager-boot smoke. KUBEBUILDER_ASSETS (set by `make test` via
// setup-envtest) points at the apiserver/etcd binaries.
var (
	testEnv *envtest.Environment
	restCfg *rest.Config
)

func TestManager(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "cmd/manager suite")
}

var _ = ginkgo.BeforeSuite(func() {
	logf.SetLogger(crzap.New(crzap.WriteTo(ginkgo.GinkgoWriter), crzap.UseDevMode(true)))

	testEnv = &envtest.Environment{}

	var err error
	restCfg, err = testEnv.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(restCfg).NotTo(gomega.BeNil())
})

var _ = ginkgo.AfterSuite(func() {
	if testEnv != nil {
		gomega.Expect(testEnv.Stop()).To(gomega.Succeed())
	}
})

// ensure ctrl is referenced even if the spec files change; harmless no-op.
var _ = ctrl.GetConfigOrDie
