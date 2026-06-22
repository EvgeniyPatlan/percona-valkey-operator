// Package service is the Percona version-service client (check.percona.com).
//
// It resolves the recommended/latest engine + exporter + backup image triple for
// an operator/crVersion against a Percona-style version service over HTTP. The
// abstraction (RecommendedImageResolver, types.go) lets the controller depend on
// the interface (repository pattern) and unit-test against a fake; the concrete
// HTTPResolver in this file POSTs the cluster coordinates and parses the response.
//
// Dependency rule: this stays a std+http near-leaf — it imports NO pkg/apis. The
// caller in pkg/controller projects the CR into a VSRequest, avoiding a
// pkg/apis -> pkg/version -> pkg/apis cycle (09 §3, GO-6.4).
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultEndpoint is the Percona-hosted version service. It is overridden per
// cluster by spec.upgradeOptions.versionServiceEndpoint for air-gapped/private
// mirrors (09 §3).
const DefaultEndpoint = "https://check.percona.com"

// defaultTimeout bounds a single version-service round-trip. The poll runs at
// most once per schedule window, so a short bound keeps a stuck endpoint from
// holding the reconcile; on timeout the poll skips cleanly and retries next
// window (09 §3, E6). It is NOT a retry budget — there are no in-window retries.
const defaultTimeout = 10 * time.Second

// versionPath is the resource the recommendation is fetched from. It is appended
// to the (possibly user-overridden) endpoint base.
const versionPath = "/versions/v1/valkey-operator"

// ErrUnreachable classifies a transport-level failure (DNS, dial, TLS, timeout):
// the endpoint could not be reached at all. It is distinct from ErrServiceError
// so the caller can skip cleanly on a transient outage without degrading (E6).
var ErrUnreachable = errors.New("version service unreachable")

// ErrServiceError classifies a reachable-but-erroring service: a non-2xx status
// or a malformed/empty body. The endpoint answered, but the answer is unusable.
var ErrServiceError = errors.New("version service error")

// CheckRequest carries the parameters a legacy (M0) version-service query needs.
// Retained for the pre-existing Client interface; the M6 path uses VSRequest.
type CheckRequest struct {
	OperatorVersion string
	Product         string
	Apply           string // upgrade strategy, e.g. "recommended", "latest", or a pinned version
}

// CheckResponse is the resolved set of recommended images for a CheckRequest.
type CheckResponse struct {
	ValkeyImage   string
	BackupImage   string
	ExporterImage string
}

// Client resolves recommended engine/sidecar images from the Percona version
// service (the pre-existing M0 interface). HTTPResolver also satisfies it by
// adapting Check onto Resolve, so existing M0 callers keep working.
type Client interface {
	// Check returns the recommended images for the given request.
	Check(ctx context.Context, req CheckRequest) (*CheckResponse, error)
}

// HTTPResolver is the concrete RecommendedImageResolver: it POSTs the cluster
// coordinates to the version service and parses the recommended/latest triple.
// The zero value is not usable; build it with NewHTTPResolver.
type HTTPResolver struct {
	endpoint string
	client   *http.Client
}

// Option configures an HTTPResolver (functional-options pattern).
type Option func(*HTTPResolver)

// WithHTTPClient injects a custom *http.Client (tests point it at an httptest
// server / inject a transport; production uses the bounded-timeout default).
func WithHTTPClient(c *http.Client) Option {
	return func(r *HTTPResolver) {
		if c != nil {
			r.client = c
		}
	}
}

// WithTimeout overrides the per-request timeout on the default client.
func WithTimeout(d time.Duration) Option {
	return func(r *HTTPResolver) {
		if d > 0 {
			r.client = &http.Client{Timeout: d}
		}
	}
}

// NewHTTPResolver builds an HTTPResolver for the given endpoint. An empty
// endpoint falls back to DefaultEndpoint. The default HTTP client carries a
// bounded timeout (defaultTimeout) so a stuck endpoint cannot wedge a reconcile.
func NewHTTPResolver(endpoint string, opts ...Option) *HTTPResolver {
	r := &HTTPResolver{
		endpoint: normalizeEndpoint(endpoint),
		client:   &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// normalizeEndpoint trims trailing slashes and falls back to DefaultEndpoint when
// empty, so endpoint + versionPath always joins to a single, well-formed URL.
func normalizeEndpoint(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return DefaultEndpoint
	}
	return strings.TrimRight(e, "/")
}

// wireRequest is the on-the-wire JSON body. It is kept private so the public
// VSRequest stays a pure primitives struct decoupled from the HTTP contract; if
// the real endpoint's field names drift (OQ-6.A) only this mapping changes.
type wireRequest struct {
	Product           string `json:"product"`
	OperatorVersion   string `json:"operatorVersion"`
	CrVersion         string `json:"crVersion,omitempty"`
	CurrentEngine     string `json:"currentEngine,omitempty"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	Platform          string `json:"platform,omitempty"`
	Apply             string `json:"apply,omitempty"`
}

// wireVersionSet mirrors VersionSet on the wire.
type wireVersionSet struct {
	Engine   string `json:"engine"`
	Exporter string `json:"exporter"`
	Backup   string `json:"backup"`
}

// wireResponse is the on-the-wire JSON response.
type wireResponse struct {
	Recommended wireVersionSet `json:"recommended"`
	Latest      wireVersionSet `json:"latest"`
}

// Resolve POSTs the cluster coordinates to the version service and returns the
// recommended/latest image triples. Transport failures wrap ErrUnreachable;
// non-2xx or malformed/empty bodies wrap ErrServiceError. It never panics and
// never retries inside a window (09 §3, E6).
func (r *HTTPResolver) Resolve(ctx context.Context, req VSRequest) (*VSResponse, error) {
	if req.OperatorVersion == "" {
		return nil, fmt.Errorf("%w: empty operatorVersion in request", ErrServiceError)
	}
	body, err := json.Marshal(toWireRequest(req))
	if err != nil {
		// Marshalling primitives cannot realistically fail; classify defensively.
		return nil, fmt.Errorf("%w: marshal request: %v", ErrServiceError, err)
	}

	url := r.endpoint + versionPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUnreachable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(httpReq)
	if err != nil {
		// DNS/dial/TLS/timeout/context-cancel: the endpoint was not reached.
		return nil, fmt.Errorf("%w: POST %s: %v", ErrUnreachable, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	return parseResponse(resp)
}

// Check adapts the pre-existing M0 Client interface onto Resolve so M0 callers
// keep compiling. It maps Apply onto a VSRequest and projects the recommended
// triple (Latest is reachable via Resolve for callers that need it).
func (r *HTTPResolver) Check(ctx context.Context, req CheckRequest) (*CheckResponse, error) {
	resp, err := r.Resolve(ctx, VSRequest{
		Product:         req.Product,
		OperatorVersion: req.OperatorVersion,
		Apply:           req.Apply,
	})
	if err != nil {
		return nil, err
	}
	set := resp.Recommended
	if strings.EqualFold(req.Apply, "latest") {
		set = resp.Latest
	}
	return &CheckResponse{
		ValkeyImage:   set.Engine,
		BackupImage:   set.Backup,
		ExporterImage: set.Exporter,
	}, nil
}

// parseResponse validates the HTTP status and decodes a non-empty, well-formed
// body into a VSResponse. A non-2xx status or a body that decodes but carries no
// engine image at all is an ErrServiceError (the service answered uselessly).
func parseResponse(resp *http.Response) (*VSResponse, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bounded prefix for context without trusting the body.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: status %d: %s", ErrServiceError, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // bound the body at 1 MiB
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrServiceError, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: empty response body", ErrServiceError)
	}

	var wr wireResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // strict: reject a contract that drifted unexpectedly
	if err := dec.Decode(&wr); err != nil {
		// Retry leniently so an additive (forward-compatible) field does not break
		// us, but still classify a truly malformed body as a service error.
		if lenientErr := json.Unmarshal(raw, &wr); lenientErr != nil {
			return nil, fmt.Errorf("%w: decode body: %v", ErrServiceError, lenientErr)
		}
	}

	out := &VSResponse{
		Recommended: VersionSet{Engine: wr.Recommended.Engine, Exporter: wr.Recommended.Exporter, Backup: wr.Recommended.Backup},
		Latest:      VersionSet{Engine: wr.Latest.Engine, Exporter: wr.Latest.Exporter, Backup: wr.Latest.Backup},
	}
	if out.Recommended.Engine == "" && out.Latest.Engine == "" {
		return nil, fmt.Errorf("%w: response carried no engine image", ErrServiceError)
	}
	return out, nil
}

// toWireRequest maps the public primitives VSRequest onto the wire body. The two
// structs are field-identical (only json tags differ), so a direct conversion
// keeps the mapping in lockstep automatically; if the wire contract drifts
// (OQ-6.A) the structs diverge and the conversion stops compiling — a loud signal.
func toWireRequest(req VSRequest) wireRequest {
	return wireRequest(req)
}

// Compile-time assertions: HTTPResolver satisfies both the new resolver seam and
// the pre-existing M0 Client interface.
var (
	_ RecommendedImageResolver = (*HTTPResolver)(nil)
	_ Client                   = (*HTTPResolver)(nil)
)
