// Package version is the single source of truth for the operator version.
//
// version.txt (embedded at build time) holds the operator semver. Version()
// returns it; CompareVersion() compares the operator's major.minor against a
// target "x.y[.z]" string, which M1+ uses for crVersion API-compatibility
// gating in CheckNSetDefaults (see docs/architecture/09-upgrades-versioning.md
// and ADR-005). Patch is immaterial to crVersion gating, so comparison is on
// major.minor only.
//
// Dependency rule: this is a near-leaf (stdlib only). The version-service
// client (pkg/version/service) is a separate sub-package, wired in M6.
package version

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

//go:embed version.txt
var raw string // seed: "0.1.0"

const (
	// defaultServerImageRepo is the GA Valkey server image repository.
	defaultServerImageRepo = "percona/percona-valkey"
	// defaultBackupImageRepo is the GA Valkey backup-tool image repository.
	defaultBackupImageRepo = "percona/valkey-backup"
	// defaultEngineTag is the engine image tag the operator pins by default. It
	// is the major Valkey engine line shipped with this operator; the version
	// service overrides it when upgradeOptions.apply != Disabled. (Real GA tags
	// are baked in by `make release` in M7; this is the dev default.)
	defaultEngineTag = "8.0"
)

// gitVersion may be overridden at build time via -ldflags
// "-X .../pkg/version.gitVersion=x.y.z" (the Makefile's VERSION_LDFLAGS). When
// set it takes precedence over the embedded version.txt; otherwise version.txt
// remains the single source of truth.
var gitVersion string

// Version returns the operator semver: the -ldflags override if present, else
// the embedded version.txt (trimmed).
func Version() string {
	if v := strings.TrimSpace(gitVersion); v != "" {
		return v
	}
	return strings.TrimSpace(raw)
}

// CompareVersion compares the operator's major.minor against the target version
// "x.y[.z]". It returns -1 if the operator is older than target, 0 if equal at
// major.minor, and +1 if the operator is newer. Patch is intentionally ignored
// (immaterial to crVersion gating).
func CompareVersion(target string) int {
	aMaj, aMin := majorMinor(Version())
	bMaj, bMin := majorMinor(target)
	switch {
	case aMaj != bMaj:
		return sign(aMaj - bMaj)
	case aMin != bMin:
		return sign(aMin - bMin)
	default:
		return 0
	}
}

// MajorMinor returns the operator version as "major.minor" (patch dropped). It
// is the single source of truth for the crVersion auto-stamp in
// CheckNSetDefaults (ADR-005, doc 03 §5), keeping crVersion in lockstep with
// version.txt and avoiding the #1 Percona release footgun (crVersion drift).
func MajorMinor() string {
	maj, mnr := majorMinor(Version())
	return fmt.Sprintf("%d.%d", maj, mnr)
}

// Major returns the operator major version component (doc 09 §1, §2).
func Major() int {
	maj, _ := majorMinor(Version())
	return maj
}

// Minor returns the operator minor version component (doc 09 §1, §2).
func Minor() int {
	_, mnr := majorMinor(Version())
	return mnr
}

// Patch returns the operator patch version component. Patch is immaterial to
// crVersion gating (09 §2 patch-immateriality) but is surfaced for the startup
// log and image-tag alignment.
func Patch() int {
	parts := strings.SplitN(strings.TrimSpace(Version()), ".", 3)
	if len(parts) < 3 {
		return 0
	}
	p, _ := strconv.Atoi(parts[2])
	return p
}

// CompareMajorMinor compares two version strings on major.minor only (patch
// ignored), returning -1 if a<b, 0 if equal, +1 if a>b. It is the version-string
// vs version-string comparator the crVersion acceptance/monotonicity logic needs
// (09 §2, §7, §8) — distinct from CompareVersion, which compares the OPERATOR's
// own version against a target. An empty or non-numeric component sorts as 0.
func CompareMajorMinor(a, b string) int {
	aMaj, aMin := majorMinor(a)
	bMaj, bMin := majorMinor(b)
	switch {
	case aMaj != bMaj:
		return sign(aMaj - bMaj)
	case aMin != bMin:
		return sign(aMin - bMin)
	default:
		return 0
	}
}

// lastMinorOfPriorMajor is the highest minor line released under the major line
// immediately preceding a new major's ".0" (09 §8). The Valkey-operator release
// cadence ships minors 0,1,2 within a major before the next major's ".0", so the
// release-order predecessor of e.g. 2.0 is 1.2 (giving 2.0 the accepted set
// {1.2, 2.0}). This constant localizes that cadence assumption: if the cadence
// changes, only this value moves. Kept as a single source for the matrix logic
// in AcceptedCrVersions.
const lastMinorOfPriorMajor = 2

// AcceptedCrVersions computes the set of crVersion values an operator at
// opMajorMinor ("x.y") will reconcile, per the 09 §8 compatibility matrix: the
// operator's own minor plus the immediately-preceding RELEASED minor in release
// order. Within a major the predecessor of x.y is x.(y-1); across the major
// boundary the predecessor of X.0 is (X-1).<lastMinorOfPriorMajor> (so 2.0
// accepts {1.2, 2.0}). The set is computed, not hardcoded per version, and is
// returned in ascending order (predecessor first) so a caller can present a
// stable, deterministic list. A 1.0 (or any X.0 with no prior major, i.e.
// major<=1) yields just {own} since there is no released predecessor line.
func AcceptedCrVersions(opMajorMinor string) []string {
	maj, mnr := majorMinor(opMajorMinor)
	own := fmt.Sprintf("%d.%d", maj, mnr)
	switch {
	case mnr > 0:
		// Within the same major: predecessor is x.(y-1).
		return []string{fmt.Sprintf("%d.%d", maj, mnr-1), own}
	case maj > 1:
		// Major boundary X.0: predecessor is (X-1).<lastMinorOfPriorMajor>.
		return []string{fmt.Sprintf("%d.%d", maj-1, lastMinorOfPriorMajor), own}
	default:
		// 1.0 (or 0.x): no released predecessor line to accept.
		return []string{own}
	}
}

// DefaultServerImage returns the default Valkey server image used when
// spec.image is empty. The engine tag is independent of the operator version
// (the second Percona version axis); the version service overrides it.
func DefaultServerImage() string {
	return defaultServerImageRepo + ":" + defaultEngineTag
}

// DefaultBackupImage returns the default Valkey backup-tool image used when
// spec.backup.image is empty.
func DefaultBackupImage() string {
	return defaultBackupImageRepo + ":" + defaultEngineTag
}

// majorMinor parses the leading major and minor components of a version string.
// Missing or non-numeric components default to 0. The local minor variable is
// deliberately named mnr (not min) to avoid shadowing the predeclared builtin.
func majorMinor(v string) (int, int) {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	maj := 0
	if len(parts) > 0 {
		maj, _ = strconv.Atoi(parts[0])
	}
	mnr := 0
	if len(parts) > 1 {
		mnr, _ = strconv.Atoi(parts[1])
	}
	return maj, mnr
}

// sign returns -1, 0, or +1 for negative, zero, or positive n.
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
