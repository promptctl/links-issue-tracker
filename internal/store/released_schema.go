package store

import (
	"context"
	"fmt"

	"github.com/pressly/goose/v3"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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

// UnreleasedMigrationError refuses raising an existing workspace's schema, or
// a commit's in a sync's lift, to a version above this lit's released-schema
// ceiling.
type UnreleasedMigrationError struct {
	// WorkspaceVersion is the schema version the workspace records. It is
	// meaningful only when PreVersioned is false.
	WorkspaceVersion int64
	// PreVersioned is true for a workspace that predates schema versioning:
	// it records no version at all, which is not version 0.
	PreVersioned  bool
	TargetVersion int64
	Ceiling       version.ReleasedSchemaCeiling
}

func (e *UnreleasedMigrationError) Error() string {
	from := fmt.Sprintf("schema version %d", e.WorkspaceVersion)
	if e.PreVersioned {
		from = "its unversioned pre-migration shape"
	}
	return fmt.Sprintf(
		"refusing to migrate this workspace from %s to schema version %d: this lit's released-schema ceiling is %d (%s), "+
			"and it never migrates an existing workspace past that ceiling",
		from, e.TargetVersion, e.Ceiling.Max, e.Ceiling.Source)
}

// EventRecord is the schema the workspace is at, the one the raise would have
// moved it to, and the ceiling that refused it.
func (e *UnreleasedMigrationError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
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

// refuseUnreleasedRaise refuses a migrating open whose target is above the
// ceiling raiseCeiling gives it, before the open writes anything. It asks the
// question upByOne asks of every step, of the whole raise at once, so the
// open refuses with nothing written instead of partway through.
func (s *Store) refuseUnreleasedRaise(state migrationState) error {
	ceiling := raiseCeiling(state, s.env.ReleasedSchema())
	if state.support.Max <= ceiling.Max {
		return nil
	}
	return &UnreleasedMigrationError{
		WorkspaceVersion: state.appliedVersion,
		PreVersioned:     state.phase == phaseAdopt,
		TargetVersion:    state.support.Max,
		Ceiling:          ceiling,
	}
}

// refuseStepPastCeiling refuses the step goose would take next when the
// migration it applies is above ceiling. That migration is the lowest pending
// one, which is not always above the highest applied: a history applied out
// of order has a gap below it. With nothing pending it passes, and goose
// reports ErrNoNextVersion itself.
func refuseStepPastCeiling(ctx context.Context, provider *goose.Provider, ceiling version.ReleasedSchemaCeiling) error {
	statuses, err := provider.Status(ctx)
	if err != nil {
		return fmt.Errorf("read pending migrations before a migration step: %w", err)
	}
	var applied int64
	next := int64(-1)
	for _, st := range statuses {
		switch {
		case st.State == goose.StateApplied:
			applied = max(applied, st.Source.Version)
		case st.State == goose.StatePending && (next < 0 || st.Source.Version < next):
			next = st.Source.Version
		}
	}
	if next < 0 || next <= ceiling.Max {
		return nil
	}
	return &UnreleasedMigrationError{WorkspaceVersion: applied, TargetVersion: next, Ceiling: ceiling}
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
