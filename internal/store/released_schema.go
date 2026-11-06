package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// The schema ceiling is the highest schema version a raise may put in place.
// Two bounds make it, and the lower wins (version.SchemaCeiling.Lower): the
// released-schema ceiling, the highest schema version a published lit release
// carries, as the composition root read it from the binary's build stamp and
// handed it down on the store's environ.Env; and the store's schema hold
// (schema_hold.go), the meta row a remote downgrade writes. No existing
// workspace is migrated past the ceiling, no fetched head is lifted past it,
// and no push raises a remote to a schema above it. A push at or below the
// remote head's own schema is no raise and the ceiling does not judge it: the
// remote already holds that schema, released by a release's push and at or
// below its own hold, so the push sends nothing the remote lacks
// (design-docs/schema-reversibility.md). Creating a store is not
// bounded by it, so a scratch workspace can be made at any schema the binary
// carries. [LAW:single-enforcer] the refusals below are the only readers of
// the ceiling.

// SchemaRaise names which write would have raised a schema past the ceiling,
// so a refusal says what it refused.
type SchemaRaise string

const (
	// RaiseMigrate is a migrating open applying pending migrations.
	RaiseMigrate SchemaRaise = "migrate"
	// RaiseRepair is a migrating open restoring a version's missing content.
	RaiseRepair SchemaRaise = "repair"
	// RaiseLift is a sync lifting a commit it reads to this lit's schema.
	RaiseLift SchemaRaise = "lift"
)

// SchemaCeilingError refuses a write that would put an existing workspace's
// schema, or a commit a sync reads, at a version above the schema ceiling.
type SchemaCeilingError struct {
	Raise SchemaRaise
	// WorkspaceVersion is the schema version the workspace or commit records.
	// It is meaningful only when PreVersioned is false.
	WorkspaceVersion int64
	// PreVersioned is true for a workspace that predates schema versioning:
	// it records no version at all, which is not version 0.
	PreVersioned bool
	// TargetVersion is the highest version the write would have put in place.
	TargetVersion int64
	// Ceiling is the bound that refused, and its Source says which one it
	// was: the released-schema ceiling or a store's hold.
	Ceiling version.SchemaCeiling
}

func (e *SchemaCeilingError) Error() string {
	from := fmt.Sprintf("schema version %d", e.WorkspaceVersion)
	if e.PreVersioned {
		from = "its unversioned pre-migration shape"
	}
	var refused string
	switch e.Raise {
	case RaiseRepair:
		refused = fmt.Sprintf("refusing to restore this workspace's missing schema content for version %d", e.TargetVersion)
	case RaiseLift:
		refused = fmt.Sprintf("refusing to lift a commit from schema version %d to %d for a sync", e.WorkspaceVersion, e.TargetVersion)
	default:
		refused = fmt.Sprintf("refusing to migrate this workspace from %s to schema version %d", from, e.TargetVersion)
	}
	return fmt.Sprintf("%s: the schema ceiling here is %d (%s), and this lit never puts a schema above that ceiling in place in an existing store%s",
		refused, e.Ceiling.Max, e.Ceiling.Source, holdRemedyClause(e.Ceiling))
}

// holdRemedyClause is the clause a hold's refusal ends with: the store knows
// the hold's version, not which release carries it, so it names the release
// by schema (design-docs/schema-reversibility.md, "Lowering a remote"). A
// refusal by the released-schema ceiling names none: no release carries the
// schema the write would put in place. [LAW:dataflow-not-control-flow] the
// bound is the domain's discriminator. [LAW:one-source-of-truth] the open's
// refusal and the lift's end with the one clause.
func holdRemedyClause(ceiling version.SchemaCeiling) string {
	switch ceiling.Bound {
	case version.BoundHold:
		return fmt.Sprintf("; the release at schema version %d (`lit downgrade --to <tag>`) steps this store down to the hold", ceiling.Max)
	default:
		return ""
	}
}

// SchemaCeilingLiftError refuses a reconcile against remote/branch that would
// lift a commit it reads above the schema ceiling: the released-schema
// ceiling, or the hold the commit carries, whichever is lower. It is the
// lift's refusal (Lift, a SchemaCeilingError at RaiseLift) stamped with the
// remote and branch the reconcile was against, which the lift itself does not
// know; the CLI reads this type, and reads the schemas and the ceiling through
// Lift, the one record of them, held by value so there is no refusal without
// its lift. Lift's TargetVersion is the lift's next step, the first migration
// above the ceiling, not this lit's schema. The chain under it still names
// the commit the lift refused, which is the fetched head or a local head held
// below this lit's schema. [LAW:parse-dont-validate] [LAW:one-source-of-truth]
type SchemaCeilingLiftError struct {
	Remote string
	Branch string
	Lift   SchemaCeilingError
	cause  error
}

// Error is the lift's own refusal, said for the remote the reconcile was
// against. [LAW:one-source-of-truth] the refusal is worded once, by Lift.
func (e *SchemaCeilingLiftError) Error() string {
	return fmt.Sprintf("refusing to settle this backlog against %s/%s: %v", e.Remote, e.Branch, &e.Lift)
}

// Unwrap is the error the reconcile's replay returned, with the lift's own
// refusal and the commit it refused in its chain.
func (e *SchemaCeilingLiftError) Unwrap() error { return e.cause }

// EventRecord is the remote and branch refused; the lift's own record, under
// this one in the chain, carries the schemas and the ceiling.
func (e *SchemaCeilingLiftError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"schema_ceiling.remote": e.Remote,
		"schema_ceiling.branch": e.Branch,
	}, nil
}

// nameLiftRefusal stamps a lift's ceiling refusal in err with the remote and
// branch the reconcile was against, and returns every other error unchanged.
// [LAW:single-enforcer] replayUnderGuard runs it on every replay's error, the
// one exit a lift refusal has toward a caller.
func nameLiftRefusal(err error, remote storage.RemoteName, branch string) error {
	var refused *SchemaCeilingError
	if !errors.As(err, &refused) || refused.Raise != RaiseLift {
		return err
	}
	return &SchemaCeilingLiftError{Remote: remote.String(), Branch: branch, Lift: *refused, cause: err}
}

// EventRecord is what the write was, the schema it started from, the one it
// would have put in place, and the ceiling that refused it.
func (e *SchemaCeilingError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"schema_ceiling.raise":            string(e.Raise),
		"schema_ceiling.workspace_schema": e.WorkspaceVersion,
		"schema_ceiling.pre_versioned":    e.PreVersioned,
		"schema_ceiling.target_schema":    e.TargetVersion,
		"schema_ceiling.ceiling":          e.Ceiling.Max,
		"schema_ceiling.ceiling_source":   e.Ceiling.Source,
	}, nil
}

// SchemaCeilingPushError refuses a push of a commit whose schema is above the
// schema ceiling for that remote: the released-schema ceiling, or the hold the
// remote's head carries, whichever is lower.
type SchemaCeilingPushError struct {
	Remote       string
	Branch       string
	CommitSchema int64
	Ceiling      version.SchemaCeiling
}

func (e *SchemaCeilingPushError) Error() string {
	return fmt.Sprintf(
		"refusing to push to %s/%s: the commit is at schema version %d, and the schema ceiling for this push is %d (%s); "+
			"this lit never raises a remote to a schema above that ceiling",
		e.Remote, e.Branch, e.CommitSchema, e.Ceiling.Max, e.Ceiling.Source)
}

// EventRecord is the remote refused, the commit's schema, and the ceiling.
func (e *SchemaCeilingPushError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"schema_ceiling.remote":         e.Remote,
		"schema_ceiling.branch":         e.Branch,
		"schema_ceiling.commit_schema":  e.CommitSchema,
		"schema_ceiling.ceiling":        e.Ceiling.Max,
		"schema_ceiling.ceiling_source": e.Ceiling.Source,
	}, nil
}

var (
	_ model.RecordedError = (*SchemaCeilingError)(nil)
	_ model.RecordedError = (*SchemaCeilingPushError)(nil)
	_ model.RecordedError = (*SchemaCeilingLiftError)(nil)
)

// refuseCeilingRaise refuses a migrating open that would put a version above
// the ceiling state.ceiling gives it in place, before the open writes
// anything: the pending migrations it would apply, or the missing content of
// an applied version above the ceiling that drift repair would restore. It
// asks the question upByOne asks of every step, of the whole open at once, so
// the open refuses with nothing written instead of partway through.
func (s *Store) refuseCeilingRaise(ctx context.Context, state migrationState, drift *VersionContentMismatchError) error {
	ceiling := state.ceiling(s.env.ReleasedSchema())
	if state.willMutate() && state.support.Max > ceiling.Max {
		return &SchemaCeilingError{
			Raise:            RaiseMigrate,
			WorkspaceVersion: state.appliedVersion,
			PreVersioned:     state.phase == phaseAdopt,
			TargetVersion:    state.support.Max,
			Ceiling:          ceiling,
		}
	}
	if drift == nil || state.appliedVersion <= ceiling.Max {
		return nil
	}
	missing, err := s.missingVersionContent(ctx, state.appliedVersion)
	if err != nil {
		return err
	}
	var highest int64
	for _, target := range missing {
		highest = max(highest, target.version)
	}
	if highest <= ceiling.Max {
		return nil
	}
	return &SchemaCeilingError{
		Raise:            RaiseRepair,
		WorkspaceVersion: state.appliedVersion,
		TargetVersion:    highest,
		Ceiling:          ceiling,
	}
}

// nextStepWithinCeiling is the version of the migration goose applies next,
// refused when it is above ceiling. That migration is the lowest pending one,
// which is not always above the highest applied: a history applied out of
// order has a gap below it. It costs one read of the version table, so a lift
// with nothing pending costs next to nothing. With nothing pending it is 0,
// and goose reports ErrNoNextVersion itself.
func (s *Store) nextStepWithinCeiling(ctx context.Context, raise SchemaRaise, ceiling version.SchemaCeiling) (int64, error) {
	applied, highest, err := s.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	registry, err := migrations.Embedded()
	if err != nil {
		return 0, err
	}
	for _, m := range registry.Migrations() {
		if applied[m.Version] {
			continue
		}
		if m.Version <= ceiling.Max {
			return m.Version, nil
		}
		return 0, &SchemaCeilingError{Raise: raise, WorkspaceVersion: highest, TargetVersion: m.Version, Ceiling: ceiling}
	}
	return 0, nil
}

// appliedVersions is the set of versions goose records as applied, and the
// highest of them, in one read. A working set with no version table yet has
// none applied.
func (s *Store) appliedVersions(ctx context.Context) (map[int64]bool, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version_id FROM `+gooseVersionTable+` WHERE is_applied`)
	if err != nil {
		if isMissingTableError(err) {
			return map[int64]bool{}, 0, nil
		}
		return nil, 0, fmt.Errorf("read applied migration versions: %w", err)
	}
	defer rows.Close()
	applied := map[int64]bool{}
	var highest int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, 0, fmt.Errorf("read applied migration versions: %w", err)
		}
		applied[v] = true
		highest = max(highest, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read applied migration versions: %w", err)
	}
	return applied, highest, nil
}

// refuseSchemaAboveCeiling refuses a push of a head at commitSchema to
// remote/branch whose head is at remoteHead, when the push raises the remote
// (remoteSchema.raisedBy) to a schema above ceiling, the lower of the
// released-schema ceiling and the hold at that head. A push at or below the
// remote's schema is no raise and lands under any ceiling. The caller read
// the schema at the head it sends; both push sites run this one verdict on
// it. [LAW:effects-at-boundaries] a pure verdict on the values the caller
// read.
func refuseSchemaAboveCeiling(remote storage.RemoteName, branch string, remoteHead remoteSchema, commitSchema int64, ceiling version.SchemaCeiling) error {
	if !remoteHead.raisedBy(commitSchema) || commitSchema <= ceiling.Max {
		return nil
	}
	return &SchemaCeilingPushError{
		Remote:       remote.String(),
		Branch:       branch,
		CommitSchema: commitSchema,
		Ceiling:      ceiling,
	}
}
