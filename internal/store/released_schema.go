package store

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// The released-schema ceiling is the highest schema version a published lit
// release carries, as the composition root read it from the binary's build
// stamp and handed it down on the store's environ.Env. No existing workspace is
// migrated past it, and no commit above it is pushed to any remote: a schema no
// release carries never leaves a scratch store. Creating a store is not
// bounded by it, so a scratch workspace can be made at any schema the binary
// carries. [LAW:single-enforcer] the two refusals below are the only readers of
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

// UnreleasedMigrationError refuses a write that would put an existing
// workspace's schema, or a commit a sync reads, at a version above this lit's
// released-schema ceiling.
type UnreleasedMigrationError struct {
	Raise SchemaRaise
	// WorkspaceVersion is the schema version the workspace or commit records.
	// It is meaningful only when PreVersioned is false.
	WorkspaceVersion int64
	// PreVersioned is true for a workspace that predates schema versioning:
	// it records no version at all, which is not version 0.
	PreVersioned bool
	// TargetVersion is the highest version the write would have put in place.
	TargetVersion int64
	Ceiling       version.ReleasedSchemaCeiling
}

func (e *UnreleasedMigrationError) Error() string {
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
	return fmt.Sprintf("%s: this lit's released-schema ceiling is %d (%s), and it never puts a schema above that ceiling in place in an existing store",
		refused, e.Ceiling.Max, e.Ceiling.Source)
}

// EventRecord is what the write was, the schema it started from, the one it
// would have put in place, and the ceiling that refused it.
func (e *UnreleasedMigrationError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"released_schema.raise":            string(e.Raise),
		"released_schema.workspace_schema": e.WorkspaceVersion,
		"released_schema.pre_versioned":    e.PreVersioned,
		"released_schema.target_schema":    e.TargetVersion,
		"released_schema.ceiling":          e.Ceiling.Max,
		"released_schema.ceiling_source":   e.Ceiling.Source,
	}, nil
}

// UnreleasedPushError refuses a push of a commit whose schema is above this
// lit's released-schema ceiling, whatever the remote is.
type UnreleasedPushError struct {
	Remote       string
	Branch       string
	CommitSchema int64
	Ceiling      version.ReleasedSchemaCeiling
}

func (e *UnreleasedPushError) Error() string {
	return fmt.Sprintf(
		"refusing to push to %s/%s: the commit is at schema version %d, and this lit's released-schema ceiling is %d (%s); "+
			"it never sends a commit above that ceiling to a remote",
		e.Remote, e.Branch, e.CommitSchema, e.Ceiling.Max, e.Ceiling.Source)
}

// EventRecord is the remote refused, the commit's schema, and the ceiling.
func (e *UnreleasedPushError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"released_schema.remote":         e.Remote,
		"released_schema.branch":         e.Branch,
		"released_schema.commit_schema":  e.CommitSchema,
		"released_schema.ceiling":        e.Ceiling.Max,
		"released_schema.ceiling_source": e.Ceiling.Source,
	}, nil
}

var (
	_ model.RecordedError = (*UnreleasedMigrationError)(nil)
	_ model.RecordedError = (*UnreleasedPushError)(nil)
)

// raiseCeiling is the ceiling a migrating open of state may raise its
// workspace to: the released-schema ceiling for an existing workspace, and the
// binary's own schema for a fresh one, which is created rather than raised.
func raiseCeiling(state migrationState, released version.ReleasedSchemaCeiling) version.ReleasedSchemaCeiling {
	if state.phase == phaseFresh {
		return version.ReleasedSchemaCeiling{Max: state.support.Max, Source: "a new store is created at the schema this lit carries"}
	}
	return released
}

// refuseUnreleasedRaise refuses a migrating open that would put a version
// above the ceiling raiseCeiling gives it in place, before the open writes
// anything: the pending migrations it would apply, or the missing content of
// an applied version above the ceiling that drift repair would restore. It
// asks the question upByOne asks of every step, of the whole open at once, so
// the open refuses with nothing written instead of partway through.
func (s *Store) refuseUnreleasedRaise(ctx context.Context, state migrationState, drift *VersionContentMismatchError) error {
	ceiling := raiseCeiling(state, s.env.ReleasedSchema())
	if state.willMutate() && state.support.Max > ceiling.Max {
		return &UnreleasedMigrationError{
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
	return &UnreleasedMigrationError{
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
func (s *Store) nextStepWithinCeiling(ctx context.Context, raise SchemaRaise, ceiling version.ReleasedSchemaCeiling) (int64, error) {
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
		return 0, &UnreleasedMigrationError{Raise: raise, WorkspaceVersion: highest, TargetVersion: m.Version, Ceiling: ceiling}
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

// refuseUnreleasedPush refuses a push of commitHash when the schema recorded
// at it is above the released-schema ceiling.
func (s *Store) refuseUnreleasedPush(ctx context.Context, remote storage.RemoteName, branch string, commitHash string) error {
	if !isDoltCommitHash(commitHash) {
		return fmt.Errorf("push schema: %q is not a Dolt commit hash", commitHash)
	}
	schema, err := s.schemaVersionAtCommit(ctx, commitHash)
	if err != nil {
		return err
	}
	ceiling := s.env.ReleasedSchema()
	if schema <= ceiling.Max {
		return nil
	}
	return &UnreleasedPushError{
		Remote:       remote.String(),
		Branch:       branch,
		CommitSchema: schema,
		Ceiling:      ceiling,
	}
}
