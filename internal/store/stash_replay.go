package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// stashReplayedFact is the event fact naming every stash an open replayed:
// a list, empty when the open replayed nothing and absent when it never
// looked. [LAW:nothing-unseen]
const stashReplayedFact = "migration.stash_replayed"

const stashReplayCheckpointPrefix = "pre-stash-replay"

var stashReplay = repairSteps{
	prefix: stashReplayCheckpointPrefix, checkpoint: stepStashCheckpoint, repair: stepStashReplay, reset: stepStashReplayReset,
	prune: pruneStashCheckpoints, what: "stash replay",
	commitFailed: "commit stash replay: %w",
}

// pendingStash is the versions at or below applied whose stash still holds
// entries. Each version's replay runs after its own forward step, so these
// are a store a lit that never replays raised past its stash: a release
// older than the stash, whose Ups this one shares byte for byte. It reads
// one grouped count over the carrier, and nothing on a store without one.
func (s *Store) pendingStash(ctx context.Context, applied int64) ([]int64, error) {
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = 'schema_stash'`).Scan(&present); err != nil {
		return nil, fmt.Errorf("look for the stash carrier: %w", err)
	}
	if present == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT table_name, column_name FROM schema_stash`)
	if err != nil {
		return nil, fmt.Errorf("read stash entries: %w", err)
	}
	defer rows.Close()
	held := map[[2]string]bool{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf("read stash entries: %w", err)
		}
		held[[2]string{table, column}] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stash entries: %w", err)
	}
	var pending []int64
	for v, stashes := range migrations.Restores {
		if v > applied {
			continue
		}
		if slices.ContainsFunc(stashes, func(st migrations.Stash) bool {
			table, column := st.Entry()
			return held[[2]string{table, column}]
		}) {
			pending = append(pending, v)
		}
	}
	slices.Sort(pending)
	return pending, nil
}

// replayPendingStash replays versions, lowest first, and commits what they
// wrote. It writes back what the stash holds onto data a later Up already
// derived without it, so it cannot undo what that Up decided; the event says
// what it replayed.
func (s *Store) replayPendingStash(ctx context.Context, versions []int64, probed probedFacts) (pruneErr error, fail *SchemaConvergenceError) {
	var replayed []migrations.Replayed
	pruneErr, fail = s.repairWithRollback(ctx, stashReplay, func() (string, error) {
		names := make([]string, 0, len(versions))
		for _, v := range versions {
			r, err := migrations.ReplayStash(ctx, s.db, v)
			if err != nil {
				return "", err
			}
			replayed = append(replayed, r...)
			names = append(names, fmt.Sprintf("v%d", v))
		}
		return fmt.Sprintf("migrate: replay the stash of %s", strings.Join(names, ", ")), nil
	})
	if fail == nil {
		probed.replayed(replayed)
	}
	return pruneErr, fail
}

// replayed appends each stash a replay wrote rows from to the open's stash
// fact; a stash that held nothing is not news.
func (p probedFacts) replayed(rs []migrations.Replayed) {
	list, _ := p[stashReplayedFact].([]map[string]any)
	if list == nil {
		list = []map[string]any{}
	}
	for _, r := range rs {
		if r.Rows == 0 {
			continue
		}
		list = append(list, map[string]any{"version": r.Version, "stash": r.Name, "rows": r.Rows})
	}
	p[stashReplayedFact] = list
}
