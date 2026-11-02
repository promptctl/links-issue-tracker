package store

import (
	"context"
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
// and no commit above it is pushed to any remote. Creating a store is not
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
	return fmt.Sprintf("%s: the schema ceiling here is %d (%s), and this lit never puts a schema above that ceiling in place in an existing store",
		refused, e.Ceiling.Max, e.Ceiling.Source)
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
			"this lit never sends a commit above that ceiling to a remote",
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

// refuseSchemaAboveCeiling refuses a push of a head at schema when that
// schema is above ceiling, the lower of the released-schema ceiling and the
// hold at the remote head the push is judged against. The caller read the
// schema at the head it sends; both push sites run this one verdict on it.
// [LAW:effects-at-boundaries]
func refuseSchemaAboveCeiling(remote storage.RemoteName, branch string, schema int64, ceiling version.SchemaCeiling) error {
	if schema <= ceiling.Max {
		return nil
	}
	return &SchemaCeilingPushError{
		Remote:       remote.String(),
		Branch:       branch,
		CommitSchema: schema,
		Ceiling:      ceiling,
	}
}
