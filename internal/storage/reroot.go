package storage

import (
	"maps"
	"time"
)

// ReRooted is what a re-root did to a branch: its history cut at a commit, the
// commits below the cut dropped and every commit above it rewritten onto a
// parentless copy of the cut.
type ReRooted struct {
	// Cut is the commit the history was cut at.
	Cut string
	// Root is the parentless commit carrying Cut's root value and metadata.
	Root string
	// Head is the branch's head after the re-root.
	Head string
	// Rewritten maps every commit the branch keeps — Cut and each commit
	// after it — from its hash before the re-root to its hash after.
	Rewritten map[string]string
	// Dropped counts the commits the branch reached before the re-root and
	// reaches no longer: the ancestors of Cut.
	Dropped int
	// Elapsed is the re-root's work inside the hold, from the commit lock's
	// acquisition to the branch move.
	Elapsed time.Duration
}

// Kept counts the commits the branch keeps.
func (r ReRooted) Kept() int {
	return len(r.Rewritten)
}

// EventRecord is the re-root as event facts and counts.
func (r ReRooted) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
			"reroot.cut":        r.Cut,
			"reroot.root":       r.Root,
			"reroot.head":       r.Head,
			"reroot.elapsed_ms": r.Elapsed.Milliseconds(),
		}, map[string]int{
			"reroot.kept":    r.Kept(),
			"reroot.dropped": r.Dropped,
		}
}

// SyncEntry names the sync entry point a step ran under.
type SyncEntry string

const (
	// SyncEntryPull is `lit sync pull`.
	SyncEntryPull SyncEntry = "pull"
	// SyncEntryReceive is a receive: the automatic one, or the on-change
	// mirror's receive of what it pushed.
	SyncEntryReceive SyncEntry = "receive"
	// SyncEntryReconcile is `lit sync reconcile`.
	SyncEntryReconcile SyncEntry = "reconcile"
	// SyncEntryPush is a push the remote refused: `lit sync push`, or the
	// on-change mirror's push.
	SyncEntryPush SyncEntry = "push"
	// SyncEntryFetch is `lit sync fetch`.
	SyncEntryFetch SyncEntry = "fetch"
)

// FollowedReRoot is a re-root a store applied to its own branch to follow a
// remote that another checkout re-rooted, and the entry point that followed.
type FollowedReRoot struct {
	Entry SyncEntry
	ReRooted
}

// EventValue is the follow as one value of the event's reroot.follows list:
// the re-root's facts and counts under their own keys, and the entry point
// under reroot.follow. A list, because one invocation can follow more than
// once — the mirror's clone and then its live store, or one long-lived mirror
// across cycles.
func (f FollowedReRoot) EventValue() map[string]any {
	facts, counts := f.ReRooted.EventRecord()
	value := map[string]any{"reroot.follow": string(f.Entry)}
	maps.Copy(value, facts)
	for key, n := range counts {
		value[key] = n
	}
	return value
}
