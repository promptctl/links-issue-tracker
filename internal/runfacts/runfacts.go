// Package runfacts is the facts one lit run is composed with: the deadlines its
// work runs under, the released-schema ceiling it migrates and pushes below,
// and whether it may raise a remote's schema. They are one value, built once
// per run — by cmd/lit from its build stamps, by a test suite from suiteenv,
// by a worker from its spawner's handoff — and carried whole on the run's
// environ.Env, so a run has all three or none, never some.
// [LAW:types-are-the-program] [LAW:one-source-of-truth]
package runfacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/deadline"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// Facts is one run's facts, every one present: only Of and Parse build one.
// The zero Facts is no run's, and reading a fact from it panics naming Of, so
// code handed none fails where it reads one; IsZero is for the one boundary
// that refuses a run given none (cli.Run).
type Facts struct {
	deadlines      deadline.Deadlines
	releasedSchema version.SchemaCeiling
	raisePolicy    version.RaisePolicy
}

// Of is the run facts d, released and policy give, or why they give none: a
// run's deadlines must be built (deadline.Of or deadline.Shipped), and its
// ceiling must be the released bound a build stamp or a suite names, since that
// is the one bound a run is composed with; a store's hold lowers it later.
// [LAW:parse-dont-validate]
func Of(d deadline.Deadlines, released version.SchemaCeiling, policy version.RaisePolicy) (Facts, error) {
	if d.IsZero() {
		return Facts{}, errors.New("the run facts carry no deadlines")
	}
	if released.Bound != version.BoundReleased {
		return Facts{}, fmt.Errorf("the run's released-schema ceiling must be the %q bound, got %q", version.BoundReleased, released.Bound)
	}
	return Facts{deadlines: d, releasedSchema: released, raisePolicy: policy}, nil
}

// IsZero reports whether f is the zero Facts, which Of never builds.
func (f Facts) IsZero() bool { return f == Facts{} }

// facts is f, read through the one check that f is a run's.
// [LAW:single-enforcer]
func (f Facts) facts() Facts {
	if f.IsZero() {
		panic("runfacts: read a fact of the zero Facts; a run's facts are built by runfacts.Of")
	}
	return f
}

// Deadlines is the deadlines the run's work runs under.
func (f Facts) Deadlines() deadline.Deadlines { return f.facts().deadlines }

// ReleasedSchema is the highest schema version a published release carries,
// as the run was composed with it: no existing workspace is migrated past it,
// and no commit above it is pushed.
func (f Facts) ReleasedSchema() version.SchemaCeiling { return f.facts().releasedSchema }

// RaisePolicy is whether the run may raise a remote's schema.
func (f Facts) RaisePolicy() version.RaisePolicy { return f.facts().raisePolicy }

// wire is Facts as a command hands them to the worker it spawns. Every field
// is a pointer, so a key the bytes leave out reads back as absent and Parse
// refuses it, where a value field would read back as its zero: a missing
// raise_policy would run a release's worker as a non-release.
type wire struct {
	Deadlines      *deadline.Deadlines `json:"deadlines"`
	ReleasedSchema *wireCeiling        `json:"released_schema"`
	RaisePolicy    *wirePolicy         `json:"raise_policy"`
}

type wireCeiling struct {
	Max    *int64               `json:"max"`
	Source *string              `json:"source"`
	Bound  *version.SchemaBound `json:"bound"`
}

type wirePolicy struct {
	Release *bool `json:"release"`
}

// MarshalJSON writes f as the bytes Parse reads.
func (f Facts) MarshalJSON() ([]byte, error) {
	f = f.facts()
	return json.Marshal(wire{
		Deadlines:      &f.deadlines,
		ReleasedSchema: &wireCeiling{Max: &f.releasedSchema.Max, Source: &f.releasedSchema.Source, Bound: &f.releasedSchema.Bound},
		RaisePolicy:    &wirePolicy{Release: &f.raisePolicy.Release},
	})
}

// UnmarshalJSON reads f through Parse, so Facts decoded anywhere, as a field
// of a larger value included, are built by the one constructor and refused as
// Parse refuses them, never decoded to a zero or partial Facts.
// [LAW:single-enforcer]
func (f *Facts) UnmarshalJSON(data []byte) error {
	parsed, err := Parse(data)
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}

// Parse reads the run facts MarshalJSON wrote, refusing bytes that carry none
// and any fact or field they leave out, then builds them through Of. It is the
// one way handed facts become a run's. [LAW:parse-dont-validate]
// [LAW:single-enforcer]
func Parse(data []byte) (Facts, error) {
	if len(data) == 0 {
		return Facts{}, errors.New("it carries no run facts")
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return Facts{}, fmt.Errorf("read the run facts: %w", err)
	}
	var missing []string
	if w.Deadlines == nil {
		missing = append(missing, "deadlines")
	}
	if w.ReleasedSchema == nil {
		missing = append(missing, "released_schema")
	} else {
		if w.ReleasedSchema.Max == nil {
			missing = append(missing, "released_schema.max")
		}
		if w.ReleasedSchema.Source == nil {
			missing = append(missing, "released_schema.source")
		}
		if w.ReleasedSchema.Bound == nil {
			missing = append(missing, "released_schema.bound")
		}
	}
	if w.RaisePolicy == nil {
		missing = append(missing, "raise_policy")
	} else if w.RaisePolicy.Release == nil {
		missing = append(missing, "raise_policy.release")
	}
	if len(missing) > 0 {
		return Facts{}, fmt.Errorf("the run facts carry no %s", strings.Join(missing, ", "))
	}
	return Of(*w.Deadlines,
		version.SchemaCeiling{Max: *w.ReleasedSchema.Max, Source: *w.ReleasedSchema.Source, Bound: *w.ReleasedSchema.Bound},
		version.RaisePolicy{Release: *w.RaisePolicy.Release})
}
