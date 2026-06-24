package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The listing hot path hydrates every open epic's children. The contract under
// test is that the number of SQL round-trips it issues is fixed per recursion
// level and does NOT scale with the number of open epics: a tree of one epic
// and a tree of many epics of the same shape and depth must cost the identical
// number of queries. (links-query-efficiency-988d.1)
//
// This is observed behaviorally — real prepared statements are counted at the
// driver boundary — rather than by asserting the shape of the Go code, so the
// test fails if a future change reintroduces per-epic query fan-out regardless
// of how the loop is written.
func TestLifecycleHydrationQueryCountIsEpicCountIndependent(t *testing.T) {
	ctx := context.Background()

	// A single epic carrying one child, and five epics each carrying one child,
	// are the same depth and per-epic shape — only the epic count differs. Their
	// listing query counts must be equal.
	oneEpicCount := listingQueryCount(t, ctx, 1)
	fiveEpicCount := listingQueryCount(t, ctx, 5)

	if oneEpicCount != fiveEpicCount {
		t.Fatalf("listing query count scales with epic count: 1 epic = %d queries, 5 epics = %d queries; the per-recursion-level count must be epic-independent", oneEpicCount, fiveEpicCount)
	}
	if oneEpicCount == 0 {
		t.Fatalf("counting driver observed no queries during ListIssues; the wrapper is not on the read path")
	}
}

// listingQueryCount builds a store holding epicCount epics, each with one leaf
// child, then counts the SQL statements ListIssues issues over the full open
// backlog.
func listingQueryCount(t *testing.T, ctx context.Context, epicCount int) int64 {
	t.Helper()
	st := openIssueStore(t, ctx)
	for e := 0; e < epicCount; e++ {
		epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "qcount", IssueType: "epic", Priority: 1})
		if err != nil {
			t.Fatalf("CreateIssue(epic) error = %v", err)
		}
		if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child", Topic: "qcount", IssueType: "task", Priority: 0, ParentID: epic.ID, Placement: RankBottom}); err != nil {
			t.Fatalf("CreateIssue(child) error = %v", err)
		}
	}

	counter := swapInCountingDB(t, st)
	atomic.StoreInt64(counter, 0)
	issues, err := st.ListIssues(ctx, ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	// Guard against the count being trivially equal because nothing was hydrated.
	gotEpics := 0
	for _, issue := range issues {
		if model.IsContainerType(issue.IssueType) {
			gotEpics++
		}
	}
	if gotEpics != epicCount {
		t.Fatalf("expected %d epics in listing, got %d", epicCount, gotEpics)
	}
	return atomic.LoadInt64(counter)
}

// swapInCountingDB replaces the store's live Dolt connection with one whose
// driver counts every prepared statement, returning the counter. It mirrors
// reconnect()'s open-new-then-close-old rotation against the same Dolt root, so
// the replacement sees the data the original committed.
func swapInCountingDB(t *testing.T, st *Store) *int64 {
	t.Helper()
	var n int64
	// Dolt exposes its connection through driver.DriverContext, not the legacy
	// Driver.Open(name) path, so route through OpenConnector.
	dc, ok := st.db.Driver().(driver.DriverContext)
	if !ok {
		t.Fatalf("Dolt driver does not implement driver.DriverContext")
	}
	inner, err := dc.OpenConnector(buildDoltDSN(st.doltRootDir, st.workspaceID, true))
	if err != nil {
		t.Fatalf("OpenConnector error = %v", err)
	}
	next := sql.OpenDB(&countingConnector{inner: inner, n: &n})
	next.SetMaxOpenConns(1)
	next.SetMaxIdleConns(1)
	next.SetConnMaxLifetime(0)
	prev := st.db
	st.db = next
	// Dolt returns a benign context.Canceled on connection shutdown, which the
	// Store boundary normalizes away; mirror that here.
	if err := prev.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("close prior connection error = %v", err)
	}
	t.Cleanup(func() { _ = next.Close() })
	return &n
}

// countingConnector wraps the real Dolt driver and produces connections that
// tally prepared statements. It deliberately implements only the base
// driver.Conn surface (no QueryerContext/ExecerContext), so database/sql routes
// every QueryContext and ExecContext through Prepare — making one Prepare call
// equal to one query and counting exact.
type countingConnector struct {
	inner driver.Connector
	n     *int64
}

func (c *countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countingConn{inner: conn, n: c.n}, nil
}

func (c *countingConnector) Driver() driver.Driver { return c.inner.Driver() }

type countingConn struct {
	inner driver.Conn
	n     *int64
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	atomic.AddInt64(c.n, 1)
	return c.inner.Prepare(query)
}

func (c *countingConn) Close() error { return c.inner.Close() }

func (c *countingConn) Begin() (driver.Tx, error) { return c.inner.Begin() } //nolint:staticcheck // base driver.Conn surface is intentional; see type doc.
