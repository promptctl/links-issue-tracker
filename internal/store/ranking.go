package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/rank"
)

func (s *Store) RankToTop(ctx context.Context, issueID string) error {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rank-to-top tx: %w", err)
	}
	defer tx.Rollback()
	var firstRank sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND id != ? ORDER BY item_rank ASC LIMIT 1", issueID).Scan(&firstRank)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rank-to-top: query first: %w", err)
	}
	var newRank string
	if !firstRank.Valid || firstRank.String == "" {
		newRank = rank.Initial()
	} else {
		newRank = rank.Before(firstRank.String)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, issueID); err != nil {
		return fmt.Errorf("rank-to-top: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rank-to-top: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "rank to top"); err != nil {
		return err
	}
	return s.smoothRanksIfNeeded(ctx, newRank)
}

// RankToBottom moves an issue to rank below all other issues.
func (s *Store) RankToBottom(ctx context.Context, issueID string) error {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rank-to-bottom tx: %w", err)
	}
	defer tx.Rollback()
	var lastRank sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND id != ? ORDER BY item_rank DESC LIMIT 1", issueID).Scan(&lastRank)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rank-to-bottom: query last: %w", err)
	}
	var newRank string
	if !lastRank.Valid || lastRank.String == "" {
		newRank = rank.Initial()
	} else {
		newRank = rank.After(lastRank.String)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, issueID); err != nil {
		return fmt.Errorf("rank-to-bottom: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rank-to-bottom: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "rank to bottom"); err != nil {
		return err
	}
	return s.smoothRanksIfNeeded(ctx, newRank)
}

// RankAbove moves an issue to rank immediately above the target issue.
func (s *Store) RankAbove(ctx context.Context, issueID, targetID string) error {
	if issueID == targetID {
		return errors.New("cannot rank an issue relative to itself")
	}
	target, err := s.GetIssue(ctx, targetID)
	if err != nil {
		return err
	}
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rank-above tx: %w", err)
	}
	defer tx.Rollback()
	var aboveRank sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE item_rank < ? AND deleted_at IS NULL AND id != ? ORDER BY item_rank DESC LIMIT 1", target.Rank, issueID).Scan(&aboveRank)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rank-above: query neighbor: %w", err)
	}
	var newRank string
	if !aboveRank.Valid || aboveRank.String == "" {
		newRank = rank.Before(target.Rank)
	} else {
		newRank, err = rank.Midpoint(aboveRank.String, target.Rank)
		if err != nil {
			return fmt.Errorf("rank-above: midpoint: %w", err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, issueID); err != nil {
		return fmt.Errorf("rank-above: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rank-above: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "rank above"); err != nil {
		return err
	}
	return s.smoothRanksIfNeeded(ctx, newRank)
}

// RankBelow moves an issue to rank immediately below the target issue.
func (s *Store) RankBelow(ctx context.Context, issueID, targetID string) error {
	if issueID == targetID {
		return errors.New("cannot rank an issue relative to itself")
	}
	target, err := s.GetIssue(ctx, targetID)
	if err != nil {
		return err
	}
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rank-below tx: %w", err)
	}
	defer tx.Rollback()
	var belowRank sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE item_rank > ? AND deleted_at IS NULL AND id != ? ORDER BY item_rank ASC LIMIT 1", target.Rank, issueID).Scan(&belowRank)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rank-below: query neighbor: %w", err)
	}
	var newRank string
	if !belowRank.Valid || belowRank.String == "" {
		newRank = rank.After(target.Rank)
	} else {
		newRank, err = rank.Midpoint(target.Rank, belowRank.String)
		if err != nil {
			return fmt.Errorf("rank-below: midpoint: %w", err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, issueID); err != nil {
		return fmt.Errorf("rank-below: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rank-below: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "rank below"); err != nil {
		return err
	}
	return s.smoothRanksIfNeeded(ctx, newRank)
}

// smoothRanksIfNeeded checks whether the given rank string has grown past the
// smoothing threshold and, if so, re-spaces a local window of items around the
// insertion point. This keeps rank strings short with O(SmoothingWindow) cost
// instead of a full O(n) rebalance.
func smoothRanksIfNeededTx(ctx context.Context, tx *sql.Tx, triggerRank string) error {
	if len(triggerRank) < rank.SmoothingThreshold {
		return nil
	}
	half := rank.SmoothingWindow / 2

	// Collect the window: up to half items at or below the trigger, plus
	// up to half items above it.
	type ranked struct {
		id   string
		rank string
	}
	var window []ranked

	belowRows, err := tx.QueryContext(ctx,
		`SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank <= ? ORDER BY item_rank DESC LIMIT ?`,
		triggerRank, half)
	if err != nil {
		return fmt.Errorf("smooth: query below: %w", err)
	}
	var below []ranked
	for belowRows.Next() {
		var r ranked
		if err := belowRows.Scan(&r.id, &r.rank); err != nil {
			belowRows.Close()
			return fmt.Errorf("smooth: scan below: %w", err)
		}
		below = append(below, r)
	}
	belowRows.Close()
	if err := belowRows.Err(); err != nil {
		return fmt.Errorf("smooth: below rows: %w", err)
	}
	// Reverse below so it's in ascending order.
	for i, j := 0, len(below)-1; i < j; i, j = i+1, j-1 {
		below[i], below[j] = below[j], below[i]
	}
	window = append(window, below...)

	aboveRows, err := tx.QueryContext(ctx,
		`SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? ORDER BY item_rank ASC LIMIT ?`,
		triggerRank, half)
	if err != nil {
		return fmt.Errorf("smooth: query above: %w", err)
	}
	for aboveRows.Next() {
		var r ranked
		if err := aboveRows.Scan(&r.id, &r.rank); err != nil {
			aboveRows.Close()
			return fmt.Errorf("smooth: scan above: %w", err)
		}
		window = append(window, r)
	}
	aboveRows.Close()
	if err := aboveRows.Err(); err != nil {
		return fmt.Errorf("smooth: above rows: %w", err)
	}

	if len(window) < 2 {
		return nil
	}

	// Find the boundary ranks just outside the window.
	var lowerBound, upperBound string
	loRow := tx.QueryRowContext(ctx,
		`SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank < ? ORDER BY item_rank DESC LIMIT 1`,
		window[0].rank)
	var lb sql.NullString
	if err := loRow.Scan(&lb); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("smooth: lower bound: %w", err)
	}
	if lb.Valid {
		lowerBound = lb.String
	}

	hiRow := tx.QueryRowContext(ctx,
		`SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? ORDER BY item_rank ASC LIMIT 1`,
		window[len(window)-1].rank)
	var ub sql.NullString
	if err := hiRow.Scan(&ub); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("smooth: upper bound: %w", err)
	}
	if ub.Valid {
		upperBound = ub.String
	}

	newRanks, err := rank.SpacedRanksBetween(lowerBound, upperBound, len(window))
	if err != nil {
		return fmt.Errorf("smooth: compute ranks: %w", err)
	}

	for i, item := range window {
		if newRanks[i] != item.rank {
			if _, err := tx.ExecContext(ctx, `UPDATE issues SET item_rank = ? WHERE id = ?`, newRanks[i], item.id); err != nil {
				return fmt.Errorf("smooth: update %s: %w", item.id, err)
			}
		}
	}
	return nil
}

func (s *Store) smoothRanksIfNeeded(ctx context.Context, triggerRank string) error {
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin smooth tx: %w", err)
	}
	defer tx.Rollback()
	if err := smoothRanksIfNeededTx(ctx, tx, triggerRank); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit smooth: %w", err)
	}
	return s.commitWorkingSet(ctx, "smooth ranks")
}

// [LAW:one-source-of-truth] Rank inversion detection uses one shared relation clause for both counting and remediation.
const rankInversionsRelationClause = `FROM relations r
	JOIN issues src ON src.id = r.src_id
	JOIN issues dst ON dst.id = r.dst_id
	WHERE r.type = 'blocks'
	AND src.deleted_at IS NULL AND dst.deleted_at IS NULL
	AND src.status != 'closed' AND dst.status != 'closed'
	AND dst.item_rank > src.item_rank`

type rankInversion struct {
	depID       string // the dependency/blocker (should be ranked above)
	dependentID string // the dependent (src in blocks relation)
}

// FixRankInversions finds all blocks relations where the dependency is ranked
// below the dependent and ranks each dependency above its dependent. Returns
// the number of dependency issues that were re-ranked.
func (s *Store) FixRankInversions(ctx context.Context) (int, error) {
	loadInversions := func(ctx context.Context, tx *sql.Tx) ([]rankInversion, error) {
		// In blocks relations: src_id is the dependent, dst_id is the dependency (blocker).
		// A rank inversion is when the dependency (dst) is ranked below the dependent (src).
		rows, err := tx.QueryContext(ctx, `SELECT r.dst_id, r.src_id `+rankInversionsRelationClause+` ORDER BY src.item_rank ASC`)
		if err != nil {
			return nil, fmt.Errorf("query: %w", err)
		}
		defer rows.Close()
		inversions := make([]rankInversion, 0)
		for rows.Next() {
			var inv rankInversion
			if err := rows.Scan(&inv.depID, &inv.dependentID); err != nil {
				return nil, fmt.Errorf("scan: %w", err)
			}
			inversions = append(inversions, inv)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("rows: %w", err)
		}
		return inversions, nil
	}
	serializeInversions := func(inversions []rankInversion) string {
		parts := make([]string, 0, len(inversions))
		for _, inv := range inversions {
			parts = append(parts, inv.depID+"<-"+inv.dependentID)
		}
		return strings.Join(parts, "|")
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return 0, fmt.Errorf("fix rank inversions: acquire lock: %w", err)
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("fix rank inversions: begin tx: %w", err)
	}
	defer tx.Rollback()
	rerankedCount := 0
	seenSnapshots := map[string]struct{}{}
	for {
		inversions, err := loadInversions(ctx, tx)
		if err != nil {
			return 0, fmt.Errorf("fix rank inversions: %w", err)
		}
		if len(inversions) == 0 {
			break
		}
		snapshot := serializeInversions(inversions)
		if _, seen := seenSnapshots[snapshot]; seen {
			return 0, fmt.Errorf("fix rank inversions: unable to converge in one run; remaining inversions=%d", len(inversions))
		}
		seenSnapshots[snapshot] = struct{}{}

		// [LAW:dataflow-not-control-flow] Every pass applies one deterministic update per dependency;
		// selected target dependents come from ordered inversion data rather than branch-specific handling.
		targets := make([]rankInversion, 0, len(inversions))
		seenDeps := map[string]struct{}{}
		for _, inv := range inversions {
			if _, seen := seenDeps[inv.depID]; seen {
				continue
			}
			seenDeps[inv.depID] = struct{}{}
			targets = append(targets, inv)
		}
		for _, target := range targets {
			// Place the dependency just above the highest-priority dependent by
			// computing a rank between the dependent's predecessor and the dependent itself.
			var targetRank string
			if err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE id = ?", target.dependentID).Scan(&targetRank); err != nil {
				return 0, fmt.Errorf("fix rank inversions: read target rank %s: %w", target.dependentID, err)
			}
			var aboveRank sql.NullString
			err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE item_rank < ? AND deleted_at IS NULL AND id != ? ORDER BY item_rank DESC LIMIT 1", targetRank, target.depID).Scan(&aboveRank)
			if err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return 0, fmt.Errorf("fix rank inversions: query neighbor: %w", err)
				}
			}
			var newRank string
			if !aboveRank.Valid || aboveRank.String == "" {
				newRank = rank.Before(targetRank)
			} else {
				newRank, err = rank.Midpoint(aboveRank.String, targetRank)
				if err != nil {
					return 0, fmt.Errorf("fix rank inversions: midpoint: %w", err)
				}
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, target.depID); err != nil {
				return 0, fmt.Errorf("fix rank inversions: update %s: %w", target.depID, err)
			}
			if err := smoothRanksIfNeededTx(ctx, tx, newRank); err != nil {
				return 0, fmt.Errorf("fix rank inversions: smooth ranks: %w", err)
			}
			rerankedCount++
		}
	}
	if rerankedCount == 0 {
		return 0, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("fix rank inversions: commit: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "fix rank inversions"); err != nil {
		return 0, err
	}
	return rerankedCount, nil
}
