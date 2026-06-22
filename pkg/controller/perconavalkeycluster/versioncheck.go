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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/version"
	"valkey.percona.com/percona-valkey-operator/pkg/version/service"
)

// This file is the M6 VERSION-CHECK SEAM (GO-6.4/6.5/6.6/6.7). It owns the
// upgradeOptions resolution + Percona-style version-service interaction that
// mutates spec.image (only-if-differs) so the smart update in smartupdate.go can
// roll the engine. The version-check leg fills reconcileVersionCheck with:
//
//   - the apply-policy decision (Disabled/Recommended/Latest/<literal>, GO-6.5);
//   - the version-service client call via service.RecommendedImageResolver (GO-6.4);
//   - the reconcile-spawned robfig/cron lifecycle keyed by cluster (GO-6.6);
//   - the status-mutex-guarded, only-if-differs spec.image mutation (GO-6.7);
//   - the VersionCheckFailed skip-on-failure semantics (no roll, no Degrade, E6).
//
// The upgrade reason/event vocabulary is declared HERE (not status.go) so the two
// M6 seam files stay disjoint from the M3/M4/M5 status vocabulary and from each
// other (smartupdate.go owns the gate/roll vocabulary; this file owns the
// version-service vocabulary). Disabled (the default) makes ZERO client calls.

// Version-service condition reasons + event reasons (09 §3; GO-6.6/6.7). Declared
// in the version-check seam to avoid collision with status.go and smartupdate.go.
const (
	// ReasonVersionCheckFailed is the informational reason emitted when a
	// version-service poll fails (unreachable or VS error). It does NOT set a
	// Degraded condition and does NOT roll — the poll is retried next window (E6).
	ReasonVersionCheckFailed = "VersionCheckFailed"
	// ReasonVersionCheckDisabled marks that upgradeOptions.apply is Disabled, so no
	// poll runs and spec.image is used as-is.
	ReasonVersionCheckDisabled = "VersionCheckDisabled"
	// ReasonNewEnginePinResolved marks that the version service returned a new
	// engine pin which was applied to spec.image (only-if-differs), arming a roll.
	ReasonNewEnginePinResolved = "NewEnginePinResolved"
)

// versionCheckAction enumerates the resolved upgradeOptions.apply policy (GO-6.5).
type versionCheckAction int

const (
	// actionDisabled: no poll; spec.image is used as-is (the default, 09 §3).
	actionDisabled versionCheckAction = iota
	// actionRecommended: poll and adopt VSResponse.Recommended (production).
	actionRecommended
	// actionLatest: poll and adopt VSResponse.Latest (dev/staging).
	actionLatest
	// actionLiteral: poll to resolve the exact build tag for a pinned version.
	actionLiteral
)

// vsProduct is the version-service product key for this operator (09 §3).
const vsProduct = "valkey-operator"

// versionCheckProduct exposes vsProduct to in-package tests.
func versionCheckProduct() string { return vsProduct }

// applyPolicy maps spec.upgradeOptions.apply to a versionCheckAction (GO-6.5).
// Disabled/empty short-circuits before any client call; Recommended/Latest poll;
// anything else is treated as a literal version to resolve.
func applyPolicy(cr *valkeyv1alpha1.PerconaValkeyCluster) versionCheckAction {
	switch strings.TrimSpace(cr.Spec.UpgradeOptions.Apply) {
	case "", valkeyv1alpha1.UpgradeApplyDisabled:
		return actionDisabled
	case valkeyv1alpha1.UpgradeApplyRecommended:
		return actionRecommended
	case valkeyv1alpha1.UpgradeApplyLatest:
		return actionLatest
	default:
		return actionLiteral
	}
}

// resolverFactory builds a RecommendedImageResolver for a version-service
// endpoint. It is a package var so in-package tests inject a fake resolver
// without a live endpoint or a Reconciler struct field (the Reconciler is owned
// by controller.go, which this leg must not edit). Production builds the bounded
// HTTP resolver against the (possibly air-gapped) endpoint.
var resolverFactory = func(endpoint string) service.RecommendedImageResolver {
	return service.NewHTTPResolver(endpoint)
}

// versionCheckClock returns "now". It is a package var so tests drive the cron
// window deterministically with a fake clock (mirrors the M4 backup Scheduler's
// explicit-clock design — no background goroutine, no wall-clock waits).
var versionCheckClock = time.Now

// vcEntry is one cluster's registered version-check window. lastPoll anchors the
// once-per-window cadence: a poll fires only when the next cron slot after
// lastPoll has elapsed (09 §3 — per-cluster, once-per-window, no continuous poll).
type vcEntry struct {
	schedule cron.Schedule
	cronText string
	lastPoll time.Time
}

// vcRegistry is the per-process version-check cron registry, keyed by cluster
// NamespacedName. It is CR-lifecycle-bound: Disabled or a deleted CR removes the
// entry, so a poll never outlives its cluster (09 §3, no detached goroutine). The
// mutex also serves as the GO-6.7 "status mutex": a poll's only-if-differs
// spec.image mutation is serialized against itself across leader-handover jitter.
type vcRegistry struct {
	mu      sync.Mutex
	entries map[string]*vcEntry
}

// versionChecks is the singleton registry. Per-cluster keying means at most one
// window per cluster regardless of how many times reconcile runs; only the
// elected leader reconciles, so this never double-fires across replicas (R7).
var versionChecks = &vcRegistry{entries: map[string]*vcEntry{}}

// vcKey identifies a cluster across namespaces.
func vcKey(cr *valkeyv1alpha1.PerconaValkeyCluster) string {
	return cr.Namespace + "/" + cr.Name
}

// remove drops a cluster's window (Disabled/delete). Safe if absent.
func (reg *vcRegistry) remove(key string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	delete(reg.entries, key)
}

// sync upserts the window for a cluster's schedule, preserving the fire anchor
// across an unchanged or re-parsed schedule. A new entry anchors lastPoll at now
// so the FIRST poll is the next scheduled slot (no spurious poll on registration,
// matching the backup Scheduler). An invalid cron is returned as an error so a
// misconfigured schedule surfaces loudly (it is a config error, not an outage).
func (reg *vcRegistry) sync(key, cronText string, now time.Time) error {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if existing, ok := reg.entries[key]; ok && existing.cronText == cronText {
		return nil // unchanged
	}
	parsed, err := cron.ParseStandard(cronText)
	if err != nil {
		return fmt.Errorf("invalid upgradeOptions.schedule %q: %w", cronText, err)
	}
	anchor := now
	if existing, ok := reg.entries[key]; ok {
		anchor = existing.lastPoll // preserve cadence across a schedule edit
	}
	reg.entries[key] = &vcEntry{schedule: parsed, cronText: cronText, lastPoll: anchor}
	return nil
}

// due reports whether a scheduled window has elapsed for the cluster at now, and
// advances the fire anchor to that slot so the same window is not polled twice
// (forward-only, once-per-window). It returns false when no slot has elapsed.
func (reg *vcRegistry) due(key string, now time.Time) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	e, ok := reg.entries[key]
	if !ok {
		return false
	}
	// Collapse any missed slots to the most recent elapsed one (no burst polling).
	var last time.Time
	for anchor := e.lastPoll; ; {
		next := e.schedule.Next(anchor)
		if next.After(now) {
			break
		}
		last = next
		anchor = next
	}
	if last.IsZero() {
		return false
	}
	e.lastPoll = last
	return true
}

// reconcileVersionCheck resolves the engine-image target from spec.upgradeOptions
// (Disabled/Recommended/Latest/<literal>) and, when the resolved pin differs from
// the current spec.image, mutates it (status-mutex-guarded, only-if-differs) so
// the downstream smart update rolls the engine. A failed version-service poll is
// tolerated by design (E6): it logs, emits a VersionCheckFailed event, mutates
// nothing, sets no Degraded condition, and is retried on the next schedule window.
//
// It is dispatched in Reconcile BEFORE the node-stepping/smart-update step so a
// version-service-resolved spec.image is in place by the time the roll runs (09 §3
// flow: cron mutates the CR -> the rendered pod template changes -> step-6 rolls).
//
// Cadence: although this runs every reconcile, the version service is polled at
// most ONCE PER schedule window (default "0 4 * * *") via the per-cluster cron
// registry — a reconcile between windows is a cheap no-op. Disabled (the default)
// removes the window and makes ZERO client calls.
func (r *Reconciler) reconcileVersionCheck(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster,
) error {
	key := vcKey(cluster)
	now := versionCheckClock()

	// Disabled (default) or a cluster under deletion: tear the window down and use
	// spec.image as-is. No poll, no client call (09 §3, E4).
	if applyPolicy(cluster) == actionDisabled || !cluster.DeletionTimestamp.IsZero() {
		versionChecks.remove(key)
		return nil
	}

	// Keep the per-cluster window in lockstep with spec.upgradeOptions.schedule.
	// An invalid cron is a loud config error (returned to the caller); CheckNSetDefaults
	// already defaults the schedule when apply != Disabled, so this is non-empty here.
	if err := versionChecks.sync(key, cluster.Spec.UpgradeOptions.Schedule, now); err != nil {
		return err
	}

	// Poll only once per window. Between windows this is a cheap no-op so the
	// data plane is fully decoupled from the version service at steady state.
	if !versionChecks.due(key, now) {
		return nil
	}

	return r.pollAndApply(ctx, cluster)
}

// pollAndApply performs the once-per-window version-service poll and the
// only-if-differs spec.image mutation. A poll failure is swallowed (logged +
// VersionCheckFailed event) and returns nil so the reconcile never degrades on a
// transient outage (E6); a literal pin that the service cannot resolve is treated
// the same. The registry mutex guards the whole poll+mutate so concurrent passes
// (leader-handover jitter) never race the spec write (GO-6.7).
func (r *Reconciler) pollAndApply(ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster) error {
	log := logf.FromContext(ctx).WithValues("cluster", vcKey(cluster))
	versionChecks.mu.Lock()
	defer versionChecks.mu.Unlock()

	resolver := resolverFactory(cluster.Spec.UpgradeOptions.VersionServiceEndpoint)
	resp, err := resolver.Resolve(ctx, r.vsRequest(cluster))
	if err != nil {
		// Tolerated by design: log, emit an informational event, mutate nothing,
		// set NO Degraded condition, retry next window (09 §3, E6). Never an error
		// to the caller (that would set Ready=False via r.fail).
		log.Info("version-service poll failed; skipping until next window", "error", err.Error())
		r.recorder.Eventf(cluster, nil, eventWarning, ReasonVersionCheckFailed, "VersionCheck",
			"version-service poll failed (retrying next window): %s", err.Error())
		return nil
	}

	target := pickVersionSet(applyPolicy(cluster), resp)
	if target == nil || target.Engine == "" {
		log.Info("version service returned no usable engine pin; skipping")
		r.recorder.Eventf(cluster, nil, eventWarning, ReasonVersionCheckFailed, "VersionCheck",
			"version service returned no engine pin for apply=%q", cluster.Spec.UpgradeOptions.Apply)
		return nil
	}

	changed, err := r.applyResolvedImages(ctx, cluster, *target)
	if err != nil {
		return fmt.Errorf("apply resolved version-service images: %w", err)
	}
	if changed {
		r.recorder.Eventf(cluster, nil, eventNormal, ReasonNewEnginePinResolved, "VersionCheck",
			"resolved new engine pin %s (apply=%q); engine roll armed", target.Engine, cluster.Spec.UpgradeOptions.Apply)
	}
	return nil
}

// vsRequest projects the CR into the primitives-only version-service request,
// keeping the pkg/apis -> pkg/version dependency one-directional (no cycle).
func (r *Reconciler) vsRequest(cluster *valkeyv1alpha1.PerconaValkeyCluster) service.VSRequest {
	return service.VSRequest{
		Product:         vsProduct,
		OperatorVersion: version.Version(),
		CrVersion:       cluster.Spec.CrVersion,
		CurrentEngine:   engineTagOf(cluster.Spec.Image),
		Platform:        string(r.platform),
		Apply:           strings.TrimSpace(cluster.Spec.UpgradeOptions.Apply),
	}
}

// pickVersionSet selects Recommended/Latest per the apply policy. A literal pin
// resolves into Recommended by the service (09 §3), so it reads Recommended too.
func pickVersionSet(action versionCheckAction, resp *service.VSResponse) *service.VersionSet {
	if resp == nil {
		return nil
	}
	switch action {
	case actionLatest:
		return &resp.Latest
	case actionRecommended, actionLiteral:
		return &resp.Recommended
	default: // actionDisabled never reaches here
		return nil
	}
}

// applyResolvedImages mutates the engine/exporter/backup image pins ONLY when the
// resolved triple differs from the current spec, persisting via a MergeFrom PATCH
// of a COPY (never a full Update): the version service moves all three together
// so a recommended engine never pairs with an incompatible exporter/backup tool
// (09 §3 multi-image compatibility). It returns changed=true when a pin moved.
//
// Patching a COPY (then mirroring resourceVersion + the new pins onto the live
// object) avoids the omitempty+defaults round-trip footgun that a full Update
// would trip — exactly the discipline ensureFinalizers uses for metadata.
func (r *Reconciler) applyResolvedImages(
	ctx context.Context, cluster *valkeyv1alpha1.PerconaValkeyCluster, set service.VersionSet,
) (bool, error) {
	changed := set.Engine != "" && set.Engine != cluster.Spec.Image
	if set.Exporter != "" && set.Exporter != cluster.Spec.Exporter.Image {
		changed = true
	}
	if set.Backup != "" && set.Backup != cluster.Spec.Backup.Image {
		changed = true
	}
	if !changed {
		return false, nil // only-if-differs: a same-pin poll is a no-op (no roll).
	}

	base := cluster.DeepCopy()
	patchTarget := cluster.DeepCopy()
	if set.Engine != "" {
		patchTarget.Spec.Image = set.Engine
	}
	if set.Exporter != "" {
		patchTarget.Spec.Exporter.Image = set.Exporter
	}
	if set.Backup != "" {
		patchTarget.Spec.Backup.Image = set.Backup
	}
	if err := r.Patch(ctx, patchTarget, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("patch resolved engine pins: %w", err)
	}

	// Mirror the persisted change onto the live in-memory object so the SAME
	// reconcile pass renders the new pod template and arms the roll, and so a
	// later status writeback patches the current revision without a stale conflict.
	cluster.ResourceVersion = patchTarget.ResourceVersion
	if set.Engine != "" {
		cluster.Spec.Image = set.Engine
	}
	if set.Exporter != "" {
		cluster.Spec.Exporter.Image = set.Exporter
	}
	if set.Backup != "" {
		cluster.Spec.Backup.Image = set.Backup
	}
	return true, nil
}

// engineTagOf extracts the tag from a "repo:tag" image reference, or "" when the
// image is empty/untagged. It is the current-engine coordinate the version
// service uses to compute a safe forward target.
func engineTagOf(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 && i < len(image)-1 {
		// Guard against a registry port (host:port/repo) with no tag.
		if !strings.Contains(image[i+1:], "/") {
			return image[i+1:]
		}
	}
	return ""
}
