package store

import (
	"context"
	"database/sql"
	"fmt"
)

// unrelatedInventory reads the issue-id set at each of the two unrelated heads and
// partitions them by presence. It reads AS OF each commit hash — a pure read that
// moves no branch and lifts no schema — so it preserves the detection's no-write
// guarantee: showing what each side holds must never mutate either side, exactly
// as the reconcile classifies SyncReconcileUnrelated before any scratch, snapshot,
// or reset. [LAW:effects-at-boundaries] the two reads are the only effect; the
// partition itself is pure set arithmetic.
func (s *Store) unrelatedInventory(ctx context.Context, localHead, remoteHead string) (*UnrelatedInventory, error) {
	local, err := issueIDsAtCommit(ctx, s.db, localHead)
	if err != nil {
		return nil, fmt.Errorf("read local issue inventory: %w", err)
	}
	remote, err := issueIDsAtCommit(ctx, s.db, remoteHead)
	if err != nil {
		return nil, fmt.Errorf("read remote issue inventory: %w", err)
	}
	return &UnrelatedInventory{
		OnlyLocal:  setDifference(local, remote),
		OnlyRemote: setDifference(remote, local),
		OnBoth:     setIntersection(local, remote),
	}, nil
}

// issueIDsAtCommit reads the set of issue ids present in the issues table at a
// commit, AS OF the commit hash so no branch moves. A commit whose head predates
// the baseline migration has no issues table (Dolt raises MySQL error 1146) — a
// pristine bootstrap root that genuinely holds no issues — reported as the empty
// set rather than surfaced as a failure, exactly as LocalIssueCount treats an
// absent table. [LAW:no-defensive-null-guards] the absent table is a real domain
// value (a side with no issues), matched here at the backend boundary.
func issueIDsAtCommit(ctx context.Context, db *sql.DB, commitHash string) (map[string]bool, error) {
	if !isDoltCommitHash(commitHash) {
		// [LAW:no-silent-failure] AS OF takes a literal, not a bound parameter, so the
		// hash is interpolated; a value that is not a Dolt hash must never reach the
		// query text. The heads come from readDoltHead/commitHashOfRef, which only ever
		// yield real hashes, so this fires only on a caller bug — loudly.
		return nil, fmt.Errorf("issue inventory: %q is not a Dolt commit hash", commitHash)
	}
	query := fmt.Sprintf(`SELECT id FROM issues AS OF '%s'`, commitHash)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		if isMissingTableError(err) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("read issue ids at %q: %w", commitHash, err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan issue id at %q: %w", commitHash, err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue ids at %q: %w", commitHash, err)
	}
	return ids, nil
}
