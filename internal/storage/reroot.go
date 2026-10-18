package storage

import (
	"maps"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
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
	// SyncEntryBound is a push past the history bound, following the re-root
	// it published (HistoryBound).
	SyncEntryBound SyncEntry = "bound"
)

// FollowedReRoot is a re-root a store applied to its own branch to follow a
// remote that another checkout re-rooted, the entry point that followed, and
// what the store reclaimed below the cut once its branch was across it.
type FollowedReRoot struct {
	Entry SyncEntry
	ReRooted
	Collection Collection
}

// Collection is the reclaim that follows a re-root onto a store's branch: the
// local refs naming commits below the cut dropped, then the full GC. Ran is
// whether the GC ran, and the two byte counts are the store's size on disk
// around the pass. Sweep is the line the GC's sweep of orphaned remote-cache
// refs rendered, empty when it found nothing to say. Err is where the pass
// stopped, or a reading around it that failed: a collection is maintenance,
// so its failure is carried here and never fails the follow it came after.
type Collection struct {
	Ran                     bool
	BytesBefore, BytesAfter int64
	Sweep                   string
	Err                     error
}

// EventRecord is the collection as event facts.
func (c Collection) EventRecord() map[string]any {
	return map[string]any{
		"collection.ran":          c.Ran,
		"collection.bytes_before": c.BytesBefore,
		"collection.bytes_after":  c.BytesAfter,
		"collection.sweep":        c.Sweep,
		"collection.error":        telemetry.ErrorText(c.Err),
	}
}

// EventValue is the follow as one value of the event's reroot.follows list:
// the re-root's facts and counts under their own keys, the collection under
// collection.*, and the entry point under reroot.follow. A list, because one
// invocation can follow more than once — the mirror's clone and then its live
// store, or one long-lived mirror across cycles.
func (f FollowedReRoot) EventValue() map[string]any {
	facts, counts := f.ReRooted.EventRecord()
	value := map[string]any{"reroot.follow": string(f.Entry)}
	maps.Copy(value, facts)
	maps.Copy(value, f.Collection.EventRecord())
	for key, n := range counts {
		value[key] = n
	}
	return value
}
