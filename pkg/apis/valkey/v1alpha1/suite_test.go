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

package v1alpha1_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// testEnv brings up a real apiserver+etcd with the generated CRDs installed so
// the CEL/immutability rules and marker defaults can be exercised exactly as the
// API server enforces them (no operator running). The CRD YAMLs are the
// controller-gen output under config/crd/bases (the same artifacts that ship in
// deploy/crd.yaml).
var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config
	k8s       client.Client
	ctx       context.Context
	cancel    context.CancelFunc
	apiScheme = runtime.NewScheme()
)

const (
	timeout  = 30 * time.Second
	interval = 250 * time.Millisecond
)

func TestAPIs(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "pkg/apis/valkey/v1alpha1 suite")
}

var _ = ginkgo.BeforeSuite(func() {
	logf.SetLogger(crzap.New(crzap.WriteTo(ginkgo.GinkgoWriter), crzap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	restCfg, err = testEnv.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(restCfg).NotTo(gomega.BeNil())

	gomega.Expect(valkeyv1alpha1.AddToScheme(apiScheme)).To(gomega.Succeed())
	gomega.Expect(apiextv1.AddToScheme(apiScheme)).To(gomega.Succeed())

	k8s, err = client.New(restCfg, client.Options{Scheme: apiScheme})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(k8s).NotTo(gomega.BeNil())
})

var _ = ginkgo.AfterSuite(func() {
	if cancel != nil {
		cancel()
	}
	if testEnv != nil {
		gomega.Expect(testEnv.Stop()).To(gomega.Succeed())
	}
})

// ensure ctrl is referenced; harmless no-op so the import survives spec churn.
var _ = ctrl.GetConfigOrDie
