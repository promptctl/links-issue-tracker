package storage

import (
	"fmt"
	"maps"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/historywindow"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// BoundWindow is the window a push keeps the sync branch's history inside,
// as the workspace's config names it, or the refusal that left the config
// naming none. An unreadable config costs the bound and never the push: the
// push lands, and its maintenance says the bound was not kept and why.
type BoundWindow struct {
	window historywindow.Window
	unread error
}

// BoundWindowRead is the window as one read of config left it: the window it
// named, or readErr, the refusal, in its place. It takes the read's own pair,
// so no window goes unread without the refusal that says why.
// [LAW:types-are-the-program]
func BoundWindowRead(window historywindow.Window, readErr error) BoundWindow {
	return BoundWindow{window: window, unread: readErr}
}

// Window is the window, or the refusal in its place.
func (b BoundWindow) Window() (historywindow.Window, error) { return b.window, b.unread }

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
	// Window is the bound the push was kept inside.
	Window historywindow.Window
	// ReRooted is the re-root written to replace Head: zero until the bound
	// fires, and zero when the re-root was refused. Whether it left this
	// checkout is Publication's to say.
	ReRooted ReRooted
	// Lease is what the force push of ReRooted found at the remote branch:
	// empty when the push never reached the remote.
	Lease LeaseOutcome
	// Publication is how far ReRooted got toward the remote: whether the
	// remote carries it in Head's place. [LAW:types-are-the-program]
	Publication ReRootPublication
	// RemoteGC is the remote's GC that follows the force push, whatever the
	// lease said: the remote's root decides whether it replaces anything.
	RemoteGC RemoteGC
	// Collection is what this checkout reclaimed once its branch followed the
	// re-root the remote carries: the Collection of the bound's own follow,
	// zero when the branch did not follow.
	Collection Collection
	// Elapsed is the bound's work end to end, from reading the head's height
	// to the follow's collection.
	Elapsed time.Duration
	// Err is where the bound stopped, if it did. The push before it landed
	// whatever Err says.
	Err error
}

// ReRootPublication is how far a history bound's re-root got toward the
// remote. Its zero value is the bound whose re-root never left this
// checkout, which is every bound that wrote none.
type ReRootPublication int

const (
	// ReRootNotPushed: the bound wrote no re-root, or its lease push stopped
	// before it reached the remote. Nothing the bound did crossed the network.
	ReRootNotPushed ReRootPublication = iota
	// ReRootNotCarried: the lease push reached the remote, and the fetch after
	// it shows the remote without the re-root.
	ReRootNotCarried
	// ReRootPossiblyPublished: the lease push reached the remote without a held
	// verdict, and no fetch settled whether an attempt landed.
	ReRootPossiblyPublished
	// ReRootPublished: the remote carries the re-root: the fetch after the
	// lease push shows it, or the lease held and no fetch said otherwise.
	ReRootPublished
)

// String is the publication as the event and the maintenance account spell it.
func (p ReRootPublication) String() string {
	switch p {
	case ReRootNotPushed:
		return "not pushed"
	case ReRootNotCarried:
		return "not carried"
	case ReRootPossiblyPublished:
		return "possibly published"
	case ReRootPublished:
		return "published"
	default:
		return fmt.Sprintf("ReRootPublication(%d)", int(p))
	}
}

// Fired is whether the remote head reached the height at which a push
// re-roots.
func (b HistoryBound) Fired() bool {
	return b.Height >= b.Window.ReRootAt()
}

// EventValue is the bound as one value of the event's history_bound.records
// list: its own facts under history_bound.*, the re-root it published under
// the reroot.* keys every re-root's facts use, and the remote's GC under
// remote_gc.*. The reclaim its follow ran is that follow's, on the event's
// reroot.follows list under reroot.follow=bound, and only there.
// [LAW:one-source-of-truth]
func (b HistoryBound) EventValue() map[string]any {
	value := map[string]any{
		"history_bound.remote":      b.Remote,
		"history_bound.branch":      b.Branch,
		"history_bound.head":        b.Head,
		"history_bound.height":      b.Height,
		"history_bound.reroot_at":   b.Window.ReRootAt(),
		"history_bound.keep":        b.Window.Keep(),
		"history_bound.fired":       b.Fired(),
		"history_bound.lease":       string(b.Lease),
		"history_bound.publication": b.Publication.String(),
		"history_bound.elapsed_ms":  b.Elapsed.Milliseconds(),
		"history_bound.error":       telemetry.ErrorText(b.Err),
	}
	facts, counts := b.ReRooted.EventRecord()
	maps.Copy(value, facts)
	maps.Copy(value, b.RemoteGC.EventRecord())
	for key, n := range counts {
		value[key] = n
	}
	return value
}
