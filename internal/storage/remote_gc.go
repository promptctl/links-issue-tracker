package storage

// RemoteGC is the remote's GC as a bound's re-root runs it from this
// checkout: the table files reachable from the re-rooted head, built in a
// scratch store here and written in place of every table file the remote
// held, under a new GC generation (FORKS.md, dolt patch 10). Each of its
// writes lands only while the remote still holds the view the GC read, so a
// write that landed in between leaves the remote's table set whole.
type RemoteGC struct {
	// Outcome is empty when the GC stopped on an error before it learned
	// whether the remote moved.
	Outcome RemoteGCOutcome
	// DroppedBranches counts the remote's branches other than the sync
	// branch that the GC's root write dropped: the collected files hold the
	// sync branch's history alone.
	DroppedBranches int
	// BytesBefore is the table files the remote held when the GC read it;
	// CollectedBytes, the collected table files, which the remote holds
	// exactly once the GC replaced its table set. Zero until collected.
	BytesBefore, CollectedBytes int64
}

// RemoteGCOutcome is how far the remote's GC got before a write landed on the
// remote ahead of it, or to the end.
type RemoteGCOutcome string

const (
	// RemoteGCMoved: the remote's sync branch was not at the re-rooted head,
	// or a write landed before the GC's root write. Nothing the remote names
	// changed, and every branch stays. The next re-root's GC collects it.
	RemoteGCMoved RemoteGCOutcome = "moved"
	// RemoteGCDropped: the remote's root names the sync branch alone, and its
	// table set stays whole: a write landed before the GC replaced it, or the
	// replacement failed.
	RemoteGCDropped RemoteGCOutcome = "dropped"
	// RemoteGCReplaced: the remote's root names the sync branch alone and its
	// manifest names exactly the collected table files, under a new GC
	// generation.
	RemoteGCReplaced RemoteGCOutcome = "replaced"
)

// EventRecord is the remote's GC as event facts.
func (g RemoteGC) EventRecord() map[string]any {
	return map[string]any{
		"remote_gc.outcome":          string(g.Outcome),
		"remote_gc.dropped_branches": g.DroppedBranches,
		"remote_gc.bytes_before":     g.BytesBefore,
		"remote_gc.collected_bytes":  g.CollectedBytes,
	}
}
