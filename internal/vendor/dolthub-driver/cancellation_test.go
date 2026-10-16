// Added by lit (README.lit-patch.md, Patch 6); not part of upstream.

package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cancelOnNthDone is a context that cancels itself on the nth call to Done,
// so a test can cut a statement short at a chosen point in its run without
// racing a timer. n == 0 never cancels, and counts the calls a whole run makes.
type cancelOnNthDone struct {
	context.Context
	n int

	mu     sync.Mutex
	calls  int
	done   chan struct{}
	closed bool
}

func newCancelOnNthDone(n int) *cancelOnNthDone {
	return &cancelOnNthDone{Context: context.Background(), n: n, done: make(chan struct{})}
}

func (c *cancelOnNthDone) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.n > 0 && c.calls >= c.n && !c.closed {
		close(c.done)
		c.closed = true
	}
	return c.done
}

func (c *cancelOnNthDone) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return context.Canceled
	}
	return nil
}

func (c *cancelOnNthDone) Deadline() (time.Time, bool) { return time.Time{}, false }

// TestCancelledStatementLandsNothing pins what Patch 6's per-statement contexts
// rest on: a statement cut short by its own ctx fails with "context canceled",
// lands none of its writes, and leaves no transaction on the session for the
// next statement or BEGIN to adopt. It cancels a 200-row autocommit UPDATE at
// points spread across its whole run — analysis and row loop alike — and
// requires every run to have either completed whole or been cancelled with
// nothing written. A driver that ran statements on any context but their
// caller's would cancel none of them.
func TestCancelledStatementLandsNothing(t *testing.T) {
	conn, cleanup := initializeTestDatabaseConnection(t, false)
	defer cleanup()
	ctx := context.Background()

	const rowCount = 200
	_, err := conn.ExecContext(ctx, "CREATE TABLE t (id int primary key, v int)")
	require.NoError(t, err)
	values := make([]string, rowCount)
	for i := range values {
		values[i] = fmt.Sprintf("(%d, 0)", i)
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO t VALUES "+strings.Join(values, ", "))
	require.NoError(t, err)

	// run executes query on the driver connection directly, under a context
	// that cancels on its nth Done call, and reports the error, how many Done
	// calls the run made, and whether the session still holds a transaction.
	run := func(query string, n int) (execErr error, calls int, txLeft bool) {
		require.NoError(t, conn.Raw(func(dc any) error {
			c := dc.(*DoltConn)
			stmt, err := c.Prepare(query)
			if err != nil {
				return err
			}
			defer stmt.Close()
			cc := newCancelOnNthDone(n)
			_, execErr = stmt.(driver.StmtExecContext).ExecContext(cc, nil)
			calls = cc.calls
			txLeft = c.gmsCtx.GetTransaction() != nil
			return nil
		}))
		return execErr, calls, txLeft
	}
	countUpdated := func() int {
		var n int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM t WHERE v = 1").Scan(&n))
		return n
	}

	// The calls an UPDATE that matches nothing makes are its fixed cost, so a
	// cancellation past that count lands in the row loop.
	_, fixedCost, _ := run("UPDATE t SET v = 1 WHERE v = -1", 0)
	_, wholeRun, _ := run("UPDATE t SET v = 0", 0)
	require.Greater(t, wholeRun, fixedCost, "the statement does not consult its own ctx: a 200-row UPDATE made %d Done calls, an empty one %d", wholeRun, fixedCost)

	cancelledInRowLoop := 0
	step := max(1, wholeRun/40)
	for n := 1; n <= wholeRun+step; n += step {
		_, err := conn.ExecContext(ctx, "UPDATE t SET v = 0")
		require.NoError(t, err)

		execErr, _, txLeft := run("UPDATE t SET v = 1", n)
		require.False(t, txLeft, "cancel at Done call %d left a transaction on the session", n)
		updated := countUpdated()
		if execErr == nil {
			require.Equal(t, rowCount, updated, "uncancelled run at Done call %d", n)
			continue
		}
		require.Contains(t, execErr.Error(), context.Canceled.Error(), "run cut at Done call %d", n)
		require.Zero(t, updated, "cancel at Done call %d landed part of the UPDATE", n)
		if n > fixedCost {
			cancelledInRowLoop++
		}
		// A BEGIN after the cancelled statement commits nothing of it either.
		tx, err := conn.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		require.Zero(t, countUpdated(), "BEGIN after a cancel at Done call %d committed part of the UPDATE", n)
	}
	require.Positive(t, cancelledInRowLoop, "no cancellation landed in the row loop (fixed cost %d of %d Done calls)", fixedCost, wholeRun)
}
