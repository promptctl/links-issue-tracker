package storage

import (
	"maps"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// HistoryBound is what one push did to keep the sync branch's history under
// its bound: the height of the remote head the push left, against the height
// at which a push re-roots, and, once that height is reached, the re-root the
// push published in the head's place and what became of it.
type HistoryBound struct {
	Remote, Branch string
	// Head is the remote head the push left, and Height its height: the
	// commits on the longest line from it down to the root, itself counted.
	Head   string
	Height uint64
	// ReRootAt is the height at which a push re-roots, and Keep how many
	// commits of the head's first-parent line the re-root keeps.
	ReRootAt, Keep uint64
	// ReRooted is the re-root published in Head's place: zero until the bound
	// fires, and zero when the re-root was refused.
	ReRooted ReRooted
	// Lease is what the force push of ReRooted found at the remote branch:
	// empty when nothing was pushed.
	Lease LeaseOutcome
	// RemoteGC is the remote's GC that follows the force push, whatever the
	// lease said: the remote's root decides whether it replaces anything.
	RemoteGC RemoteGC
	// Collection is what this checkout reclaimed once its branch followed the
	// re-root the remote carries: the Collection of the bound's own follow,
	// zero when the branch did not follow.
	Collection Collection
	// Err is where the bound stopped, if it did. The push before it landed
	// whatever Err says.
	Err error
}

// Fired is whether the remote head reached the height at which a push
// re-roots.
func (b HistoryBound) Fired() bool {
	return b.Height >= b.ReRootAt
}

// EventValue is the bound as one value of the event's history_bound.records
// list: its own facts under history_bound.*, the re-root it published under
// the reroot.* keys every re-root's facts use, and the remote's GC under
// remote_gc.*. The reclaim its follow ran is that follow's, on the event's
// reroot.follows list under reroot.follow=bound, and only there.
// [LAW:one-source-of-truth]
func (b HistoryBound) EventValue() map[string]any {
	value := map[string]any{
		"history_bound.remote":    b.Remote,
		"history_bound.branch":    b.Branch,
		"history_bound.head":      b.Head,
		"history_bound.height":    b.Height,
		"history_bound.reroot_at": b.ReRootAt,
		"history_bound.keep":      b.Keep,
		"history_bound.fired":     b.Fired(),
		"history_bound.lease":     string(b.Lease),
		"history_bound.error":     telemetry.ErrorText(b.Err),
	}
	facts, counts := b.ReRooted.EventRecord()
	maps.Copy(value, facts)
	maps.Copy(value, b.RemoteGC.EventRecord())
	for key, n := range counts {
		value[key] = n
	}
	return value
}
