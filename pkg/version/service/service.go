// Package service is the Percona version-service client (check.percona.com).
//
// In M0 this is an INTERFACE STUB only — no HTTP implementation. The concrete
// client (recommended-image resolution, upgrade-target polling) is wired in M6
// (see docs/implementation/07-phase6-upgrades-versioning.md). Defining the
// interface now lets M1+ controllers depend on the abstraction (repository
// pattern) and unit-test against a mock.
package service

import "context"

// CheckRequest carries the parameters a version-service query needs: the
// operator version and the product/engine versions in play.
type CheckRequest struct {
	OperatorVersion string
	Product         string
	Apply           string // upgrade strategy, e.g. "recommended", "latest", or a pinned version
}

// CheckResponse is the resolved set of recommended images for a request.
type CheckResponse struct {
	ValkeyImage   string
	BackupImage   string
	ExporterImage string
}

// Client resolves recommended engine/sidecar images from the Percona version
// service. The concrete implementation is provided in M6.
type Client interface {
	// Check returns the recommended images for the given request.
	Check(ctx context.Context, req CheckRequest) (*CheckResponse, error)
}
