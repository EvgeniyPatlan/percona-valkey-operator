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

package perconavalkeycluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/version/service"
)

// ---- test doubles -----------------------------------------------------------

// fakeResolver is an injectable RecommendedImageResolver for the version-check
// unit tests. It records the request it saw and returns a scripted response/err.
type fakeResolver struct {
	resp     *service.VSResponse
	err      error
	calls    int
	lastReq  service.VSRequest
	endpoint string
}

func (f *fakeResolver) Resolve(_ context.Context, req service.VSRequest) (*service.VSResponse, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// installResolver swaps the package resolverFactory to return fr and restores the
// previous factory + clock on cleanup, isolating each test's registry/clock.
func installResolver(t *testing.T, fr *fakeResolver) {
	t.Helper()
	prevFactory := resolverFactory
	prevClock := versionCheckClock
	resolverFactory = func(endpoint string) service.RecommendedImageResolver {
		fr.endpoint = endpoint
		return fr
	}
	t.Cleanup(func() {
		resolverFactory = prevFactory
		versionCheckClock = prevClock
	})
}

func vcTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := valkeyv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add valkey scheme: %v", err)
	}
	return s
}

// vcCluster builds a cluster CR with the given apply policy and image, seeded
// with a default daily schedule (as CheckNSetDefaults would).
func vcCluster(apply, image string) *valkeyv1alpha1.PerconaValkeyCluster {
	c := &valkeyv1alpha1.PerconaValkeyCluster{}
	c.Name, c.Namespace = "vc", "default"
	c.Spec.Image = image
	c.Spec.UpgradeOptions.Apply = apply
	if apply != valkeyv1alpha1.UpgradeApplyDisabled && apply != "" {
		c.Spec.UpgradeOptions.Schedule = "0 4 * * *"
		c.Spec.UpgradeOptions.VersionServiceEndpoint = "https://vs.example"
	}
	return c
}

func vcReconciler(t *testing.T, cluster *valkeyv1alpha1.PerconaValkeyCluster) (*Reconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	s := vcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).Build()
	rec := events.NewFakeRecorder(200)
	return &Reconciler{Client: c, scheme: s, recorder: rec}, c, rec
}

func recommended(engine, exporter, backup string) *service.VSResponse {
	return &service.VSResponse{
		Recommended: service.VersionSet{Engine: engine, Exporter: exporter, Backup: backup},
		Latest:      service.VersionSet{Engine: "percona/percona-valkey:9.9.9-1"},
	}
}

// resetRegistry clears the singleton registry entry for a cluster so cross-test
// state never leaks (the registry is process-global by design).
func resetRegistry(cluster *valkeyv1alpha1.PerconaValkeyCluster) {
	versionChecks.remove(vcKey(cluster))
}

// ---- applyPolicy table ------------------------------------------------------

func TestApplyPolicyMapping(t *testing.T) {
	cases := []struct {
		apply string
		want  versionCheckAction
	}{
		{"", actionDisabled},
		{valkeyv1alpha1.UpgradeApplyDisabled, actionDisabled},
		{valkeyv1alpha1.UpgradeApplyRecommended, actionRecommended},
		{valkeyv1alpha1.UpgradeApplyLatest, actionLatest},
		{"9.0.1", actionLiteral},
		{"  Recommended  ", actionRecommended}, // trimmed
	}
	for _, tc := range cases {
		c := &valkeyv1alpha1.PerconaValkeyCluster{}
		c.Spec.UpgradeOptions.Apply = tc.apply
		if got := applyPolicy(c); got != tc.want {
			t.Errorf("applyPolicy(%q) = %v, want %v", tc.apply, got, tc.want)
		}
	}
}

// ---- Disabled = no-op, zero client calls ------------------------------------

func TestVersionCheckDisabledMakesNoCall(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyDisabled, "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended("percona/percona-valkey:9.0.1-1", "", "")}
	installResolver(t, fr)
	r, c, _ := vcReconciler(t, cluster)

	if err := r.reconcileVersionCheck(context.Background(), cluster); err != nil {
		t.Fatalf("reconcileVersionCheck returned error: %v", err)
	}
	if fr.calls != 0 {
		t.Errorf("Disabled made %d client calls, want 0", fr.calls)
	}
	// spec.image unchanged on the live object and in storage.
	if cluster.Spec.Image != "percona/percona-valkey:8.0.1" {
		t.Errorf("Disabled mutated spec.image to %q", cluster.Spec.Image)
	}
	got := &valkeyv1alpha1.PerconaValkeyCluster{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cluster), got)
	if got.Spec.Image != "percona/percona-valkey:8.0.1" {
		t.Errorf("Disabled persisted image %q, want unchanged", got.Spec.Image)
	}
}

// ---- Recommended polls on window and mutates only-if-differs -----------------

func TestVersionCheckRecommendedMutatesImage(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyRecommended, "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended(
		"percona/percona-valkey:9.0.1-1", "percona/valkey-exporter:1.2.0", "percona/valkey-backup:9.0.1-1")}
	installResolver(t, fr)
	r, c, rec := vcReconciler(t, cluster)

	base := time.Date(2026, 6, 22, 3, 0, 0, 0, time.UTC) // before the 04:00 slot
	versionCheckClock = func() time.Time { return base }
	// First pass registers the window, anchored at base; no slot elapsed yet.
	if err := r.reconcileVersionCheck(context.Background(), cluster); err != nil {
		t.Fatalf("first pass error: %v", err)
	}
	if fr.calls != 0 {
		t.Fatalf("polled before window elapsed (%d calls)", fr.calls)
	}

	// Advance past the 04:00 slot: the window is now due.
	versionCheckClock = func() time.Time { return base.Add(2 * time.Hour) }
	if err := r.reconcileVersionCheck(context.Background(), cluster); err != nil {
		t.Fatalf("windowed pass error: %v", err)
	}
	if fr.calls != 1 {
		t.Fatalf("polled %d times, want exactly 1", fr.calls)
	}

	// The air-gapped endpoint override reached the factory.
	if fr.endpoint != "https://vs.example" {
		t.Errorf("resolver built with endpoint %q, want the spec override", fr.endpoint)
	}
	// Request coordinates projected from the CR.
	if fr.lastReq.Product != versionCheckProduct() || fr.lastReq.CurrentEngine != "8.0.1" || fr.lastReq.Apply != "Recommended" {
		t.Errorf("vsRequest = %+v, coordinates wrong", fr.lastReq)
	}

	// All three pins moved together (live object + storage).
	if cluster.Spec.Image != "percona/percona-valkey:9.0.1-1" {
		t.Errorf("live spec.image = %q, want recommended engine", cluster.Spec.Image)
	}
	if cluster.Spec.Exporter.Image != "percona/valkey-exporter:1.2.0" || cluster.Spec.Backup.Image != "percona/valkey-backup:9.0.1-1" {
		t.Errorf("exporter/backup not updated: %q / %q", cluster.Spec.Exporter.Image, cluster.Spec.Backup.Image)
	}
	got := &valkeyv1alpha1.PerconaValkeyCluster{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cluster), got); err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if got.Spec.Image != "percona/percona-valkey:9.0.1-1" {
		t.Errorf("persisted spec.image = %q, want recommended engine", got.Spec.Image)
	}
	assertEvent(t, rec, ReasonNewEnginePinResolved)
}

// ---- Latest selects the Latest set ------------------------------------------

func TestVersionCheckLatestUsesLatestSet(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyLatest, "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	resp := &service.VSResponse{
		Recommended: service.VersionSet{Engine: "percona/percona-valkey:9.0.1-1"},
		Latest:      service.VersionSet{Engine: "percona/percona-valkey:9.2.0-1"},
	}
	fr := &fakeResolver{resp: resp}
	installResolver(t, fr)
	r, _, _ := vcReconciler(t, cluster)

	base := time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC)
	versionCheckClock = func() time.Time { return base }
	_ = r.reconcileVersionCheck(context.Background(), cluster) // register window
	versionCheckClock = func() time.Time { return base.Add(24 * time.Hour) }
	if err := r.reconcileVersionCheck(context.Background(), cluster); err != nil {
		t.Fatalf("error: %v", err)
	}
	if cluster.Spec.Image != "percona/percona-valkey:9.2.0-1" {
		t.Errorf("Latest used %q, want the latest engine", cluster.Spec.Image)
	}
}

// ---- literal version resolves through Recommended ---------------------------

func TestVersionCheckLiteralResolvesViaRecommended(t *testing.T) {
	cluster := vcCluster("9.0.1", "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended("percona/percona-valkey:9.0.1-7", "", "")}
	installResolver(t, fr)
	r, _, _ := vcReconciler(t, cluster)

	base := time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC)
	versionCheckClock = func() time.Time { return base }
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	versionCheckClock = func() time.Time { return base.Add(24 * time.Hour) }
	if err := r.reconcileVersionCheck(context.Background(), cluster); err != nil {
		t.Fatalf("error: %v", err)
	}
	if fr.lastReq.Apply != "9.0.1" {
		t.Errorf("literal apply not propagated: %q", fr.lastReq.Apply)
	}
	if cluster.Spec.Image != "percona/percona-valkey:9.0.1-7" {
		t.Errorf("literal resolved %q, want the exact build tag", cluster.Spec.Image)
	}
}

// ---- same pin = no mutation, no event ---------------------------------------

func TestVersionCheckSamePinIsNoop(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyRecommended, "percona/percona-valkey:9.0.1-1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended("percona/percona-valkey:9.0.1-1", "", "")}
	installResolver(t, fr)
	r, _, rec := vcReconciler(t, cluster)

	base := time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC)
	versionCheckClock = func() time.Time { return base }
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	versionCheckClock = func() time.Time { return base.Add(24 * time.Hour) }
	if err := r.reconcileVersionCheck(context.Background(), cluster); err != nil {
		t.Fatalf("error: %v", err)
	}
	if cluster.Spec.Image != "percona/percona-valkey:9.0.1-1" {
		t.Errorf("same pin changed image to %q", cluster.Spec.Image)
	}
	if hasEvent(rec, ReasonNewEnginePinResolved) {
		t.Errorf("same pin emitted NewEnginePinResolved event")
	}
}

// ---- service down: skip, no degrade, VersionCheckFailed event ---------------

func TestVersionCheckServiceDownIsTolerated(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyRecommended, "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{err: errors.New("boom: " + service.ErrUnreachable.Error())}
	installResolver(t, fr)
	r, _, rec := vcReconciler(t, cluster)

	base := time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC)
	versionCheckClock = func() time.Time { return base }
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	versionCheckClock = func() time.Time { return base.Add(24 * time.Hour) }
	if err := r.reconcileVersionCheck(context.Background(), cluster); err != nil {
		t.Fatalf("a poll failure must NOT return an error (E6): %v", err)
	}
	if cluster.Spec.Image != "percona/percona-valkey:8.0.1" {
		t.Errorf("poll failure mutated image to %q, want unchanged", cluster.Spec.Image)
	}
	assertEvent(t, rec, ReasonVersionCheckFailed)
}

// ---- no usable engine returned ----------------------------------------------

func TestVersionCheckEmptyEngineSkips(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyRecommended, "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended("", "x", "y")} // exporter/backup but no engine
	installResolver(t, fr)
	r, _, rec := vcReconciler(t, cluster)

	base := time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC)
	versionCheckClock = func() time.Time { return base }
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	versionCheckClock = func() time.Time { return base.Add(24 * time.Hour) }
	if err := r.reconcileVersionCheck(context.Background(), cluster); err != nil {
		t.Fatalf("error: %v", err)
	}
	if cluster.Spec.Image != "percona/percona-valkey:8.0.1" {
		t.Errorf("empty engine mutated image to %q", cluster.Spec.Image)
	}
	assertEvent(t, rec, ReasonVersionCheckFailed)
}

// ---- once-per-window cadence ------------------------------------------------

func TestVersionCheckPollsOncePerWindow(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyRecommended, "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended("percona/percona-valkey:8.0.1", "", "")} // same pin to keep image stable
	installResolver(t, fr)
	r, _, _ := vcReconciler(t, cluster)

	base := time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC)
	versionCheckClock = func() time.Time { return base }
	_ = r.reconcileVersionCheck(context.Background(), cluster) // register

	// Many reconciles within the same day → at most ONE poll (the 04:00 next-day slot
	// has not elapsed; next slot after base 05:00 is the following 04:00).
	for i := 0; i < 5; i++ {
		versionCheckClock = func() time.Time { return base.Add(time.Duration(i) * time.Hour) }
		_ = r.reconcileVersionCheck(context.Background(), cluster)
	}
	if fr.calls != 0 {
		t.Fatalf("polled %d times within one window, want 0", fr.calls)
	}

	// Cross the next 04:00 slot → exactly one poll.
	versionCheckClock = func() time.Time { return base.Add(24 * time.Hour) }
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	versionCheckClock = func() time.Time { return base.Add(25 * time.Hour) }
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	if fr.calls != 1 {
		t.Fatalf("polled %d times across one elapsed window, want exactly 1", fr.calls)
	}
}

// ---- Disabled / delete tears the window down --------------------------------

func TestVersionCheckDisableRemovesWindow(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyRecommended, "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended("percona/percona-valkey:8.0.1", "", "")}
	installResolver(t, fr)
	r, _, _ := vcReconciler(t, cluster)

	versionCheckClock = func() time.Time { return time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC) }
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	if !windowExists(cluster) {
		t.Fatalf("Recommended did not register a window")
	}

	cluster.Spec.UpgradeOptions.Apply = valkeyv1alpha1.UpgradeApplyDisabled
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	if windowExists(cluster) {
		t.Errorf("Disabled did not remove the window")
	}
}

func TestVersionCheckDeletionRemovesWindow(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyRecommended, "percona/percona-valkey:8.0.1")
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended("percona/percona-valkey:8.0.1", "", "")}
	installResolver(t, fr)
	r, _, _ := vcReconciler(t, cluster)

	versionCheckClock = func() time.Time { return time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC) }
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	if !windowExists(cluster) {
		t.Fatalf("window not registered")
	}
	now := metav1.Now()
	cluster.DeletionTimestamp = &now
	cluster.Finalizers = []string{"x"} // a DeletionTimestamp requires a finalizer to be valid
	_ = r.reconcileVersionCheck(context.Background(), cluster)
	if windowExists(cluster) {
		t.Errorf("deletion did not remove the window")
	}
}

// ---- invalid cron is a loud error -------------------------------------------

func TestVersionCheckInvalidCronErrors(t *testing.T) {
	cluster := vcCluster(valkeyv1alpha1.UpgradeApplyRecommended, "percona/percona-valkey:8.0.1")
	cluster.Spec.UpgradeOptions.Schedule = "not a cron"
	defer resetRegistry(cluster)
	fr := &fakeResolver{resp: recommended("percona/percona-valkey:9.0.1-1", "", "")}
	installResolver(t, fr)
	r, _, _ := vcReconciler(t, cluster)

	versionCheckClock = func() time.Time { return time.Date(2026, 6, 22, 5, 0, 0, 0, time.UTC) }
	if err := r.reconcileVersionCheck(context.Background(), cluster); err == nil {
		t.Fatalf("invalid cron did not error")
	}
	if fr.calls != 0 {
		t.Errorf("invalid cron still polled (%d calls)", fr.calls)
	}
}

// ---- engineTagOf helper -----------------------------------------------------

func TestEngineTagOf(t *testing.T) {
	cases := map[string]string{
		"percona/percona-valkey:9.0.1-1": "9.0.1-1",
		"percona/percona-valkey:8.0":     "8.0",
		"percona/percona-valkey":         "", // untagged
		"":                               "",
		"registry:5000/percona-valkey":   "", // host:port, no tag
		"registry:5000/valkey:9.0.1":     "9.0.1",
	}
	for img, want := range cases {
		if got := engineTagOf(img); got != want {
			t.Errorf("engineTagOf(%q) = %q, want %q", img, got, want)
		}
	}
}

// ---- helpers ----------------------------------------------------------------

func windowExists(cluster *valkeyv1alpha1.PerconaValkeyCluster) bool {
	versionChecks.mu.Lock()
	defer versionChecks.mu.Unlock()
	_, ok := versionChecks.entries[vcKey(cluster)]
	return ok
}

func assertEvent(t *testing.T, rec *events.FakeRecorder, reason string) {
	t.Helper()
	if !hasEvent(rec, reason) {
		t.Errorf("expected an event containing %q, none found", reason)
	}
}

func hasEvent(rec *events.FakeRecorder, reason string) bool {
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				return true
			}
		default:
			return false
		}
	}
}
