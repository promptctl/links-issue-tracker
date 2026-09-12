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
	// Origin names the producer that built this binary. It is stamped rather
	// than inferred because inference provably does not work here: goreleaser
	// stamps Version from the tag ("0.14.0") and install.sh's source mode
	// stamps it from `git describe`, which on a tagged commit emits that exact
	// same string. No parse of Version can separate the two, and a parse that
	// tried would be a loose matcher over an exactly-produced value — so
	// provenance is carried as its own fact, never guessed from another one.
	// [LAW:types-are-the-program]
	Origin string
)

// OriginRelease is the only value meaning "this binary will not be refreshed
// by rebuilding a working tree". Every other value — including the empty
// string a bare `go build` or `go test` leaves — describes a binary built from
// a tree that can land changes without it, so the unstamped case reads as
// from-source: the direction that warns rather than the one that goes quiet.
// [LAW:no-silent-failure]
const OriginRelease = "release"

// OriginSource is what both from-source entrypoints stamp — the Justfile's
// `build` recipe and scripts/install.sh's source mode, via the shared
// LIT_BUILD_ORIGIN in scripts/version-ldflags.sh.
const OriginSource = "source"

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
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	IsDev   bool   `json:"is_dev"`
	// FromSource is true when this binary was built from a working tree that
	// can move on without it — the discriminator build-age staleness actually
	// turns on. Deliberately a second field beside IsDev rather than a reading
	// of it: `just install` stamps a `git describe` Version, so the binary
	// agents run has IsDev == false while being exactly the locally-built
	// binary whose age matters. "Was a Version stamped" (what the upgrade
	// resolver and the migration runner's producer-stamp guard ask) and "can
	// this binary be behind its own tree" are different questions, and each
	// gets its own field instead of one field answering both wrongly.
	// [LAW:one-source-of-truth]
	FromSource bool          `json:"from_source"`
	Schema     SchemaSupport `json:"schema_support"`
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
		Version:    Version,
		Commit:     Commit,
		Date:       Date,
		IsDev:      Version == "",
		FromSource: Origin != OriginRelease,
		Schema:     SchemaSupport{Min: migrations.Baseline, Max: max},
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

// StaleSourceBuild reports whether this binary is old enough that the working
// tree it was built from has had time to land fixes it does not carry, and
// hands back the age behind that verdict so no caller re-asks BuildAge for the
// number it is about to print.
//
// This is the one predicate every staleness surface reads — `lit version`'s
// warning line, the build-status note on doctor/sync/init, and the next/backlog
// banner — so the three cannot reach different verdicts about one binary. They
// did: `lit version` warned on age alone while the build-status note required
// IsDev, which is how a `just install` binary could be told "run just build" by
// one command and called a release by the next. [LAW:single-enforcer]
//
// MUST REPORT STALE: a source build dated at or past StaleBuildThreshold (the
// comparison is >=, so the boundary itself is stale); an Origin-unstamped build
// dated past it, since an unknown producer is read as from-source.
//
// MUST REPORT FRESH: a release build at any age — a released binary ages by
// design and is refreshed by `lit upgrade` rather than a rebuild, so warning
// there would fire forever on every released install and train its reader past
// the loud case this exists to make loud; a source build inside the threshold;
// and a source build whose Date is absent, unparseable, or in the future,
// because BuildAge refuses to hand back an age it cannot stand behind and a
// staleness claim is exactly a claim about age.
func (i Info) StaleSourceBuild(now time.Time) (age time.Duration, stale bool) {
	age, ok := i.BuildAge(now)
	return age, ok && i.FromSource && age >= StaleBuildThreshold
}
