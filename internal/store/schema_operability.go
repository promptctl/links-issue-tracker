package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// minWriterSchemaKey is the meta row a migration that changes a write rule
// sets to its own version: the oldest schema version a lit must support to
// write to the store. A lit whose registry stops below it reads the store but
// refuses to write, because its writes would not obey the newer rule — the
// read-only compatible feature of ext4, where a kernel that lacks the feature
// may mount the filesystem read-only. No row means no migration declared one.
// internal/store/migrations/README.md says how a migration declares it.
const minWriterSchemaKey = "min_writer_schema"

// operability is what a lit whose registry tops out at maxSupported may do
// with a workspace at workspaceSchema. A schema at or below maxSupported is one
// the lit's own registry produced, so nothing restricts it; one ahead of it is
// readable while its baseline shape is intact, and writable while the lit
// also supports the store's minimum writer schema.
type operability struct {
	workspaceSchema, maxSupported int64
	// missingBaseline names the baseline tables and columns the live schema
	// lacks, the shape every lit operates against.
	missingBaseline []string
	// minWriterSchema is the store's meta.min_writer_schema, 0 when no
	// migration set it. It is read only from a schema ahead of the lit with its
	// baseline intact: below that, no migration the lit lacks could have set it.
	minWriterSchema int64
}

func (o operability) ahead() bool { return o.workspaceSchema > o.maxSupported }

func (o operability) readable() bool { return len(o.missingBaseline) == 0 }

func (o operability) writable() bool {
	return o.readable() && o.minWriterSchema <= o.maxSupported
}

// schemaAhead is the newer schema an open served, as the open reports it; the
// zero value when the workspace was not ahead of this binary.
func (o operability) schemaAhead() storage.SchemaAhead {
	if !o.ahead() {
		return storage.SchemaAhead{}
	}
	return storage.SchemaAhead{WorkspaceSchema: o.workspaceSchema, MinWriterSchema: o.minWriterSchema}
}

// operability reads what decides whether a lit whose registry tops out at
// maxSupported may read and write this workspace, now at workspaceSchema.
// [LAW:single-enforcer] the open's refusal and the rollback search
// (snapshotOperable) both decide by this one rule, so the refusal never offers
// a snapshot it would refuse to write once restored.
func (s *Store) operability(ctx context.Context, workspaceSchema, maxSupported int64) (operability, error) {
	op := operability{workspaceSchema: workspaceSchema, maxSupported: maxSupported}
	if !op.ahead() {
		return op, nil
	}
	_, missing, err := s.verifyBaselineShape(ctx)
	if err != nil {
		return operability{}, err
	}
	op.missingBaseline = missing
	if !op.readable() {
		return op, nil
	}
	op.minWriterSchema, err = s.minWriterSchema(ctx)
	return op, err
}

// minWriterSchema parses meta.min_writer_schema. [LAW:parse-dont-validate] a
// row that is not a schema version fails here, loudly, rather than reading as
// no declaration.
func (s *Store) minWriterSchema(ctx context.Context) (int64, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT meta_value FROM meta WHERE meta_key = ?`, minWriterSchemaKey).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read meta %s: %w", minWriterSchemaKey, err)
	}
	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("meta %s = %q is not a schema version: %w", minWriterSchemaKey, raw, err)
	}
	return version, nil
}

// schemaAheadRefusal is the refusal of a workspace ahead of this binary that op
// says it may not use, with the newest migration snapshot it may use offered
// as a lossy rollback.
func (s *Store) schemaAheadRefusal(ctx context.Context, op operability) *UnsupportedSchemaVersionError {
	search := s.searchRollbackSnapshot(ctx, op.maxSupported)
	return &UnsupportedSchemaVersionError{
		WorkspaceVersion:    op.workspaceSchema,
		MaxSupported:        op.maxSupported,
		MissingBaseline:     op.missingBaseline,
		MinWriterSchema:     op.minWriterSchema,
		SnapshotName:        search.snapshot,
		SnapshotLookupErr:   search.err,
		SnapshotsExamined:   search.examined,
		SnapshotsUnreadable: search.unreadable,
	}
}
