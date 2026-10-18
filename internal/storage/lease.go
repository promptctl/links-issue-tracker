package storage

// LeaseOutcome is what a leased push found at the remote branch it updates.
type LeaseOutcome string

const (
	// LeaseHeld: the remote branch named the commit the lease expected, or
	// already the one pushed, and now names the one pushed.
	LeaseHeld LeaseOutcome = "held"
	// LeaseStale: the remote branch had moved to some other commit, and the
	// push left it there.
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
