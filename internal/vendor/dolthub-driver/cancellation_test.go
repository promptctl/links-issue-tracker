// Added by lit (README.lit-patch.md, Patch 6); not part of upstream.

package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// cancelOnNthDone is a context that cancels itself on the nth call to Done,
// so a test can cut a statement short at a chosen point in its run without
// racing a timer. n == 0 never cancels, and counts the calls a whole run makes.
// Done hands out the channel of the context.WithCancel it wraps, so the
// contexts go-mysql-server derives from it attach to that cancelCtx directly
// rather than through a propagation goroutine whose own Done call would land
// at an unscheduled point in the count.
type cancelOnNthDone struct {
	context.Context
	cancel context.CancelFunc
	n      int

	mu    sync.Mutex
	calls int
}

func newCancelOnNthDone(n int) *cancelOnNthDone {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelOnNthDone{Context: ctx, cancel: cancel, n: n}
}

func (c *cancelOnNthDone) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.n > 0 && c.calls >= c.n {
		c.cancel()
	}
	return c.Context.Done()
}

func (c *cancelOnNthDone) doneCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestCancelledStatementLandsNothing pins what Patch 6's per-statement contexts
// rest on: a statement cut short by its own ctx fails with "context canceled",
// lands none of its writes, and leaves no transaction on the session for the
// next statement or BEGIN to adopt. It cancels a 200-row autocommit UPDATE at
// every point of its run — analysis, row loop and the commit inside its
// close — and requires every run to have either completed whole or been
// cancelled with nothing written. A driver that ran statements on any context
// but their caller's would cancel none of them.
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
			calls = cc.doneCalls()
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

	// Cancel at every Done call in turn, from the first until n passes the
	// end of the run: every point of the statement's run, analysis, row loop
	// and the commit inside its close alike, gets cut once. A cut inside the
	// close lands after the work is done, so the statement completes.
	cancelledInRowLoop := 0
	for n := 1; ; n++ {
		_, err := conn.ExecContext(ctx, "UPDATE t SET v = 0")
		require.NoError(t, err)

		execErr, calls, txLeft := run("UPDATE t SET v = 1", n)
		require.False(t, txLeft, "cancel at Done call %d left a transaction on the session", n)
		updated := countUpdated()
		if execErr == nil {
			require.Equal(t, rowCount, updated, "completed run cut at Done call %d", n)
			if calls < n {
				break
			}
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
	require.Positive(t, cancelledInRowLoop, "no cancellation landed past the fixed cost of %d Done calls: the statement does not consult its own ctx", fixedCost)
}
