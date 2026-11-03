package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// The event facts naming each stash a step settled, replayed, or held: a
// list, empty when the open looked and found nothing, and absent when it
// never looked. [LAW:nothing-unseen]
const (
	stashSettledFact  = "migration.stash_settled"
	stashReplayedFact = "migration.stash_replayed"
	stashHeldFact     = "migration.stash_held"
)

// heldStash is each stash at or below applied whose entries are still in the
// carrier: a store a lit that never replays raised past its stash, a release
// older than the stash, whose Ups this one shares byte for byte. Its Up ran
// without a settle, so nothing can say whether a write since reached their
// units: they are held, never written, and the carrier keeps them. It reads
// the carrier's entry counts, and nothing on a store without one.
func (s *Store) heldStash(ctx context.Context, applied int64) ([]migrations.StashRows, error) {
	present, err := migrations.CarrierPresent(ctx, s.db)
	if err != nil || !present {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT table_name, column_name, COUNT(*) FROM schema_stash GROUP BY table_name, column_name`)
	if err != nil {
		return nil, fmt.Errorf("read stash entries: %w", err)
	}
	defer rows.Close()
	counts := map[[2]string]int64{}
	for rows.Next() {
		var table, column string
		var n int64
		if err := rows.Scan(&table, &column, &n); err != nil {
			return nil, fmt.Errorf("read stash entries: %w", err)
		}
		counts[[2]string{table, column}] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stash entries: %w", err)
	}
	var held []migrations.StashRows
	for v, stashes := range migrations.Restores {
		if v > applied {
			continue
		}
		for _, st := range stashes {
			table, column := st.Entry()
			if n := counts[[2]string{table, column}]; n > 0 {
				held = append(held, migrations.StashRows{Version: v, Name: st.Name(), Rows: n})
			}
		}
	}
	slices.SortStableFunc(held, func(a, b migrations.StashRows) int { return cmp.Compare(a.Version, b.Version) })
	return held, nil
}

// stash appends each stash with rows to fact's list, creating the list: a
// stash that counted nothing is not news.
func (p probedFacts) stash(fact string, rs []migrations.StashRows) {
	list, _ := p[fact].([]map[string]any)
	if list == nil {
		list = []map[string]any{}
	}
	for _, r := range rs {
		if r.Rows == 0 {
			continue
		}
		list = append(list, map[string]any{"version": r.Version, "stash": r.Name, "rows": r.Rows})
	}
	p[fact] = list
}
