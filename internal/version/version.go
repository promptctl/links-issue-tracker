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
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// Build-time identity. Populated by `-ldflags "-X .../internal/version.Version=...
// -X .../internal/version.Commit=... -X .../internal/version.Date=..."` at link
// time. Empty strings indicate a build that did not stamp them — treated as a
// development build in Info.IsDev.
var (
	Version string
	Commit  string
	Date    string
)

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
