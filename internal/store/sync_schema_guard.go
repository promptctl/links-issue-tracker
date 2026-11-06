package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	embedded "github.com/dolthub/driver"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// RemoteSchemaAheadError reports that the remote head a reconcile would replay
// onto records a schema version above this binary's schema support. The
// replay re-creates every row a folded commit changed from this binary's own
// columns (export_delta.go: a changed row is a delete and an insert, never an
// UPDATE), so onto that head it would reset the columns the newer schema added
// to their defaults on every row it touched: a regression within the remote's
// schema, which no comparison of the sent head's schema to the remote's would
// see. The open's admission of a write at that schema does not carry here,
// because the write the open admits is the model's column-named UPDATE, which
// leaves the columns it does not name as they were; a push of a head written
// that way lands at the remote's schema and runs no such refusal
// (design-docs/schema-reversibility.md, "A push at the remote's own schema
// lands"). The lossless fix is a binary whose schema support reaches the
// remote's version, so the error states that requirement and routes to `lit
// upgrade`. It does not name the binary that authored the remote head: a
// build identity can be unresolvable, and when the message is replayed later
// it can be older than the binary the reader is already running.
//
// [LAW:types-are-the-program] The refusal is version arithmetic on data —
// (RemoteVersion, BinarySupportedMax) — never inferred from a query happening to
// succeed or fail on a column. It is the REMOTE mirror of
// UnsupportedSchemaVersionError (the LOCAL workspace-ahead refusal): each names the
// same remedy, install the newer binary, for the boundary it guards, and both are
// model.SchemaAhead, so the CLI gives them one exit code, one reason and one
// install-gating policy. [LAW:one-type-per-behavior]
type RemoteSchemaAheadError struct {
	Remote             string
	Branch             string
	RemoteVersion      int64
	BinarySupportedMax int64
}

func (e *RemoteSchemaAheadError) Error() string {
	return fmt.Sprintf(
		"remote %s/%s is at schema version %d but this binary supports only up to %d; "+
			"refusing to reconcile onto its head, since the replay would re-create every row it changes "+
			"from this binary's columns and reset the newer schema's fields — run `lit upgrade` "+
			"to install a lit that supports schema version %d",
		e.Remote, e.Branch, e.RemoteVersion, e.BinarySupportedMax, e.RemoteVersion,
	)
}

// SchemaAheadOfBinary: no rerun of this binary reconciles onto the remote
// head; only a lit whose schema support reaches the remote head's can.
func (*RemoteSchemaAheadError) SchemaAheadOfBinary() {}

// EventRecord is the remote that refused and both schema versions, so an event
// whose reason is workspace_schema_ahead says which boundary refused it.
func (e *RemoteSchemaAheadError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"schema_ahead.remote":             e.Remote,
		"schema_ahead.branch":             e.Branch,
		"schema_ahead.remote_schema":      e.RemoteVersion,
		"schema_ahead.running_schema_max": e.BinarySupportedMax,
	}, nil
}

var (
	_ model.SchemaAhead   = (*RemoteSchemaAheadError)(nil)
	_ model.RecordedError = (*RemoteSchemaAheadError)(nil)
)

// trackingHeadSchema is the schema and schema hold of the head
// `remotes/<remote>/<branch>` points at, or unknownRemoteSchema when that ref
// does not exist yet (the branch was never fetched or pushed). It is the one
// read of R a push makes: the ceiling, raise and lowering refusals all judge
// the value it returns. [LAW:one-source-of-truth]
func (s *Store) trackingHeadSchema(ctx context.Context, remote storage.RemoteName, branch string) (remoteSchema, error) {
	head, synced, err := s.trackingHeadHash(ctx, remote, branch)
	if err != nil {
		return remoteSchema{}, err
	}
	if !synced {
		return unknownRemoteSchema(), nil
	}
	return s.remoteHeadAt(ctx, head, remote, branch)
}

// freshnessHeadSchema is the schema and schema hold of the head a freshness
// judged (SyncFreshness.RemoteHead), or unknownRemoteSchema when the branch
// never synced. A settle and a reconcile plan read the remote through it, so
// the head whose freshness they report and the head whose schema they report
// are one read of the tracking ref: a diverged plan's remote head is the
// commit its schema was read AS OF, never a second resolution of the ref.
// [LAW:one-source-of-truth] [LAW:no-ambient-temporal-coupling]
func (s *Store) freshnessHeadSchema(ctx context.Context, fresh storage.SyncFreshness, remote storage.RemoteName, branch string) (remoteSchema, error) {
	if !fresh.Synced {
		return unknownRemoteSchema(), nil
	}
	return s.remoteHeadAt(ctx, fresh.RemoteHead, remote, branch)
}

// judgeSchemaAhead is the schema-ahead verdict on a remote head's schema the
// caller read: the head's schema against this binary's schema support, zero
// when the head is within it. A remote this store holds no head of is not
// ahead. Every settle reports it, and the reconcile's replay refuses on it
// (refuseSchemaAhead), so the owner channel ends the episode on the same
// judgement the guard raised it from. [LAW:single-enforcer] the one judgement
// of "remote schema ahead".
func judgeSchemaAhead(remoteHead remoteSchema) (storage.RemoteSchemaAhead, error) {
	if !remoteHead.known {
		return storage.RemoteSchemaAhead{}, nil
	}
	support, err := version.CarriedSchemaSupport()
	if err != nil {
		return storage.RemoteSchemaAhead{}, err
	}
	// [LAW:dataflow-not-control-flow] The comparison runs every call; the two
	// versions are the values that decide, not a mode.
	if remoteHead.version <= support.Max {
		return storage.RemoteSchemaAhead{}, nil
	}
	return storage.RemoteSchemaAhead{RemoteSchema: remoteHead.version, BinarySupportedMax: support.Max}, nil
}

// refuseSchemaAhead refuses a reconcile's replay onto remote/branch whose
// head is at a schema above this binary's support, before any write
// (replayUnderGuard: the three-way reconcile, combine, and take-local, which
// all author replay commits on that head), on the plan's captured read of
// it. It is the reconcile's refusal alone. A push runs none: a head at or
// below R regresses nothing (the lowering verdict refuses an unheld head
// below R), and the open decided what this binary may write. A landing (the
// behind fast-forward, a take-remote with nothing to carry) authors no commit
// and runs none either: it adopts the head at any schema, and the open
// decides what this binary may do with the store it finds. remote and branch
// are carried only to name the failure for the sync-failure contract.
func refuseSchemaAhead(remote storage.RemoteName, branch string, remoteHead remoteSchema) error {
	ahead, err := judgeSchemaAhead(remoteHead)
	if err != nil || !ahead.Ahead() {
		return err
	}
	return &RemoteSchemaAheadError{
		Remote:             remote.String(),
		Branch:             branch,
		RemoteVersion:      ahead.RemoteSchema,
		BinarySupportedMax: ahead.BinarySupportedMax,
	}
}

// trackingHeadHash returns the commit hash the remote-tracking ref
// `remotes/<remote>/<branch>` points at, or synced=false when that ref does not
// exist yet (the branch has never been fetched/pushed). It mirrors SyncFreshness's
// ref-existence probe so the guard and the freshness read agree on "never synced".
func (s *Store) trackingHeadHash(ctx context.Context, remote storage.RemoteName, branch string) (hash string, synced bool, err error) {
	trackingRef := fmt.Sprintf("remotes/%s/%s", remote, branch)
	var count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dolt_remote_branches WHERE name = ?`, trackingRef,
	).Scan(&count); err != nil {
		return "", false, fmt.Errorf("check remote-tracking ref %q: %w", trackingRef, err)
	}
	if count == 0 {
		return "", false, nil
	}
	head, err := commitHashOfRef(ctx, s.db, trackingRef)
	if err != nil {
		return "", false, err
	}
	return head, true, nil
}

// remoteHeadSchema reads the schema version recorded at a Dolt commit as raw
// handshake DATA. It reads AS OF the commit hash so no branch moves and nothing is
// lifted — lifting an ahead commit is exactly the schema regression this guard
// exists to prevent, so the read must never trigger it.
// [LAW:effects-at-boundaries] a pure read. [LAW:one-source-of-truth] MAX(version_id)
// is goose's own mysql-dialect definition of the applied version, not a second one.
func (s *Store) remoteHeadSchema(ctx context.Context, commitHash string) (version int64, err error) {
	if !isDoltCommitHash(commitHash) {
		// [LAW:no-silent-failure] AS OF takes a literal, not a bound parameter, so
		// the hash is interpolated; a value that is not a Dolt hash must never reach
		// the query text. commitHashOfRef only ever yields a real hash, so this
		// fires only on a caller bug — loudly, not by interpolating something unsafe.
		return 0, fmt.Errorf("remote head schema: %q is not a Dolt commit hash", commitHash)
	}
	return s.schemaVersionAtCommit(ctx, commitHash)
}

// schemaVersionAtCommit reads goose's applied schema version at a commit. A commit
// whose head predates goose has no goose_db_version table — Dolt raises MySQL error
// 1146 — which is a pre-goose remote at schema 0, never ahead, classified as such
// rather than surfaced as a failure. An empty goose table (NULL max) is likewise
// schema 0. Every other read error propagates. [LAW:no-silent-failure]
func (s *Store) schemaVersionAtCommit(ctx context.Context, commitHash string) (int64, error) {
	query := fmt.Sprintf(`SELECT MAX(version_id) FROM goose_db_version AS OF '%s'`, commitHash)
	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, query).Scan(&version); err != nil {
		if isMissingTableError(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read schema version at %q: %w", commitHash, err)
	}
	if !version.Valid {
		return 0, nil
	}
	return version.Int64, nil
}

// isMissingTableError reports whether a query failed because the table does not
// exist at the queried revision (MySQL error 1146). The embedded Dolt driver
// re-wraps engine errors into its own *embedded.MySQLError, so the number is
// matched on that typed error rather than on the message text, which varies with
// server version and locale. [LAW:types-are-the-program]
func isMissingTableError(err error) bool {
	var mysqlErr *embedded.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1146
}

// isDoltCommitHash reports whether s is a Dolt content hash: 32 characters over
// Dolt's base32 alphabet (0-9, a-v). Only such a value is safe to interpolate into
// the AS OF literal, which cannot take a bound parameter. [LAW:types-are-the-program]
// the predicate is the exact shape Dolt produces, so nothing else reaches the query.
func isDoltCommitHash(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'v') {
			continue
		}
		return false
	}
	return true
}
