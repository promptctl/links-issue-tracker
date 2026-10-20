package storage

// PrunedRef is one tracking ref a fetch deleted because its branch is gone
// from the remote. The fork point that went with it is on the event's
// fork_point.records, the one ledger of fork points.
type PrunedRef struct {
	Remote, Branch string
	// Head is the tracking ref's head before the fetch deleted it.
	Head string
}

// EventValue is the record as one value of the event's fetch.pruned list.
func (r PrunedRef) EventValue() map[string]any {
	return map[string]any{
		"fetch.pruned.remote": r.Remote,
		"fetch.pruned.branch": r.Branch,
		"fetch.pruned.head":   r.Head,
	}
}
