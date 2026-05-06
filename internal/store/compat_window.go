package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// codeVersion is the highest schema migration version this binary
// understands. It is bumped together with the registration of any goose
// migration whose schema or data shape requires code that older binaries
// don't have.
//
// [LAW:one-source-of-truth] codeVersion and the registered goose migrations
// are the two writers of "what version range this binary supports". The
// compat-window gate (checkCompatWindow) is the runtime check that catches
// drift between them.
const codeVersion int64 = 1

// migrationMinCodeVersions declares the minimum binary codeVersion each
// migration's *schema or data shape* requires. A migration omitted from this
// map defaults to 1 — any binary that knows about goose at all can run it.
//
// Bumping a migration's min_code_version means: workspaces where this
// migration is applied cannot be opened by binaries with codeVersion below
// this number. The compat-window gate enforces that.
var migrationMinCodeVersions = map[int64]int64{
	// 1: 1 — baseline; default. Listed for documentation only.
}

// minCodeVersionFor returns the minimum binary codeVersion required to
// operate a workspace where the given migration version is applied.
// Defaults to 1 for migrations not explicitly registered.
func minCodeVersionFor(version int64) int64 {
	if v, ok := migrationMinCodeVersions[version]; ok {
		return v
	}
	return 1
}

// codeCompatFloorMetaKey is the meta row that tracks "the lowest binary
// codeVersion this workspace can be opened with". Advanced by
// advanceCompatFloor after a migration requiring newer code lands; read by
// checkCompatWindow on every Open.
const codeCompatFloorMetaKey = "code_compat_floor"

// CompatError signals that a workspace is outside the compat-window for the
// running binary. Callers can errors.As to read both the binary's view and
// the workspace's view, then act accordingly (upgrade the binary, restore
// the workspace, etc.).
type CompatError struct {
	// Reason is a short machine-stable string identifying which side of the
	// window the workspace fell off: "workspace_requires_newer_binary" or
	// "workspace_ahead_of_binary".
	Reason string
	// BinaryCodeVersion is this binary's codeVersion at the time of refusal.
	BinaryCodeVersion int64
	// WorkspaceCompatFloor is the workspace's recorded code_compat_floor
	// (zero if unset).
	WorkspaceCompatFloor int64
	// WorkspaceDBVersion is the workspace's highest applied goose
	// migration version (zero if goose_db_version is absent or empty).
	WorkspaceDBVersion int64
}

func (e *CompatError) Error() string {
	return fmt.Sprintf(
		"%s: binary code_version=%d, workspace code_compat_floor=%d, workspace goose_db_version=%d. "+
			"Upgrade the lit binary, or restore the workspace from a snapshot taken before the offending migration.",
		e.Reason, e.BinaryCodeVersion, e.WorkspaceCompatFloor, e.WorkspaceDBVersion,
	)
}

// checkCompatWindow refuses to proceed when the workspace is outside the
// running binary's supported version range. Read-only; runs at the top of
// s.migrate before adoption or any pending migration is applied so a refusal
// leaves the workspace untouched.
//
// [LAW:dataflow-not-control-flow] Always reads the same two values
// (code_compat_floor + goose_db_version MAX); only the comparison result
// varies. The refuse path returns a typed error; the happy path returns
// nil. Both paths execute the same operations.
func checkCompatWindow(ctx context.Context, db *sql.DB, binaryCodeVersion int64) error {
	floor, err := readCodeCompatFloor(ctx, db)
	if err != nil {
		return fmt.Errorf("read code_compat_floor: %w", err)
	}
	top, err := readGooseDBVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read goose_db_version: %w", err)
	}
	if floor > binaryCodeVersion {
		return &CompatError{
			Reason:               "workspace_requires_newer_binary",
			BinaryCodeVersion:    binaryCodeVersion,
			WorkspaceCompatFloor: floor,
			WorkspaceDBVersion:   top,
		}
	}
	if top > binaryCodeVersion {
		return &CompatError{
			Reason:               "workspace_ahead_of_binary",
			BinaryCodeVersion:    binaryCodeVersion,
			WorkspaceCompatFloor: floor,
			WorkspaceDBVersion:   top,
		}
	}
	return nil
}

// readCodeCompatFloor returns the workspace's recorded code_compat_floor
// from the meta table. Returns 0 if meta doesn't exist (fresh workspace
// before adoption/baseline runs) or the row is absent.
func readCodeCompatFloor(ctx context.Context, db *sql.DB) (int64, error) {
	metaExists, err := tableExists(ctx, db, "meta")
	if err != nil {
		return 0, err
	}
	if !metaExists {
		return 0, nil
	}
	var raw sql.NullString
	err = db.QueryRowContext(ctx,
		"SELECT meta_value FROM meta WHERE meta_key = ?",
		codeCompatFloorMetaKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw.String), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", codeCompatFloorMetaKey, raw.String, err)
	}
	return parsed, nil
}

// readGooseDBVersion returns the workspace's highest applied goose migration
// version. Returns 0 if goose_db_version is absent (fresh or pre-goose
// workspace) or contains no applied rows.
func readGooseDBVersion(ctx context.Context, db *sql.DB) (int64, error) {
	exists, err := tableExists(ctx, db, gooseVersionTable)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var top sql.NullInt64
	err = db.QueryRowContext(ctx,
		"SELECT MAX(version_id) FROM "+gooseVersionTable+" WHERE is_applied = TRUE").Scan(&top)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !top.Valid {
		return 0, nil
	}
	return top.Int64, nil
}

// advanceCompatFloor inspects the just-applied migration versions and
// advances meta.code_compat_floor to the highest min_code_version among
// them, when that exceeds the currently-recorded floor. Idempotent: a
// second call with the same versions makes no change.
//
// [LAW:single-enforcer] The only writer of meta.code_compat_floor.
func (s *Store) advanceCompatFloor(ctx context.Context, applied []int64) (bool, error) {
	if len(applied) == 0 {
		return false, nil
	}
	current, err := readCodeCompatFloor(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("read current code_compat_floor: %w", err)
	}
	target := current
	for _, version := range applied {
		if minimum := minCodeVersionFor(version); minimum > target {
			target = minimum
		}
	}
	if target == current {
		return false, nil
	}
	return s.ensureMetaValue(ctx, codeCompatFloorMetaKey, strconv.FormatInt(target, 10))
}
