package storage

// ReconcileBaseSource is where a reconcile's three-way base came from.
// [LAW:types-are-the-program] a closed set, so a reader of the event can tell
// a merge against the heads' own ancestor from one against the fork point.
type ReconcileBaseSource string

const (
	// ReconcileBaseMergeBase: the local head and the remote head share
	// history, and the base is their merge base.
	ReconcileBaseMergeBase ReconcileBaseSource = "merge_base"
	// ReconcileBaseForkPoint: the two heads share no history, because the
	// remote re-rooted above every commit this checkout holds, and the base is
	// the merge base of the local head and the fork point.
	ReconcileBaseForkPoint ReconcileBaseSource = "fork_point"
	// ReconcileBaseNone: there is no base — the branch never synced, or the
	// heads share no history and the divergence is unrelated histories.
	ReconcileBaseNone ReconcileBaseSource = "none"
)

// ReconcileBase is the base a reconcile merges against and where it came
// from. When the heads share no history it also says what the fork point
// held: the remote head this checkout last knew while the two still shared
// history, recorded before each fetch moved the tracking ref.
type ReconcileBase struct {
	Source ReconcileBaseSource
	// Commit is the base; empty when Source is none.
	Commit string
	// ForkPoint is the recorded fork point, read because the heads share no
	// history; empty when they share one or no fork point was recorded. With
	// Source none and no UnaccountedTickets, the fork point itself shares no
	// history with the local head.
	ForkPoint string
	// UnaccountedTickets counts the tickets at the fork point's base that the
	// remote head holds neither a row nor a purge record for, and
	// UnaccountedEvents the base's ticket events the remote head lacks whose
	// ticket no purge record names. The merge reads a missing ticket as one the
	// remote removed and a missing event's edit as one the remote reverted, so
	// a remote that lacks any is not taken to continue the history the fork
	// point names, and Source is none.
	UnaccountedTickets int
	UnaccountedEvents  int
}

// Found is the base commit and whether there is one.
func (b ReconcileBase) Found() (commit string, ok bool) {
	return b.Commit, b.Source == ReconcileBaseMergeBase || b.Source == ReconcileBaseForkPoint
}

// EventValue is the base as one value of the event's reconcile.bases list.
func (b ReconcileBase) EventValue() map[string]any {
	return map[string]any{
		"reconcile.base":                string(b.Source),
		"reconcile.base_commit":         b.Commit,
		"reconcile.fork_point":          b.ForkPoint,
		"reconcile.unaccounted_tickets": b.UnaccountedTickets,
		"reconcile.unaccounted_events":  b.UnaccountedEvents,
	}
}

// ForkPointOutcome is what a fetch did to one fork point before it moved the
// tracking ref.
type ForkPointOutcome string

const (
	// ForkPointRecorded: the fork point now names the tracking ref's head.
	ForkPointRecorded ForkPointOutcome = "recorded"
	// ForkPointUnchanged: the fork point already named it.
	ForkPointUnchanged ForkPointOutcome = "unchanged"
	// ForkPointKept: the local branch shares no history with the tracking
	// ref's head, so the fork point keeps the head it last recorded.
	ForkPointKept ForkPointOutcome = "kept"
	// ForkPointNone: the local branch shares no history with the tracking
	// ref's head and no fork point was ever recorded, so there is none.
	ForkPointNone ForkPointOutcome = "none"
	// ForkPointDropped: the fetch left no tracking ref for the fork point, so
	// it was deleted with it; Head is the head it held.
	ForkPointDropped ForkPointOutcome = "dropped"
)

// ForkPointRecord is one tracking ref a fetch was about to move, and what it
// did to that ref's fork point first, or one fork point deleted because its
// tracking ref is gone.
type ForkPointRecord struct {
	Remote, Branch string
	// Head is the tracking ref's head before the fetch, or the head a dropped
	// fork point held.
	Head    string
	Outcome ForkPointOutcome
}

// EventValue is the record as one value of the event's fork_point.records
// list.
func (r ForkPointRecord) EventValue() map[string]any {
	return map[string]any{
		"fork_point.remote":  r.Remote,
		"fork_point.branch":  r.Branch,
		"fork_point.head":    r.Head,
		"fork_point.outcome": string(r.Outcome),
	}
}
