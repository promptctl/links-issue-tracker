package store

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The released-schema ceiling is the highest schema version a published lit
// release carries, as the composition root read it from the binary's build
// stamp and handed it down on the store's environ.Env. No existing workspace is
// migrated past it, and no commit above it is pushed to any remote: a schema no
// release carries never leaves a scratch store. Creating a store is not
// bounded by it, so a scratch workspace can be made at any schema the binary
// carries. [LAW:single-enforcer] the two refusals below are the only readers of
// the ceiling.

// UnreleasedMigrationError refuses a migrating open that would move an
// existing workspace to a schema above the released-schema ceiling.
type UnreleasedMigrationError struct {
	WorkspaceVersion int64
	TargetVersion    int64
	ReleasedSchema   int64
}

func (e *UnreleasedMigrationError) Error() string {
	return fmt.Sprintf(
		"refusing to migrate this workspace from schema version %d to %d: no published lit release carries a schema above %d, "+
			"and an existing workspace is never migrated to a schema no release carries",
		e.WorkspaceVersion, e.TargetVersion, e.ReleasedSchema)
}

// EventRecord is the schema the workspace is at, the one the open would have
// moved it to, and the ceiling that refused it.
func (e *UnreleasedMigrationError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"released_schema.workspace_schema": e.WorkspaceVersion,
		"released_schema.target_schema":    e.TargetVersion,
		"released_schema.ceiling":          e.ReleasedSchema,
	}, nil
}

// UnreleasedPushError refuses a push of a commit whose schema is above the
// released-schema ceiling, whatever the remote is.
type UnreleasedPushError struct {
	Remote         string
	Branch         string
	CommitSchema   int64
	ReleasedSchema int64
}

func (e *UnreleasedPushError) Error() string {
	return fmt.Sprintf(
		"refusing to push to %s/%s: the commit is at schema version %d, and no published lit release carries a schema above %d; "+
			"a commit at a schema no release carries is never sent to a remote",
		e.Remote, e.Branch, e.CommitSchema, e.ReleasedSchema)
}

// EventRecord is the remote refused, the commit's schema, and the ceiling.
func (e *UnreleasedPushError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"released_schema.remote":        e.Remote,
		"released_schema.branch":        e.Branch,
		"released_schema.commit_schema": e.CommitSchema,
		"released_schema.ceiling":       e.ReleasedSchema,
	}, nil
}

var (
	_ model.RecordedError = (*UnreleasedMigrationError)(nil)
	_ model.RecordedError = (*UnreleasedPushError)(nil)
)

// refuseUnreleasedMigration refuses a migration of an existing workspace whose
// target is above the released-schema ceiling. A fresh workspace is created,
// not migrated, and is not bounded.
func (s *Store) refuseUnreleasedMigration(state migrationState) error {
	if state.phase == phaseFresh || state.support.Max <= s.env.ReleasedSchema() {
		return nil
	}
	return &UnreleasedMigrationError{
		WorkspaceVersion: state.appliedVersion,
		TargetVersion:    state.support.Max,
		ReleasedSchema:   s.env.ReleasedSchema(),
	}
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
	if schema <= s.env.ReleasedSchema() {
		return nil
	}
	return &UnreleasedPushError{
		Remote:         remote.String(),
		Branch:         branch,
		CommitSchema:   schema,
		ReleasedSchema: s.env.ReleasedSchema(),
	}
}
