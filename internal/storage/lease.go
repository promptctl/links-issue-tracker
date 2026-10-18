package storage

// LeaseOutcome is what a leased push found at the remote branch it updates.
type LeaseOutcome string

const (
	// LeaseHeld: the remote branch named the commit the lease expected, or
	// already the one pushed, and now names the one pushed.
	LeaseHeld LeaseOutcome = "held"
	// LeaseStale: the remote branch named neither the commit the lease
	// expected nor the one pushed, or did not exist, and the push left it
	// there. It is the verdict of the push's last transport attempt, and an
	// earlier attempt may have landed the pushed commit before a peer built
	// on it, so whether the remote carries Pushed is answered by a fetch, as
	// SyncPushFromClone answers it for a refused push.
	LeaseStale LeaseOutcome = "stale"
)

// LeasedPush is one push that updated a remote branch only while that branch
// still named the commit the pusher expected: git's --force-with-lease. It is
// how a rewritten history is published without overwriting a push that landed
// after the pusher last read the remote.
type LeasedPush struct {
	Remote, Branch string
	// Expected is the remote branch's head the lease named.
	Expected string
	// Pushed is the commit the push sent.
	Pushed  string
	Outcome LeaseOutcome
}
