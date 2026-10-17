package storage

import "time"

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

// EventRecord is the follow as event facts and counts: the re-root's, and the
// entry point under reroot.follow.
func (f FollowedReRoot) EventRecord() (map[string]any, map[string]int) {
	facts, counts := f.ReRooted.EventRecord()
	facts["reroot.follow"] = string(f.Entry)
	return facts, counts
}
