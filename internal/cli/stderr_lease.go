package cli

import (
	"io"
)

// stderrLease lends a run's stderr to a goroutine the run may abandon. It is a
// serialWriter that can be revoked: revoke takes the mutex every write holds
// to swap the writer for io.Discard, so a write in flight when the run ends
// finishes first and every write after it is dropped: the run it belonged to
// is over, and its caller may already be reading that stderr.
// [LAW:no-ambient-temporal-coupling] the run's end is owned state, not the
// goroutine's timing.
type stderrLease struct {
	serialWriter
}

func leaseStderr(stderr io.Writer) *stderrLease {
	return &stderrLease{serialWriter{w: stderr}}
}

// revoke ends the lease: when it returns, nothing written through the lease
// reaches the run's stderr again.
func (l *stderrLease) revoke() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w = io.Discard
}
