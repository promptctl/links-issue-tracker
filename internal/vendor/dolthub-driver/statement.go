// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"

	"github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/dolthub/dolt/go/cmd/dolt/commands/engine"
	gms "github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	"github.com/dolthub/vitess/go/sqltypes"
)

// doltMultiStmt represents a collection of statements to be executed against a
// Dolt database.
type doltMultiStmt struct {
	stmts []*doltStmt
}

var (
	_ driver.Stmt             = (*doltMultiStmt)(nil)
	_ driver.StmtExecContext  = (*doltMultiStmt)(nil)
	_ driver.StmtQueryContext = (*doltMultiStmt)(nil)
)

func (d doltMultiStmt) Close() error {
	var retErr error
	for _, stmt := range d.stmts {
		if err := stmt.Close(); err != nil {
			retErr = err
		}
	}

	return retErr
}

func (d doltMultiStmt) NumInput() int {
	return -1
}

func (d doltMultiStmt) Exec(args []driver.Value) (driver.Result, error) {
	return d.execValues(context.Background(), args)
}

// ExecContext executes every statement in turn on ctx, stopping at the first error.
func (d doltMultiStmt) ExecContext(ctx context.Context, named []driver.NamedValue) (driver.Result, error) {
	args, err := namedValuesToValues(named)
	if err != nil {
		return nil, err
	}
	return d.execValues(ctx, args)
}

func (d doltMultiStmt) execValues(ctx context.Context, args []driver.Value) (result driver.Result, err error) {
	for _, stmt := range d.stmts {
		result, err = stmt.execValues(ctx, args)
		if err != nil {
			// If any error occurs, return the error and don't execute any more statements
			return nil, err
		}
	}

	// Otherwise, return the last result, to match the MySQL driver's behavior
	return result, nil
}

func (d doltMultiStmt) Query(args []driver.Value) (driver.Rows, error) {
	return d.queryValues(context.Background(), args)
}

// QueryContext runs every statement on ctx and returns their result sets.
func (d doltMultiStmt) QueryContext(ctx context.Context, named []driver.NamedValue) (driver.Rows, error) {
	args, err := namedValuesToValues(named)
	if err != nil {
		return nil, err
	}
	return d.queryValues(ctx, args)
}

func (d doltMultiStmt) queryValues(ctx context.Context, args []driver.Value) (driver.Rows, error) {
	var ret doltMultiRows
	for _, stmt := range d.stmts {
		ret.rowSets = append(ret.rowSets, func() *doltRows {
			rows, err := stmt.queryValues(ctx, args)
			if err != nil {
				return &doltRows{err: err}
			}
			return rows
		})
	}
	for ret.currentIdx = 0; ret.currentIdx < len(ret.rowSets); ret.currentIdx++ {
		rows := ret.rowSets[ret.currentIdx]()
		if rows.err != nil {
			return nil, rows.err
		} else if rows.isQueryResultSet {
			ret.currentRowSet = rows
			break
		} else {
			err := rows.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	return &ret, nil
}

// doltStmt represents a single statement to be executed against a Dolt database.
//
// [LAW:no-ambient-temporal-coupling] Modified by lit (README.lit-patch.md,
// Patch 6): session is the connection's session context, and each execution
// runs on session.WithContext(ctx) for the ctx its caller passed, so a
// caller's cancellation or deadline governs that statement and no other.
// Upstream implemented only Exec and Query, which ran every statement on the
// context Connect was called with.
type doltStmt struct {
	se      *engine.SqlEngine
	session *gms.Context
	query   string
}

var (
	_ driver.Stmt             = (*doltStmt)(nil)
	_ driver.StmtExecContext  = (*doltStmt)(nil)
	_ driver.StmtQueryContext = (*doltStmt)(nil)
)

// Close closes the statement.
func (stmt *doltStmt) Close() error {
	return nil
}

// NumInput returns the number of placeholder parameters.
func (stmt *doltStmt) NumInput() int {
	return -1
}

func argsToBindings(args []driver.Value) (map[string]sqlparser.Expr, error) {
	bindings := make(map[string]sqlparser.Expr)
	for i := range args {
		vIdx := "v" + strconv.FormatInt(int64(i+1), 10)
		bv, err := sqltypes.BuildBindVariable(args[i])
		if err != nil {
			return nil, err
		}
		v, err := sqltypes.BindVariableToValue(bv)
		if err != nil {
			return nil, err
		}
		bindings[vIdx], err = sqlparser.ExprFromValue(v)
		if err != nil {
			return nil, err
		}
	}

	return bindings, nil
}

// namedValuesToValues refuses named parameters, which the statement's
// positional bindings (v1, v2, ...) cannot carry, as database/sql does for a
// driver that takes only positional arguments.
func namedValuesToValues(named []driver.NamedValue) ([]driver.Value, error) {
	args := make([]driver.Value, len(named))
	for i, nv := range named {
		if nv.Name != "" {
			return nil, errors.New("sql: driver does not support the use of Named Parameters")
		}
		args[i] = nv.Value
	}
	return args, nil
}

// Exec executes a query that doesn't return rows, such as an INSERT or UPDATE.
func (stmt *doltStmt) Exec(args []driver.Value) (driver.Result, error) {
	return stmt.execValues(context.Background(), args)
}

// ExecContext executes a query that doesn't return rows on ctx.
func (stmt *doltStmt) ExecContext(ctx context.Context, named []driver.NamedValue) (driver.Result, error) {
	args, err := namedValuesToValues(named)
	if err != nil {
		return nil, err
	}
	return stmt.execValues(ctx, args)
}

func (stmt *doltStmt) execValues(ctx context.Context, args []driver.Value) (driver.Result, error) {
	gmsCtx := stmt.session.WithContext(ctx)
	sch, itr, err := stmt.execWithArgs(gmsCtx, args)
	if err != nil {
		return nil, translateError(err)
	}

	res := newResult(gmsCtx, sch, itr)
	if res.err != nil {
		return nil, res.err
	}

	return res, nil
}

func (stmt *doltStmt) execWithArgs(gmsCtx *gms.Context, args []driver.Value) (gms.Schema, gms.RowIter, error) {
	bindings, err := argsToBindings(args)
	if err != nil {
		return nil, nil, err
	}

	sch, itr, _, err := stmt.se.GetUnderlyingEngine().QueryWithBindings(gmsCtx, stmt.query, nil, bindings, nil)
	if err != nil {
		return nil, nil, err
	}
	return sch, itr, nil
}

// Query executes a query that may return rows, such as a SELECT
func (stmt *doltStmt) Query(args []driver.Value) (driver.Rows, error) {
	return stmt.queryValues(context.Background(), args)
}

// QueryContext executes a query that may return rows on ctx; the rows are read on ctx too.
func (stmt *doltStmt) QueryContext(ctx context.Context, named []driver.NamedValue) (driver.Rows, error) {
	args, err := namedValuesToValues(named)
	if err != nil {
		return nil, err
	}
	return stmt.queryValues(ctx, args)
}

func (stmt *doltStmt) queryValues(ctx context.Context, args []driver.Value) (*doltRows, error) {
	gmsCtx := stmt.session.WithContext(ctx)
	var sch gms.Schema
	var rowIter gms.RowIter
	var err error

	if len(args) != 0 {
		sch, rowIter, err = stmt.execWithArgs(gmsCtx, args)
	} else {
		sch, rowIter, _, err = stmt.se.Query(gmsCtx, stmt.query)
	}
	if err != nil {
		return nil, translateError(err)
	}

	// Wrap the result iterator in a peekableRowIter and call Peek() to read the first row from the result iterator.
	// This is necessary for insert operations, since the insert happens inside the result iterator logic. Without
	// calling this now, insert statements and some DML statements (e.g. CREATE PROCEDURE) would not be executed yet,
	// and future statements in a multi-statement query that depend on those results would fail.
	// If an error does occur, we want that error to be returned in the Next() codepath, not here.
	peekIter := peekableRowIter{iter: rowIter}
	row, peekErr := peekIter.Peek(gmsCtx)

	return &doltRows{
		sch:              sch,
		rowIter:          &peekIter,
		gmsCtx:           gmsCtx,
		isQueryResultSet: isQueryResultSet(row),
		err:              peekResultError(peekErr),
	}, nil
}

// peekResultError maps the error returned by Query()'s first-row Peek() to the error
// to carry on doltRows. A non-EOF error must not be dropped: Peek() does not buffer a
// failed row, so a later Next() would re-invoke the underlying iterator and can observe
// a different outcome (e.g. io.EOF), silently converting a real query error into an
// empty result set. Such an error is translated and surfaced from Next(). io.EOF (and
// no error) is a valid empty result set, not a failure, and yields a nil doltRows.err.
func peekResultError(peekErr error) error {
	if peekErr == nil || peekErr == io.EOF {
		return nil
	}
	return translateError(peekErr)
}

// isQueryResultSet returns true if the specified |row| is a valid result set for a query. If row only contains
// one column and is an OkResult, or if row has zero columns, then the statement that generated this row was not
// a query.
func isQueryResultSet(row gms.Row) bool {
	// If row is nil, return true since this could still be a valid, empty result set.
	if row == nil {
		return true
	}

	if len(row) == 1 {
		if _, ok := row[0].(types.OkResult); ok {
			return false
		}
	} else if len(row) == 0 {
		return false
	}

	return true
}
