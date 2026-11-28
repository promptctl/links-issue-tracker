package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// walkCTE is the one statement of which leaves an epic's state is read from,
// as two CTEs a statement opens with: descent, every issue below an epic
// through parent-child edges, and tally, each epic's leaves counted by state.
// Hydration and a selection that reads a state both read an epic's progress
// from it, so the two cannot disagree about an epic. [LAW:one-source-of-truth]
//
// An edge counts under the container rule: a live parent counts only its live
// children, and a parent out of the flow counts every child, so an archived
// epic keeps the state it left with.
//
// A parent loop gives an epic no state to derive. The walk carries the path it
// came down, stops where a child is already on it, and marks that row looped,
// so it ends on a loop rather than descending forever, and tally.loop_path spells
// one loop below each epic that reaches one: the path down to the repeating
// issue, which loopFrom names. [LAW:no-silent-failure]
//
// seed is the FROM clause the walk starts from, binding relations as r: every
// edge, or the edges below the epics a hydration names.
func walkCTE(seed string) string {
	return `WITH RECURSIVE descent (root, node, path, looped) AS (
		SELECT r.dst_id, r.src_id, CAST(CONCAT(',', r.dst_id, ',') AS CHAR(16000)), r.src_id = r.dst_id
		FROM ` + seed + `
		JOIN issues p ON p.id = r.dst_id JOIN issues c ON c.id = r.src_id
		WHERE r.type = 'parent-child' AND ` + countsUnder + `
		UNION ALL
		SELECT d.root, r.src_id, CONCAT(d.path, d.node, ','), LOCATE(CONCAT(',', r.src_id, ','), CONCAT(d.path, d.node, ',')) > 0
		FROM descent d
		JOIN relations r ON r.dst_id = d.node AND r.type = 'parent-child'
		JOIN issues p ON p.id = r.dst_id JOIN issues c ON c.id = r.src_id
		WHERE NOT d.looped AND ` + countsUnder + `
	), tally (id, open, started, closed, loop_path) AS (
		SELECT d.root,
			COALESCE(SUM(CASE WHEN n.status = 'open' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN n.status = 'in_progress' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN n.status = 'closed' THEN 1 ELSE 0 END), 0),
			MIN(CASE WHEN d.looped THEN CONCAT(d.path, d.node) END)
		FROM descent d JOIN issues n ON n.id = d.node
		GROUP BY d.root
	) `
}

// countsUnder is the container rule for the edge from parent p to child c.
const countsUnder = `(p.archived_at IS NOT NULL OR p.deleted_at IS NOT NULL OR (c.archived_at IS NULL AND c.deleted_at IS NULL))`

// everyEdge seeds the walk at every parent, for a statement that may read any
// epic's state.
const everyEdge = `relations r`

// loopFrom names the loop a tally.loop_path spells: the path down to an issue that
// was already on it. The loop is the stretch from that issue's first place on
// the path to the parent it was reached from, nearest first, each the child of
// the next, as storage.ParentCycleError names one.
func loopFrom(spelled string) storage.ParentCycleError {
	ids := strings.Split(strings.Trim(spelled, ","), ",")
	repeat := model.ID(ids[len(ids)-1])
	path := make([]model.ID, 0, len(ids)-1)
	for _, id := range ids[:len(ids)-1] {
		path = append(path, model.ID(id))
	}
	loop := slices.Clone(path[slices.Index(path, repeat):])
	slices.Reverse(loop)
	return storage.ParentCycleError{Cycle: loop}
}

// epicProgress reads the progress of each epic in epicIDs from the walk. An
// epic on or above a parent loop has none, and is refused.
func (s *Store) epicProgress(ctx context.Context, epicIDs []model.ID) (map[model.ID]model.Progress, error) {
	if len(epicIDs) == 0 {
		return map[model.ID]model.Progress{}, nil
	}
	seed, arg := newIDSet(epicIDs).rows("seed")
	return s.tallies(ctx, walkCTE(seed+` JOIN relations r ON r.dst_id = seed.id`)+`SELECT id, open, started, closed, loop_path FROM tally`, arg)
}

// admittedProgress is the progress of every epic the selection admits, an
// epic with no leaves at none: what a selection reading a state compares, and
// what its listing hydrates the epics it lists with. An admitted epic on or
// above a parent loop has no progress, and the read is refused.
func (s *Store) admittedProgress(ctx context.Context, selection storage.Selection) (map[model.ID]model.Progress, error) {
	admits := (&compiler{selection: selection}).admits("i")
	return s.tallies(ctx, walkCTE(everyEdge)+`SELECT i.id, COALESCE(t.open, 0), COALESCE(t.started, 0), COALESCE(t.closed, 0), t.loop_path
		FROM issues i LEFT JOIN tally t ON t.id = i.id
		WHERE `+admittedContainer+` AND `+admits.sql, admits.args...)
}

// admittedContainer is whether i is an epic, by the model's container types.
var admittedContainer = fmt.Sprintf("i.issue_type IN (%s)", quotedIssueTypeList(model.ContainerTypes()))

// tallies runs a statement over the walk's tally, columns id, open, started,
// closed and loop_path, as each epic's progress. A tallied loop refuses.
func (s *Store) tallies(ctx context.Context, query string, args ...any) (map[model.ID]model.Progress, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tally epic progress: %w", err)
	}
	defer rows.Close()
	out := map[model.ID]model.Progress{}
	for rows.Next() {
		var id model.ID
		var progress model.Progress
		var loop sql.NullString
		if err := rows.Scan(&id, &progress.Open, &progress.InProgress, &progress.Closed, &loop); err != nil {
			return nil, err
		}
		if loop.Valid {
			return nil, loopFrom(loop.String)
		}
		progress.Total = progress.Open + progress.InProgress + progress.Closed
		out[id] = progress
	}
	return out, rows.Err()
}
