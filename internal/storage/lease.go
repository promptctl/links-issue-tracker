package storage

// LeaseOutcome is what a leased push found at the remote branch it updates.
// A push that stopped before it reached the remote found nothing, and has no
// outcome.
type LeaseOutcome string

const (
	// LeaseHeld: the remote branch named the commit the lease expected, or
	// already the one pushed, and now names the one pushed.
	LeaseHeld LeaseOutcome = "held"
	// LeaseStale: the remote branch named neither the commit the lease
	// expected nor the one pushed, or did not exist, and the push left it
	// there. It is the verdict of the push's last transport attempt, and an
	// earlier attempt may have landed the pushed commit before a peer built
	// on it, so whether the remote carries the pushed commit is answered by a fetch, as
	// SyncPushFromClone answers it for a refused push.
	LeaseStale LeaseOutcome = "stale"
	// LeaseUnsettled: the push was sent and came back with no verdict, its
	// error beside it, an unreachable remote's among them. The write may have
	// landed before the transport failed, or an attempt before the last may
	// have, so it is answered by a fetch, as a stale lease is.
	LeaseUnsettled LeaseOutcome = "unsettled"
)
