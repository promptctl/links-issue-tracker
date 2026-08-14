package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

// A genuine RowsAffected() error must surface to the caller as the failure it
// is, not be swallowed and misread as a real zero-rows "not found" result.
// This drives ClearParent's DELETE through a driver that executes the
// statement for real but forces the result's RowsAffected() to fail,
// proving the two outcomes are now distinguishable. (links-store-mb6e.1)
func TestClearParentSurfacesGenuineRowsAffectedError(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	parent, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Parent", Topic: "rows", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child", Topic: "rows", IssueType: "task", Priority: 0, ParentID: parent.ID, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}

	causeErr := errors.New("synthetic driver failure reading affected rows")
	swapInExecErrDB(t, st, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, causeErr)

	err = st.ClearParent(ctx, child.ID)
	if err == nil {
		t.Fatalf("ClearParent() error = nil, want the injected RowsAffected failure")
	}
	var notFound NotFoundError
	if errors.As(err, &notFound) {
		t.Fatalf("ClearParent() masked a genuine RowsAffected error as NotFoundError: %v", err)
	}
	if !strings.Contains(err.Error(), causeErr.Error()) {
		t.Fatalf("ClearParent() error = %v, want it to wrap %q", err, causeErr)
	}
}

// The same masking risk exists in RemoveLabel's DELETE FROM labels statement;
// this proves its RowsAffected() error is likewise surfaced rather than
// misread as a real zero-rows "label not found" result. (links-store-mb6e.1)
func TestRemoveLabelSurfacesGenuineRowsAffectedError(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Labeled", Topic: "rows", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := st.AddLabel(ctx, AddLabelInput{IssueID: issue.ID, Name: "critical", CreatedBy: "test"}); err != nil {
		t.Fatalf("AddLabel() error = %v", err)
	}

	causeErr := errors.New("synthetic driver failure reading affected rows")
	swapInExecErrDB(t, st, `DELETE FROM labels WHERE issue_id = ? AND label = ?`, causeErr)

	_, err = st.RemoveLabel(ctx, issue.ID, "critical")
	if err == nil {
		t.Fatalf("RemoveLabel() error = nil, want the injected RowsAffected failure")
	}
	var notFound NotFoundError
	if errors.As(err, &notFound) {
		t.Fatalf("RemoveLabel() masked a genuine RowsAffected error as NotFoundError: %v", err)
	}
	if !strings.Contains(err.Error(), causeErr.Error()) {
		t.Fatalf("RemoveLabel() error = %v, want it to wrap %q", err, causeErr)
	}
}

// swapInExecErrDB replaces the store's live Dolt connection with one that,
// whenever it prepares a statement whose text contains match, executes it for
// real (so the underlying row is genuinely deleted) but forces its
// Result.RowsAffected() to return causeErr instead of the true count. It
// mirrors swapInCountingDB's connector-wrapping technique in
// lifecycle_hydration_query_count_test.go, applied to fault injection instead
// of counting.
func swapInExecErrDB(t *testing.T, st *Store, match string, causeErr error) {
	t.Helper()
	inner, err := newDoltConnector(st.doltRootDir, st.workspaceID, doltDatabaseName, st.access)
	if err != nil {
		t.Fatalf("newDoltConnector error = %v", err)
	}
	next := sql.OpenDB(&execErrConnector{inner: inner, match: match, causeErr: causeErr})
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
}

type execErrConnector struct {
	inner    driver.Connector
	match    string
	causeErr error
}

func (c *execErrConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &execErrConn{inner: conn, match: c.match, causeErr: c.causeErr}, nil
}

func (c *execErrConnector) Driver() driver.Driver { return c.inner.Driver() }

// Close forwards to the wrapped connector so sql.DB.Close really closes the
// inner engine — with the singleton cache bypassed, an unforwarded Close would
// leak the engine (and its journal lock) for the rest of the test process.
func (c *execErrConnector) Close() error {
	if closer, ok := c.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// execErrConn deliberately implements only the base driver.Conn surface (no
// QueryerContext/ExecerContext), so database/sql routes every ExecContext
// through Prepare — the same routing guarantee countingConn relies on —
// letting Prepare match on query text and swap in a fault-injecting Stmt.
type execErrConn struct {
	inner    driver.Conn
	match    string
	causeErr error
}

func (c *execErrConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(query, c.match) {
		return stmt, nil
	}
	return &execErrStmt{inner: stmt, causeErr: c.causeErr}, nil
}

func (c *execErrConn) Close() error { return c.inner.Close() }

func (c *execErrConn) Begin() (driver.Tx, error) { return c.inner.Begin() } //nolint:staticcheck // base driver.Conn surface is intentional; see type doc.

// execErrStmt runs the wrapped statement exactly as the real driver would —
// including via ExecContext, which database/sql prefers when available — and
// only substitutes the Result, so the DELETE it wraps still genuinely executes.
type execErrStmt struct {
	inner    driver.Stmt
	causeErr error
}

func (s *execErrStmt) Close() error  { return s.inner.Close() }
func (s *execErrStmt) NumInput() int { return s.inner.NumInput() }

func (s *execErrStmt) Exec(args []driver.Value) (driver.Result, error) { //nolint:staticcheck // deprecated legacy path must still be wrapped for non-context callers.
	res, err := s.inner.Exec(args) //nolint:staticcheck // see method doc.
	if err != nil {
		return nil, err
	}
	return &execErrResult{inner: res, causeErr: s.causeErr}, nil
}

func (s *execErrStmt) Query(args []driver.Value) (driver.Rows, error) { //nolint:staticcheck // see Exec.
	return s.inner.Query(args) //nolint:staticcheck // see Exec.
}

func (s *execErrStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if ec, ok := s.inner.(driver.StmtExecContext); ok {
		res, err := ec.ExecContext(ctx, args)
		if err != nil {
			return nil, err
		}
		return &execErrResult{inner: res, causeErr: s.causeErr}, nil
	}
	// The wrapped statement predates context support; fall back to the legacy
	// path the same way database/sql itself would if execErrStmt did not
	// implement ExecContext at all.
	res, err := s.inner.Exec(namedValuesToValues(args)) //nolint:staticcheck // see method doc.
	if err != nil {
		return nil, err
	}
	return &execErrResult{inner: res, causeErr: s.causeErr}, nil
}

// namedValuesToValues converts positional NamedValue arguments back into the
// legacy Value slice driver.Stmt.Exec expects. NamedValue.Value has already
// been through the driver's value converter by this point, so this is a pure
// reordering by Ordinal, not a conversion.
func namedValuesToValues(named []driver.NamedValue) []driver.Value {
	args := make([]driver.Value, len(named))
	for _, nv := range named {
		args[nv.Ordinal-1] = nv.Value
	}
	return args
}

// execErrResult reports the query as having genuinely executed — the DELETE
// really ran, and a real zero-rows case still returns 0 with no error from
// the inner result — while RowsAffected() unconditionally fails. That is the
// exact fault ClearParent and RemoveLabel now surface instead of masking as
// NotFound.
type execErrResult struct {
	inner    driver.Result
	causeErr error
}

func (r *execErrResult) LastInsertId() (int64, error) { return r.inner.LastInsertId() }
func (r *execErrResult) RowsAffected() (int64, error) { return 0, r.causeErr }
