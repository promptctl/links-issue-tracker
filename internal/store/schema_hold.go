package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// The schema hold is the meta row `lit downgrade --remote` writes beside a
// lowered head, meta.schema_hold = v: the remote hold of
// design-docs/schema-reversibility.md, "Lowering a remote, and making it
// stay lowered". A binary that reads it migrates no store past v, lifts no
// fetched head past v, and pushes no head above v. It enters each of those
// sites as a version.SchemaCeiling lowering the released-schema ceiling, so
// the three refusals that bind a raise today bind it to the hold as well, and
// a store with no row yields the unbounded ceiling, which lowers nothing.
// [LAW:single-enforcer] [LAW:dataflow-not-control-flow]
const schemaHoldKey = "schema_hold"

// metaSchemaVersion is one meta row read as a schema version: the version it
// declares when present and well formed; malformed, why a present row is not a
// schema version. A malformed row is kept as what it is, never read as no
// declaration. [LAW:parse-dont-validate]
type metaSchemaVersion struct {
	version   int64
	present   bool
	malformed error
}

// metaSchemaVersionRow reads the row the caller selected from meta for key as
// a schema version. A missing meta table (MySQL error 1146) is a store before
// the baseline, which no migration or downgrade has written a row into, the
// classification schemaVersionAtCommit gives a missing goose table; no row is
// no declaration; every other read error propagates. [LAW:no-silent-failure]
// It is the one reader of meta's schema-version rows: the minimum writer
// schema (schema_operability.go) and the schema hold differ only in what the
// caller does with the value. [LAW:one-type-per-behavior]
func metaSchemaVersionRow(key string, row *sql.Row) (metaSchemaVersion, error) {
	var raw string
	err := row.Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows), isMissingTableError(err):
		return metaSchemaVersion{}, nil
	case err != nil:
		return metaSchemaVersion{}, fmt.Errorf("read meta %s: %w", key, err)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return metaSchemaVersion{present: true, malformed: fmt.Errorf("meta %s = %q is not a schema version: %w", key, raw, err)}, nil
	}
	return metaSchemaVersion{version: v, present: true}, nil
}

// ceiling is the row as the ceiling it sets on a raise: the declared version,
// held by holder, or the unbounded ceiling when no row declares one. A
// malformed row is an error: a hold that cannot be read holds nothing
// knowable, and a raise past it must not land on that account.
// [LAW:no-silent-failure] [LAW:types-are-the-program]
func (m metaSchemaVersion) ceiling(holder string) (version.SchemaCeiling, error) {
	if m.malformed != nil {
		return version.SchemaCeiling{}, m.malformed
	}
	if !m.present {
		return version.UnboundedSchemaCeiling(), nil
	}
	return version.SchemaCeiling{
		Max:    m.version,
		Source: fmt.Sprintf("%s holds its schema at %d (meta.%s), which a remote downgrade set", holder, m.version, schemaHoldKey),
	}, nil
}

// schemaHold is the working set's schema hold as a ceiling: the live store's
// at a migrating open, and the fetched head's when the reconcile's scratch
// branch has been reset to it (resetAndLift).
func (s *Store) schemaHold(ctx context.Context) (version.SchemaCeiling, error) {
	row := s.db.QueryRowContext(ctx, `SELECT meta_value FROM meta WHERE meta_key = ?`, schemaHoldKey)
	read, err := metaSchemaVersionRow(schemaHoldKey, row)
	if err != nil {
		return version.SchemaCeiling{}, err
	}
	return read.ceiling("this store")
}

// schemaHoldAt is the schema hold recorded at a commit, as a ceiling held by
// the head the caller names (holder, as "origin/master's head"). It reads AS
// OF the commit hash, as remoteHeadSchema reads the schema there, so the push
// judges R's hold and R's schema at one pinned commit.
// [LAW:no-ambient-temporal-coupling] [LAW:one-source-of-truth]
func (s *Store) schemaHoldAt(ctx context.Context, commitHash string, holder string) (version.SchemaCeiling, error) {
	if !isDoltCommitHash(commitHash) {
		// [LAW:no-silent-failure] AS OF takes a literal, so the hash is
		// interpolated; only a value of Dolt's hash shape reaches the query.
		return version.SchemaCeiling{}, fmt.Errorf("schema hold at %q: not a Dolt commit hash", commitHash)
	}
	query := fmt.Sprintf(`SELECT meta_value FROM meta AS OF '%s' WHERE meta_key = ?`, commitHash)
	read, err := metaSchemaVersionRow(schemaHoldKey, s.db.QueryRowContext(ctx, query, schemaHoldKey))
	if err != nil {
		return version.SchemaCeiling{}, err
	}
	return read.ceiling(holder)
}
