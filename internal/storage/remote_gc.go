package storage

// RemoteGC is the remote's GC as a bound's re-root runs it from this
// checkout: the table files reachable from the re-rooted head, built in a
// scratch store here and written in place of every table file the remote
// held, under a new GC generation (FORKS.md, dolt patch 10). The one manifest
// write lands only while the remote's root is still the one those files hold,
// so a write that landed in between keeps the remote's table set whole.
type RemoteGC struct {
	// Outcome is empty when the GC did not reach the remote's manifest.
	Outcome RemoteGCOutcome
	// DroppedBranches counts the remote's branches other than the sync
	// branch, which the GC deletes before it compares roots: the collected
	// files hold the sync branch's history alone.
	DroppedBranches int
	// FilesBefore and BytesBefore are the table files the remote's manifest
	// named when the GC read it; FilesAfter and BytesAfter, when it was done.
	FilesBefore, FilesAfter int
	BytesBefore, BytesAfter int64
}

// RemoteGCOutcome is what the remote's GC found at the remote's manifest.
type RemoteGCOutcome string

const (
	// RemoteGCReplaced: the remote's manifest names exactly the collected
	// table files, under a new GC generation.
	RemoteGCReplaced RemoteGCOutcome = "replaced"
	// RemoteGCMoved: a write landed on the remote after the re-root, so its
	// root is not the one the collected files hold, and its table files
	// stay. The next re-root's GC collects them.
	RemoteGCMoved RemoteGCOutcome = "moved"
)

// EventRecord is the remote's GC as event facts.
func (g RemoteGC) EventRecord() map[string]any {
	return map[string]any{
		"remote_gc.outcome":          string(g.Outcome),
		"remote_gc.dropped_branches": g.DroppedBranches,
		"remote_gc.files_before":     g.FilesBefore,
		"remote_gc.files_after":      g.FilesAfter,
		"remote_gc.bytes_before":     g.BytesBefore,
		"remote_gc.bytes_after":      g.BytesAfter,
	}
}
