package lifecycle

import (
	"errors"
	"strings"
)

// Resolution is the sealed set of reasons a `close` records — why the work was
// not finished. It is the payload of the closed lifecycle state and exists on
// no other state. Each member is behaviorally distinct for the next selector:
// duplicate and superseded redirect to a canonical ticket, obsolete means the
// need is gone, wontfix is a standing decision not to do the work.
//
// [LAW:types-are-the-program] Every legal resolution is a named constant and
// nothing else is constructible through ParseResolution, so downstream code
// matches on the constants instead of re-validating strings. This mirrors the
// RelationType pattern: values originate from ParseResolution at trust
// boundaries or from the constants; raw conversions are reserved for salvage
// paths that must conserve bytes.
type Resolution string

const (
	ResolutionDuplicate  Resolution = "duplicate"
	ResolutionSuperseded Resolution = "superseded"
	ResolutionObsolete   Resolution = "obsolete"
	ResolutionWontfix    Resolution = "wontfix"
)

// RedirectsToCanonical reports whether this resolution closes the issue *in
// favor of* another, canonical ticket — duplicate and superseded both do; the
// non-canonical issue redirects to its canonical counterpart. obsolete and
// wontfix are terminal: the need is gone or the decision stands, with nowhere
// to redirect.
// [LAW:single-enforcer] The one place that names the redirect subset. The
// `lit close` boundary requires a target exactly for these resolutions, and
// NewStatus attaches a redirect target to the closed leaf exactly beside these
// — both consult this predicate, so "which resolutions carry a target" cannot
// drift.
func (r Resolution) RedirectsToCanonical() bool {
	return r == ResolutionDuplicate || r == ResolutionSuperseded
}

// ParseResolution maps an untrusted resolution string (CLI flag, import
// payload) into the sealed set.
// [LAW:single-enforcer] The only string-to-Resolution gate; every trust
// boundary calls this instead of carrying its own equality chain.
func ParseResolution(s string) (Resolution, error) {
	switch r := Resolution(strings.TrimSpace(s)); r {
	case ResolutionDuplicate, ResolutionSuperseded, ResolutionObsolete, ResolutionWontfix:
		return r, nil
	default:
		return "", errors.New("resolution must be one of: duplicate, superseded, obsolete, wontfix")
	}
}

func cloneResolution(value *Resolution) *Resolution {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
