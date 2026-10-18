package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// unrelatedInventory reads each unrelated head's issues and purge records and
// partitions the issues: those a purge record on either head names, which every
// resolution drops, and of the rest, ids only the local head carries, ids only
// the remote head carries, and ids both carry. It reads AS OF each commit hash —
// a pure read that moves no branch and lifts no schema — so it preserves the
// detection's no-write guarantee: showing what each side holds must never mutate
// either side, exactly as the reconcile classifies SyncReconcileUnrelated before
// any scratch, snapshot, or reset. [LAW:effects-at-boundaries] the reads are the
// only effect; the partition itself is pure set arithmetic.
func (s *Store) unrelatedInventory(ctx context.Context, localHead, remoteHead string) (*storage.UnrelatedInventory, error) {
	local, err := issuesAtCommit(ctx, s.db, localHead)
	if err != nil {
		return nil, fmt.Errorf("read local issue inventory: %w", err)
	}
	remote, err := issuesAtCommit(ctx, s.db, remoteHead)
	if err != nil {
		return nil, fmt.Errorf("read remote issue inventory: %w", err)
	}
	localPurges, err := purgesAtCommit(ctx, s.db, localHead)
	if err != nil {
		return nil, fmt.Errorf("read local purge records: %w", err)
	}
	remotePurges, err := purgesAtCommit(ctx, s.db, remoteHead)
	if err != nil {
		return nil, fmt.Errorf("read remote purge records: %w", err)
	}
	// [LAW:single-enforcer] "this record purged that ticket" is Purge.Names,
	// the test every merge and restore applies.
	records := model.UnionPurges(localPurges, remotePurges)
	purged := map[string]bool{}
	for _, held := range []map[string]model.Issue{local, remote} {
		for id, issue := range held {
			if slices.ContainsFunc(records, func(p model.Purge) bool { return p.Names(issue) }) {
				purged[id] = true
			}
		}
	}
	kept := func(held map[string]model.Issue) map[string]bool {
		ids := map[string]bool{}
		for id := range held {
			if !purged[id] {
				ids[id] = true
			}
		}
		return ids
	}
	localIDs, remoteIDs := kept(local), kept(remote)
	return &storage.UnrelatedInventory{
		OnlyLocal:  setDifference(localIDs, remoteIDs),
		OnlyRemote: setDifference(remoteIDs, localIDs),
		OnBoth:     setIntersection(localIDs, remoteIDs),
		Purged:     slices.Sorted(maps.Keys(purged)),
	}, nil
}

// queryAsOf runs a read against the tables as they stood at a commit — AS OF the
// commit hash, so no branch moves — and hands each row to scan. A table the
// commit predates (Dolt raises MySQL error 1146) holds no rows: a commit from
// before the baseline holds no issues, and one from before migration v9 holds no
// purge records, so neither is surfaced as a failure, exactly as
// LocalIssueCount treats an absent table. [LAW:no-defensive-null-guards] the
// absent table is a real domain value, matched here at the backend boundary.
// queryFormat carries one %s, where the hash goes.
func queryAsOf(ctx context.Context, db *sql.DB, commitHash, queryFormat string, scan func(*sql.Rows) error) error {
	if !isDoltCommitHash(commitHash) {
		// [LAW:no-silent-failure] AS OF takes a literal, not a bound parameter, so the
		// hash is interpolated; a value that is not a Dolt hash must never reach the
		// query text. The heads come from readDoltHead/commitHashOfRef, which only ever
		// yield real hashes, so this fires only on a caller bug — loudly.
		return fmt.Errorf("read as of commit: %q is not a Dolt commit hash", commitHash)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(queryFormat, commitHash))
	if err != nil {
		if isMissingTableError(err) {
			return nil
		}
		return fmt.Errorf("read at %q: %w", commitHash, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("scan at %q: %w", commitHash, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate at %q: %w", commitHash, err)
	}
	return nil
}

// issuesAtCommit reads the issues present at a commit, by id, each carrying its
// id and birth — the two fields a purge record names a ticket by.
func issuesAtCommit(ctx context.Context, db *sql.DB, commitHash string) (map[string]model.Issue, error) {
	issues := map[string]model.Issue{}
	err := queryAsOf(ctx, db, commitHash, `SELECT id, created_at FROM issues AS OF '%s'`, func(rows *sql.Rows) error {
		var issue model.Issue
		var createdAt string
		if err := rows.Scan(&issue.ID, &createdAt); err != nil {
			return err
		}
		born, err := scanTime(createdAt)
		if err != nil {
			return err
		}
		issue.CreatedAt = born
		issues[issue.ID] = issue
		return nil
	})
	return issues, err
}

// eventTicketsAtCommit reads the ticket events present at a commit: each
// event's id, mapped to the id of the ticket it records.
func eventTicketsAtCommit(ctx context.Context, db *sql.DB, commitHash string) (map[string]string, error) {
	events := map[string]string{}
	err := queryAsOf(ctx, db, commitHash, `SELECT id, issue_id FROM issue_events AS OF '%s'`, func(rows *sql.Rows) error {
		var id, ticketID string
		if err := rows.Scan(&id, &ticketID); err != nil {
			return err
		}
		events[id] = ticketID
		return nil
	})
	return events, err
}

// purgesAtCommit reads the purge records present at a commit.
func purgesAtCommit(ctx context.Context, db *sql.DB, commitHash string) ([]model.Purge, error) {
	var out []model.Purge
	err := queryAsOf(ctx, db, commitHash, `SELECT issue_id, issue_created_at, purged_at, purged_by FROM purges AS OF '%s'`, func(rows *sql.Rows) error {
		p, err := scanPurge(rows)
		if err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	return out, err
}
