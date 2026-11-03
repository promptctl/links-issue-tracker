package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/promptctl/links-issue-tracker/internal/storage"
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
// a schema version: an integer at or above 1, the baseline, since no
// migration or hold is at 0 and 0 is what a reader reports for "none". A
// missing meta table (MySQL error 1146) is a store before the baseline, which
// no migration or downgrade has written a row into, the classification
// schemaVersionAtCommit gives a missing goose table; no row is no
// declaration; every other read error propagates. [LAW:no-silent-failure]
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
	if err != nil || v < 1 {
		return metaSchemaVersion{present: true, malformed: fmt.Errorf("meta %s = %q is not a schema version, an integer at or above 1", key, raw)}, nil
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
		Bound:  version.BoundHold,
	}, nil
}

// readSchemaHold is the working set's schema hold row as read.
func (s *Store) readSchemaHold(ctx context.Context) (metaSchemaVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT meta_value FROM meta WHERE meta_key = ?`, schemaHoldKey)
	return metaSchemaVersionRow(schemaHoldKey, row)
}

// schemaHold is the working set's schema hold as a ceiling: the live store's
// at a migrating open, and the fetched head's when the reconcile's scratch
// branch has been reset to it (resetAndLift).
func (s *Store) schemaHold(ctx context.Context) (version.SchemaCeiling, error) {
	read, err := s.readSchemaHold(ctx)
	if err != nil {
		return version.SchemaCeiling{}, err
	}
	return read.ceiling("this store")
}

// holdPolicy is what a downgrade does about the hold once its head is at the
// target: leave the store free to be raised again (holdNone, `lit downgrade
// --to`), or hold it at the target (holdAtTarget, `lit downgrade --remote`).
// [LAW:types-are-the-program] a closed enum, not a bool selecting a behavior.
type holdPolicy int

const (
	holdNone holdPolicy = iota
	holdAtTarget
)

// holdWorkingSet writes the hold a downgrade to target leaves the working set
// with, and reports that hold (0 for none) and whether it wrote: target under
// holdAtTarget; under holdNone, the hold the store already carries, moved
// down to target when it was above it, or none. A hold never sits above the
// head that carries it, which the lowering verdict (schema_lowering.go) would
// read as a head holding the remote at a schema it is not at; and a release
// downgrade never raises one, since a hold below the head (a binary from
// before the row raised the store past it) is the operator's hold and stays.
// A row already reading the hold is left as it is, so a repeated remote
// downgrade commits nothing; a row that is not a schema version is an error,
// as it is everywhere the hold is read. The caller commits.
// [LAW:single-enforcer] the one writer of meta.schema_hold; the lift
// (LiftSchemaHold) is its one remover. [LAW:types-are-the-program] the
// invariant lives in the one writer, not in every reader.
func (s *Store) holdWorkingSet(ctx context.Context, target int64, policy holdPolicy) (int64, bool, error) {
	read, err := s.readSchemaHold(ctx)
	if err != nil {
		return 0, false, err
	}
	if read.malformed != nil {
		return 0, false, read.malformed
	}
	if policy == holdNone && !read.present {
		return 0, false, nil
	}
	if policy == holdNone {
		target = min(target, read.version)
	}
	if read.present && read.version == target {
		return target, false, nil
	}
	if err := s.setMeta(ctx, nil, schemaHoldKey, strconv.FormatInt(target, 10)); err != nil {
		return 0, false, err
	}
	return target, true, nil
}

// LiftSchemaHold removes the store's schema hold in one commit and reports the
// hold it lifted. A store with no hold lifts nothing and commits nothing: the
// lift's caller pushes the head either way, so a lift whose push failed is run
// again without inventing a second commit. Writes a failed commit left in the
// working set are versioned first, as the downgrade does: the sync open runs
// no standing-writes gate, so a removal a crash left uncommitted would
// otherwise read as "no hold" while HEAD still carries it. A row that is not
// a schema version is an error, as it is everywhere the hold is read. Its one
// caller is `lit upgrade --remote`. [LAW:no-silent-failure]
// [LAW:single-enforcer] commitStrandedWrites is the one place a stranded
// write lands.
func (s *Store) LiftSchemaHold(ctx context.Context) (storage.SchemaHoldLifted, error) {
	var lifted storage.SchemaHoldLifted
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		if err := s.commitStrandedWrites(ctx, strandedWritesCommitMessage); err != nil {
			return err
		}
		read, err := s.readSchemaHold(ctx)
		if err != nil {
			return err
		}
		if read.malformed != nil {
			return read.malformed
		}
		if !read.present {
			return nil
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE meta_key = ?`, schemaHoldKey); err != nil {
			return fmt.Errorf("remove meta %s: %w", schemaHoldKey, err)
		}
		if err := s.commitWorkingSet(ctx, fmt.Sprintf("upgrade: lift the schema hold at %d", read.version)); err != nil {
			return fmt.Errorf("commit the lift of the schema hold at %d: %w", read.version, err)
		}
		lifted.Lifted = read.version
		return nil
	})
	return lifted, err
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
