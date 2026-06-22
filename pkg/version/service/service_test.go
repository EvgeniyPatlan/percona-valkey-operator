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

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"valkey.percona.com/percona-valkey-operator/pkg/version/service"
)

// goldenBody is a representative version-service response: the recommended and
// latest engine/exporter/backup triples validated together (09 §3).
const goldenBody = `{
  "recommended": {
    "engine":   "percona/percona-valkey:9.0.1-1",
    "exporter": "percona/valkey-exporter:1.2.0",
    "backup":   "percona/valkey-backup:9.0.1-1"
  },
  "latest": {
    "engine":   "percona/percona-valkey:9.2.0-1",
    "exporter": "percona/valkey-exporter:1.3.0",
    "backup":   "percona/valkey-backup:9.2.0-1"
  }
}`

func newResolver(srv *httptest.Server) *service.HTTPResolver {
	return service.NewHTTPResolver(srv.URL, service.WithHTTPClient(srv.Client()))
}

func TestResolveGoldenResponse(t *testing.T) {
	var gotBody service.VSRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		raw, _ := io.ReadAll(r.Body)
		// The body is the private wire shape; decode loosely to confirm the
		// coordinates were carried through.
		var wire map[string]string
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Errorf("server could not decode request body: %v", err)
		}
		gotBody = service.VSRequest{
			Product:         wire["product"],
			OperatorVersion: wire["operatorVersion"],
			CrVersion:       wire["crVersion"],
			CurrentEngine:   wire["currentEngine"],
			Apply:           wire["apply"],
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, goldenBody)
	}))
	defer srv.Close()

	req := service.VSRequest{
		Product:         "valkey-operator",
		OperatorVersion: "1.0.0",
		CrVersion:       "1.0",
		CurrentEngine:   "8.0.2",
		Apply:           "Recommended",
	}
	resp, err := newResolver(srv).Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if gotBody.Product != "valkey-operator" || gotBody.OperatorVersion != "1.0.0" {
		t.Errorf("server saw request %+v, coordinates not propagated", gotBody)
	}
	if gotBody.CrVersion != "1.0" || gotBody.CurrentEngine != "8.0.2" || gotBody.Apply != "Recommended" {
		t.Errorf("server saw request %+v, optional coordinates not propagated", gotBody)
	}

	wantRec := service.VersionSet{
		Engine:   "percona/percona-valkey:9.0.1-1",
		Exporter: "percona/valkey-exporter:1.2.0",
		Backup:   "percona/valkey-backup:9.0.1-1",
	}
	if resp.Recommended != wantRec {
		t.Errorf("Recommended = %+v, want %+v", resp.Recommended, wantRec)
	}
	wantLatest := service.VersionSet{
		Engine:   "percona/percona-valkey:9.2.0-1",
		Exporter: "percona/valkey-exporter:1.3.0",
		Backup:   "percona/valkey-backup:9.2.0-1",
	}
	if resp.Latest != wantLatest {
		t.Errorf("Latest = %+v, want %+v", resp.Latest, wantLatest)
	}
}

func TestResolveLiteralResponse(t *testing.T) {
	// A literal apply resolves the exact build tag into Recommended (09 §3).
	const body = `{"recommended":{"engine":"percona/percona-valkey:9.0.1-3","exporter":"e:1","backup":"b:1"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	resp, err := newResolver(srv).Resolve(context.Background(),
		service.VSRequest{OperatorVersion: "1.0.0", Apply: "9.0.1"})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if resp.Recommended.Engine != "percona/percona-valkey:9.0.1-3" {
		t.Errorf("Recommended.Engine = %q, want the resolved literal tag", resp.Recommended.Engine)
	}
}

func TestResolveServiceDownReturnsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // close before the call so the dial fails: the endpoint is down.

	_, err := service.NewHTTPResolver(url).Resolve(context.Background(),
		service.VSRequest{OperatorVersion: "1.0.0", Apply: "Recommended"})
	if !errors.Is(err, service.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestResolveContextTimeoutIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, goldenBody)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := newResolver(srv).Resolve(ctx, service.VSRequest{OperatorVersion: "1.0.0", Apply: "Latest"})
	if !errors.Is(err, service.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable on context timeout", err)
	}
}

func TestResolveNon2xxIsServiceError(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, "upstream boom")
		}))
		_, err := newResolver(srv).Resolve(context.Background(),
			service.VSRequest{OperatorVersion: "1.0.0", Apply: "Recommended"})
		srv.Close()
		if !errors.Is(err, service.ErrServiceError) {
			t.Errorf("status %d: err = %v, want ErrServiceError", code, err)
		}
	}
}

func TestResolveMalformedBodyIsServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{ this is not json")
	}))
	defer srv.Close()

	_, err := newResolver(srv).Resolve(context.Background(),
		service.VSRequest{OperatorVersion: "1.0.0", Apply: "Recommended"})
	if !errors.Is(err, service.ErrServiceError) {
		t.Fatalf("err = %v, want ErrServiceError on malformed body", err)
	}
}

func TestResolveEmptyBodyIsServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	_, err := newResolver(srv).Resolve(context.Background(),
		service.VSRequest{OperatorVersion: "1.0.0", Apply: "Recommended"})
	if !errors.Is(err, service.ErrServiceError) {
		t.Fatalf("err = %v, want ErrServiceError on empty body", err)
	}
}

func TestResolveNoEngineIsServiceError(t *testing.T) {
	// A well-formed body that carries no engine at all is unusable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"recommended":{"exporter":"e:1","backup":"b:1"}}`)
	}))
	defer srv.Close()

	_, err := newResolver(srv).Resolve(context.Background(),
		service.VSRequest{OperatorVersion: "1.0.0", Apply: "Recommended"})
	if !errors.Is(err, service.ErrServiceError) {
		t.Fatalf("err = %v, want ErrServiceError when no engine present", err)
	}
}

func TestResolveEmptyOperatorVersionRejected(t *testing.T) {
	// Guard the request before any network call.
	_, err := service.NewHTTPResolver("http://127.0.0.1:0").Resolve(context.Background(), service.VSRequest{})
	if !errors.Is(err, service.ErrServiceError) {
		t.Fatalf("err = %v, want ErrServiceError on empty operatorVersion", err)
	}
}

func TestResolveForwardCompatibleUnknownFields(t *testing.T) {
	// An additive (unknown) field must not break parsing — forward compatibility.
	const body = `{"recommended":{"engine":"x:1","exporter":"e:1","backup":"b:1","newField":"ignored"},"meta":{"k":"v"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	resp, err := newResolver(srv).Resolve(context.Background(),
		service.VSRequest{OperatorVersion: "1.0.0", Apply: "Recommended"})
	if err != nil {
		t.Fatalf("Resolve returned error on forward-compatible body: %v", err)
	}
	if resp.Recommended.Engine != "x:1" {
		t.Errorf("Recommended.Engine = %q, want x:1", resp.Recommended.Engine)
	}
}

func TestCheckAdaptsOntoResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, goldenBody)
	}))
	defer srv.Close()

	res := newResolver(srv)
	rec, err := res.Check(context.Background(), service.CheckRequest{
		Product:         "valkey-operator",
		OperatorVersion: "1.0.0",
		Apply:           "recommended",
	})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rec.ValkeyImage != "percona/percona-valkey:9.0.1-1" || rec.BackupImage != "percona/valkey-backup:9.0.1-1" {
		t.Errorf("Check(recommended) = %+v, want recommended triple", rec)
	}

	latest, err := res.Check(context.Background(), service.CheckRequest{OperatorVersion: "1.0.0", Apply: "latest"})
	if err != nil {
		t.Fatalf("Check(latest) returned error: %v", err)
	}
	if latest.ValkeyImage != "percona/percona-valkey:9.2.0-1" {
		t.Errorf("Check(latest).ValkeyImage = %q, want latest engine", latest.ValkeyImage)
	}
}

func TestNewHTTPResolverEmptyEndpointFallsBack(t *testing.T) {
	// An empty endpoint must fall back to the default (no panic, no empty URL).
	res := service.NewHTTPResolver("", service.WithTimeout(time.Second))
	// The dial will fail (we are not contacting the real service), but it must
	// classify as unreachable, proving a well-formed URL was built.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := res.Resolve(ctx, service.VSRequest{OperatorVersion: "1.0.0", Apply: "Recommended"})
	if err == nil || !strings.Contains(err.Error(), "check.percona.com") {
		t.Fatalf("err = %v, want a default-endpoint (check.percona.com) error", err)
	}
}
