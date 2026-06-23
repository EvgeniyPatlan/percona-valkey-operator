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

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDefaultedTrueBoolSerializesFalse guards the omitempty + +kubebuilder:default=true
// footgun. For a non-pointer bool defaulted to true, omitempty would DROP an explicit
// false on marshal; when the operator then creates/updates a child object (e.g. a
// ValkeyNode whose spec.exporter was propagated from a cluster with the exporter
// disabled), the API server sees the field as absent and RE-APPLIES the true default —
// silently re-enabling it. These fields therefore must NOT carry omitempty so a false
// always reaches the wire. Confirmed live: a ValkeyNode submitted with exporter.enabled
// omitted comes back enabled=true.
func TestDefaultedTrueBoolSerializesFalse(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"ExporterSpec.Enabled", ExporterSpec{Enabled: false}},
		{"UserACLSpec.Enabled", UserACLSpec{Name: "appuser", Enabled: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(b), `"enabled":false`) {
				t.Errorf("%s must serialize an explicit \"enabled\":false (omitempty would drop it and the "+
					"API server would re-default it to true); got %s", tc.name, b)
			}
		})
	}
}
