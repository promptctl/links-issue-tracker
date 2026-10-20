package storage

// RemoteRemovalRecord is one attempt to remove a remote Dolt holds: the name,
// the owner of the refs its fetches wrote (the name's first segment), and
// what went with it. The owner's tracking refs and fork points go only when
// Dolt holds no other remote named the owner; the fork points dropped are on
// the event's fork_point.records.
type RemoteRemovalRecord struct {
	Remote, Owner string
	// Held is whether Dolt held the name when the attempt ran. A name an
	// earlier attempt or another reconcile already removed is not held, and
	// is not removed again.
	Held bool
	// OwnerHeld is whether Dolt holds another remote named Owner, whose refs
	// the owner's are, so none of them went.
	OwnerHeld bool
	// TrackingRefsDropped is how many of the owner's tracking refs the
	// attempt deleted.
	TrackingRefsDropped int
}

// EventValue is the record as one value of the event's remote.removals list.
func (r RemoteRemovalRecord) EventValue() map[string]any {
	return map[string]any{
		"remote_removal.remote":                r.Remote,
		"remote_removal.owner":                 r.Owner,
		"remote_removal.held":                  r.Held,
		"remote_removal.owner_held":            r.OwnerHeld,
		"remote_removal.tracking_refs_dropped": r.TrackingRefsDropped,
	}
}
