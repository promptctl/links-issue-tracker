package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	drivermysql "github.com/go-sql-driver/mysql"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// RemoteSchemaAheadError reports that the remote-tracking head records a schema
// version this binary cannot produce — its applied version is above this binary's
// registry max. Writing here (a push, or the reconcile's replay commit) would
// author a commit BELOW the remote head's schema: this binary knows only its own
// older columns, so it would regress the shared remote to a schema it understands
// and drop every field the newer schema added. That is the exact 2026-07-08
// incident. The lossless fix is to run the newer binary, so the error names the
// remote head's producer version for `lit upgrade --to <it>`.
//
// [LAW:types-are-the-program] The refusal is version arithmetic on data —
// (RemoteVersion, BinarySupportedMax, RemoteProducerVersion) — never inferred from
// a query happening to succeed or fail on a column. It is the REMOTE mirror of
// UnsupportedSchemaVersionError (the LOCAL workspace-ahead refusal): each names the
// same remedy, install the newer binary, for the boundary it guards.
// [LAW:one-type-per-behavior] one refusal shape per boundary, both routing to
// `lit upgrade`.
type RemoteSchemaAheadError struct {
	Remote                string
	Branch                string
	RemoteVersion         int64
	BinarySupportedMax    int64
	RemoteProducerVersion string // "" when the remote head records no producer stamp
}

func (e *RemoteSchemaAheadError) Error() string {
	ref := e.Remote + "/" + e.Branch
	var b strings.Builder
	fmt.Fprintf(&b,
		"remote %s is at schema version %d but this binary supports only up to %d; refusing to write a commit below the remote head's schema",
		ref, e.RemoteVersion, e.BinarySupportedMax,
	)
	// [LAW:dataflow-not-control-flow] Same renderer every call; the populated
	// producer field decides whether the upgrade line names a concrete target.
	if e.RemoteProducerVersion != "" {
		fmt.Fprintf(&b, " — run `lit upgrade --to %s`", e.RemoteProducerVersion)
	} else {
		b.WriteString(" — upgrade lit to a version that supports this schema")
	}
	return b.String()
}

// guardRemoteSchemaAhead refuses when the remote-tracking head's schema exceeds
// this binary's registry max, so no push authors a commit below the remote head's
// schema. It resolves the remote head from the tracking ref that reflects the
// remote as of the last fetch; a branch that never synced (no tracking ref) has no
// remote head to fall behind, so the guard is a no-op there. [LAW:single-enforcer]
// the push and the reconcile share this one detection of "remote schema ahead".
func (s *Store) guardRemoteSchemaAhead(ctx context.Context, remote, branch string) error {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return err
	}
	head, synced, err := s.trackingHeadHash(ctx, trimmedRemote, trimmedBranch)
	if err != nil {
		return err
	}
	if !synced {
		return nil
	}
	return s.guardCommitSchemaAhead(ctx, trimmedRemote, trimmedBranch, head)
}

// guardCommitSchemaAhead is the shared core: it reads the schema version recorded
// at commitHash as data and refuses when it is above this binary's registry max.
// The reconcile calls it with the remote head anchor it already captured (no second
// read); the push calls it via guardRemoteSchemaAhead after resolving the tracking
// head. [LAW:no-ambient-temporal-coupling] the version is read from a fixed commit
// hash, so the decision cannot shift under a concurrent fetch. remote and branch
// are carried only to name the failure for the sync-failure contract.
func (s *Store) guardCommitSchemaAhead(ctx context.Context, remote, branch, commitHash string) error {
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		return err
	}
	remoteVersion, producer, err := s.remoteHeadSchema(ctx, commitHash)
	if err != nil {
		return err
	}
	// [LAW:dataflow-not-control-flow] The comparison runs every call; the two
	// versions are the values that decide block-vs-proceed, not a mode.
	if remoteVersion <= registryMax {
		return nil
	}
	return &RemoteSchemaAheadError{
		Remote:                remote,
		Branch:                branch,
		RemoteVersion:         remoteVersion,
		BinarySupportedMax:    registryMax,
		RemoteProducerVersion: producer,
	}
}

// trackingHeadHash returns the commit hash the remote-tracking ref
// `remotes/<remote>/<branch>` points at, or synced=false when that ref does not
// exist yet (the branch has never been fetched/pushed). It mirrors SyncFreshness's
// ref-existence probe so the guard and the freshness read agree on "never synced".
func (s *Store) trackingHeadHash(ctx context.Context, remote, branch string) (hash string, synced bool, err error) {
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

// remoteHeadSchema reads the schema version and producer binary version recorded at
// a Dolt commit as raw handshake DATA. It reads AS OF the commit hash so no branch
// moves and nothing is lifted — lifting an ahead commit is exactly the schema
// regression this guard exists to prevent, so the read must never trigger it.
// [LAW:effects-at-boundaries] a pure read. [LAW:one-source-of-truth] MAX(version_id)
// is goose's own mysql-dialect definition of the applied version, not a second one.
func (s *Store) remoteHeadSchema(ctx context.Context, commitHash string) (version int64, producer string, err error) {
	if !isDoltCommitHash(commitHash) {
		// [LAW:no-silent-failure] AS OF takes a literal, not a bound parameter, so
		// the hash is interpolated; a value that is not a Dolt hash must never reach
		// the query text. commitHashOfRef only ever yields a real hash, so this
		// fires only on a caller bug — loudly, not by interpolating something unsafe.
		return 0, "", fmt.Errorf("remote head schema: %q is not a Dolt commit hash", commitHash)
	}
	version, err = s.schemaVersionAtCommit(ctx, commitHash)
	if err != nil {
		return 0, "", err
	}
	producer, err = s.producerVersionAtCommit(ctx, commitHash)
	if err != nil {
		return 0, "", err
	}
	return version, producer, nil
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

// producerVersionAtCommit reads the producer binary version stamped at a commit,
// or "" when the commit records none (an older workspace, a recovery path that
// bypassed the migrate tail) or predates the meta table. The empty string is a real
// domain value — "no producer to name" — which the sync-failure contract renders as
// a generic upgrade instruction rather than a specific `--to` target.
func (s *Store) producerVersionAtCommit(ctx context.Context, commitHash string) (string, error) {
	query := fmt.Sprintf(`SELECT meta_value FROM meta AS OF '%s' WHERE meta_key = ?`, commitHash)
	var value sql.NullString
	if err := s.db.QueryRowContext(ctx, query, producerBinaryVersionMetaKey).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) || isMissingTableError(err) {
			return "", nil
		}
		return "", fmt.Errorf("read producer version at %q: %w", commitHash, err)
	}
	return strings.TrimSpace(value.String), nil
}

// isMissingTableError reports whether a query failed because the table does not
// exist at the queried revision (MySQL error 1146). The embedded Dolt driver
// re-wraps engine errors into *mysql.MySQLError, so the number is matched on that
// typed error rather than on the message text. [LAW:types-are-the-program]
func isMissingTableError(err error) bool {
	var mysqlErr *drivermysql.MySQLError
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
