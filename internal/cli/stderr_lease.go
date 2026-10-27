package cli

import (
	"io"
	"sync"
)

// stderrLease lends a run's stderr to a goroutine the run may abandon. Every
// write holds the lease's mutex, and revoke takes the same mutex to swap the
// writer for io.Discard, so a write in flight when the run ends finishes
// first and every write after it is dropped: the run it belonged to is over,
// and its caller may already be reading that stderr.
// [LAW:no-ambient-temporal-coupling] the run's end is owned state, not the
// goroutine's timing.
type stderrLease struct {
	mu sync.Mutex
	w  io.Writer
}

func leaseStderr(stderr io.Writer) *stderrLease {
	return &stderrLease{w: stderr}
}

func (l *stderrLease) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// revoke ends the lease: when it returns, nothing written through the lease
// reaches the run's stderr again.
func (l *stderrLease) revoke() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w = io.Discard
}
