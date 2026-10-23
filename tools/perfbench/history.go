package main

import "fmt"

// historyEdits is how many single-row edits the history measurement makes.
// Every lit write is one Dolt commit, so this is a count of commits, and the
// store's growth across them divided by it is what one commit of history
// costs on disk. Measured 2026-10-21 on a 590-row store, 100 edits took 16s
// and grew the store by 1.83 MB after a full compaction: large enough that
// the per-commit figure stands well clear of the store's own run-to-run
// spread, small enough that `just perf` stays a command people run.
const historyEdits = 100

// historyCost is what a run of single-row edits added to a store, measured
// after a full compaction on both sides so the figure is history the store
// keeps rather than garbage the next compaction would reclaim.
type historyCost struct {
	edits         int
	before, after int64
}

// perCommit is the bytes one commit of history adds.
func (h historyCost) perCommit() int64 {
	return (h.after - h.before) / int64(h.edits)
}

// compactedBytes collects store fully and returns its bytes: what the store
// keeps once compaction has reclaimed everything it can. A freshly imported
// store carries the import's uncollected garbage, so this, not the imported
// size, is what a store of these rows costs to keep.
func compactedBytes(bin litBinary, store generatedStore) (int64, error) {
	compact := step{budget: runBudget, argv: []string{bin.path, "sync", "compact", "--full"}}
	if _, err := runQuiet(store.root, store.env, compact); err != nil {
		return 0, err
	}
	return storeBytes(store.databasePath)
}

// measureHistory edits historyEdits rows of store, one commit each, and
// returns what that history costs, from before: the store's compacted bytes.
// It runs after every probe, because it changes the store the probes'
// figures describe.
//
// The edits go round the store's own rows, in the order they were generated,
// rather than one row many times, so each commit touches the kind of row an
// ordinary write touches: one row of many, not the same row again. A store
// with no row has nothing to edit, and says so with an error instead of a
// figure of zero.
func measureHistory(bin litBinary, store generatedStore, before int64) (historyCost, error) {
	ids := store.ids
	if len(ids) == 0 {
		return historyCost{}, fmt.Errorf("store %s has no row to edit", store.size.name)
	}
	for i := range historyEdits {
		edit := step{budget: runBudget, argv: []string{bin.path, "update", ids[i%len(ids)], "--title", fmt.Sprintf("perfbench history edit %d", i)}}
		if _, err := runQuiet(store.root, store.env, edit); err != nil {
			return historyCost{}, err
		}
	}
	after, err := compactedBytes(bin, store)
	if err != nil {
		return historyCost{}, err
	}
	return historyCost{edits: historyEdits, before: before, after: after}, nil
}
