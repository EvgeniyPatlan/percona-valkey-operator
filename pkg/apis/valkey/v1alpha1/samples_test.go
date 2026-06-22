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
	"os"
	"path/filepath"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// The §11 sample CRs must be apply-valid against the generated CRDs (E6). This
// spec loads each config/samples/*.yaml, strips the placeholder namespace
// (envtest only has "default") and server-dry-run applies it, proving the API
// server accepts every sample (defaults + CEL).
var _ = ginkgo.Describe("Sample CRs (03 §11)", func() {
	samplesDir := filepath.Join("..", "..", "..", "..", "config", "samples")
	samples := []string{
		"valkey_v1alpha1_perconavalkeycluster_minimal.yaml",
		"valkey_v1alpha1_perconavalkeycluster.yaml",
		"valkey_v1alpha1_perconavalkeybackup.yaml",
		"valkey_v1alpha1_perconavalkeyrestore.yaml",
	}
	for _, name := range samples {
		ginkgo.It("server-dry-run accepts "+name, func() {
			data, err := os.ReadFile(filepath.Join(samplesDir, name))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			obj := &unstructured.Unstructured{}
			gomega.Expect(yaml.Unmarshal(data, &obj.Object)).To(gomega.Succeed())
			// The samples use namespace "valkey"; envtest only has "default".
			obj.SetNamespace("default")

			err = k8s.Create(ctx, obj, client.DryRunAll)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "sample %s should be apply-valid", name)
		})
	}
})
