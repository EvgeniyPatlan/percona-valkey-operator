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
