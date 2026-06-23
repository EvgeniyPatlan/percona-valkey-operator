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

package service

import "context"

// The M6 version-service contract (doc 09 §3, impl 07 §8.3 / GO-6.4). These types
// are the SEAM the version-service leg (GO-6.4) fills with an HTTP implementation.
// They are deliberately PRIMITIVES-ONLY (no pkg/apis import) so pkg/version stays
// the std+http near-leaf established in M1: the caller in pkg/controller projects
// the CR into a VSRequest, avoiding a pkg/apis -> pkg/version -> pkg/apis cycle.

// VersionSet is the mutually-compatible engine/exporter/backup image triple the
// version service validated TOGETHER (09 §3 multi-image-compatibility guarantee).
// All three move as a unit so a recommended engine never pairs with an
// incompatible exporter or backup tool.
type VersionSet struct {
	// Engine is the fully-qualified Valkey server image tag (e.g.
	// percona/valkey:9.0.1-1).
	Engine string
	// Exporter is the metrics-exporter image tag validated with Engine.
	Exporter string
	// Backup is the backup-tool image tag validated with Engine.
	Backup string
}

// VSRequest carries the cluster coordinates the version service needs to resolve a
// recommendation: the operator product + version/crVersion, the engine currently
// running, and the platform/k8s coordinates. All fields are primitives so the
// request never depends on pkg/apis (leaf-rule, GO-6.4).
type VSRequest struct {
	// Product is the version-service product key (e.g. "valkey-operator").
	Product string
	// OperatorVersion is the operator semver from pkg/version (version.Version()).
	OperatorVersion string
	// CrVersion is the CR's spec.crVersion (major.minor), the contract the
	// recommendation is validated against.
	CrVersion string
	// CurrentEngine is the engine version currently pinned in spec.image, so the
	// service can compute a safe forward target.
	CurrentEngine string
	// KubernetesVersion is the detected cluster k8s version (may be empty).
	KubernetesVersion string
	// Platform is the detected platform ("kubernetes"/"openshift"/...; may be empty).
	Platform string
	// Apply is the resolved upgradeOptions.apply policy ("Recommended"/"Latest"/
	// a literal version). The literal case lets the service resolve the exact build
	// tag for a user-pinned engine version.
	Apply string
}

// VSResponse is the recommended and latest VersionSet the service returns for a
// VSRequest. The caller picks Recommended or Latest per the apply policy (GO-6.5),
// or, for a literal apply, the service resolves the exact tag into Recommended.
type VSResponse struct {
	// Recommended is the Percona-validated, CVE-patched engine triple for this
	// operator/crVersion (the production managed-update target).
	Recommended VersionSet
	// Latest is the newest engine triple the service offers for this operator
	// (dev/staging target; not recommended for production).
	Latest VersionSet
}

// RecommendedImageResolver is the abstraction the version-check leg (GO-6.5/6.6)
// depends on (repository pattern): the controller holds this interface, the
// version-service leg (GO-6.4) supplies the concrete HTTP implementation, and
// tests inject a fake. Resolve returns typed errors distinguishing a reachable-
// but-erroring service from an unreachable endpoint so the once-per-window poll
// can skip cleanly on a transient outage (09 §3, E6) without rolling or degrading.
//
// SEAM: the HTTP body, request marshalling, response parsing, bounded timeout, and
// typed unreachable-vs-error classification are filled by GO-6.4. This pass ships
// the interface + types only.
type RecommendedImageResolver interface {
	// Resolve POSTs the cluster coordinates to the version service and returns the
	// recommended/latest image triples, or a typed error.
	Resolve(ctx context.Context, req VSRequest) (*VSResponse, error)
}
