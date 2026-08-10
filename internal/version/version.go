// Package version is the single source of truth for "what binary am I and
// what can I do." It exposes a typed Info value carrying the binary's identity
// (link-time-injected version/commit/build-date) plus its capability bounds
// (the schema-version range it can produce, derived from the embedded
// migration registry). Downstream code — the `lit version` command, the
// release manifest (internal/release), the `lit downgrade` resolver
// (downgrade epic .4), and the refusal-message upgrade (.5) — all read this
// Info; nothing reconstructs it from parsed strings or duplicates its fields.
//
// [LAW:one-source-of-truth] One typed Info per binary; the schema fields are
// derived from internal/store/migrations at call time, not stored as separate
// constants that could drift.
// [LAW:single-enforcer] Only the package-level variables below are written at
// link time (by goreleaser or scripts/install.sh). No other code mutates them.
package version

import (
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// Build-time identity. Populated by `-ldflags "-X .../internal/version.Version=...
// -X .../internal/version.Commit=... -X .../internal/version.Date=..."` at link
// time. Empty strings indicate a build that did not stamp them — treated as a
// development build in Info.IsDev.
//
// [LAW:single-enforcer] Three writers stamp these, and only these: goreleaser
// (all three fields, for tagged releases), scripts/install.sh's source mode
// (all three, Version via `git describe`), and the Justfile's `build` recipe
// (Commit + Date only, via scripts/version-ldflags.sh — deliberately NOT
// Version, so a plain `just build` stays IsDev==true; see BuildAge below for
// why Commit/Date alone are still worth stamping).
var (
	Version string
	Commit  string
	Date    string
)

// StaleBuildThreshold is the build age past which `lit version` flags a
// locally built binary as worth rebuilding. This package's build-age
// reporting exists because a stale local binary silently missing a landed
// fix is the suspected root cause of the field incident that motivated the
// links-sync-pgct epic — nothing in `lit version` could tell anyone the
// binary predated the fix. [LAW:one-source-of-truth] the one constant every
// staleness check compares against.
const StaleBuildThreshold = 7 * 24 * time.Hour

// Info is the typed snapshot of this binary's identity and capabilities. It is
// the single shape every downstream consumer reads; consumers MUST NOT parse
// `lit version` human output to reconstruct any field on this struct.
//
// [LAW:types-are-the-program] Every field is either link-time identity
// (Version/Commit/Date) or registry-derived (Schema). IsDev is the explicit
// boolean for the "no version stamped at link time" case, promoted to a field
// so consumers don't reimplement `info.Version == ""`.
type Info struct {
	Version string        `json:"version"`
	Commit  string        `json:"commit"`
	Date    string        `json:"date"`
	IsDev   bool          `json:"is_dev"`
	Schema  SchemaSupport `json:"schema_support"`
}

// SchemaSupport is the inclusive schema-version range this binary can produce
// against a workspace. Min is the registry's baseline; Max is its highest
// migration. Both are derived from internal/store/migrations at call time.
//
// [LAW:one-source-of-truth] These bounds are the same numbers the migration
// runner uses to decide forward-compat. Code that needs the bounds reads them
// from internal/store/migrations directly; this struct exists to expose them
// alongside the binary identity, not as a parallel source.
type SchemaSupport struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// Get returns this binary's Info. It performs one ReadDir over the embedded
// migration registry to derive SchemaSupport.Max; cheap but not free, so
// callers that fan out (e.g., a tight loop) should cache the result.
func Get() (Info, error) {
	max, err := migrations.MaxVersion()
	if err != nil {
		return Info{}, err
	}
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		IsDev:   Version == "",
		Schema:  SchemaSupport{Min: migrations.Baseline, Max: max},
	}, nil
}

// BuildAge reports how long ago i.Date was stamped, relative to now. ok is
// false when Date is empty, fails to parse as RFC3339, or names a future
// instant (clock skew) — an unstamped build, or a value that cannot be
// trusted, must never render a fabricated age.
// [LAW:effects-at-boundaries] the clock is a parameter, not read internally,
// so this stays a pure function callers can test with no mocks.
func (i Info) BuildAge(now time.Time) (age time.Duration, ok bool) {
	if i.Date == "" {
		return 0, false
	}
	stamped, err := time.Parse(time.RFC3339, i.Date)
	if err != nil {
		return 0, false
	}
	age = now.Sub(stamped)
	if age < 0 {
		return 0, false
	}
	return age, true
}
