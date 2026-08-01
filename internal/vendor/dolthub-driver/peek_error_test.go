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
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	gms "github.com/dolthub/go-mysql-server/sql"
)

// This is the lit-local patch guarding against the swallowed first-row query error
// described in README.lit-patch.md (Patch 2) and lit ticket promptctl-dolt-driver-iip.
// It pins the driver contract independently of any consumer: a first-row query error
// is surfaced by Next(), never silently converted into an empty result set.

// firstRowErrIter reproduces the non-idempotent iterator behind the bug: its first
// Next() raises a real (non-EOF) error, and every subsequent Next() returns io.EOF —
// exactly how DOLT_MERGE_BASE behaves for refs with no common ancestor (it raises
// "Error 1105: no common ancestor" on the first row, then EOF). A driver that discards
// the first error and re-drives the iterator therefore observes io.EOF and reports an
// empty result set.
type firstRowErrIter struct {
	err   error
	calls int
}

func (it *firstRowErrIter) Next(*gms.Context) (gms.Row, error) {
	it.calls++
	if it.calls == 1 {
		return nil, it.err
	}
	return nil, io.EOF
}

func (it *firstRowErrIter) Close(*gms.Context) error { return nil }

// TestPeekReturnsFirstRowErrorWithoutBuffering documents the root of the bug: Peek()
// returns the first-row error but buffers nothing, so a naive later Next() re-drives
// the underlying iterator. This is why discarding Peek()'s error is unsafe.
func TestPeekReturnsFirstRowErrorWithoutBuffering(t *testing.T) {
	sentinel := errors.New("no common ancestor")
	p := peekableRowIter{iter: &firstRowErrIter{err: sentinel}}

	row, err := p.Peek(nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Peek() error = %v, want %v", err, sentinel)
	}
	if row != nil {
		t.Fatalf("Peek() row = %v, want nil on error", row)
	}
	if len(p.peeks) != 0 {
		t.Fatalf("Peek() buffered %d rows on error, want 0 — the empty buffer is what "+
			"makes a later Next() re-drive the iterator and observe io.EOF", len(p.peeks))
	}
}

// TestDoltRowsNextSurfacesFirstRowError is the core regression: a doltRows wired the
// way doltStmt.Query wires it (peek error carried on doltRows.err) surfaces that error
// from Next() rather than swallowing it into io.EOF, and does not re-drive the iterator.
func TestDoltRowsNextSurfacesFirstRowError(t *testing.T) {
	sentinel := errors.New("no common ancestor")
	iter := &firstRowErrIter{err: sentinel}
	peek := &peekableRowIter{iter: iter}

	// Mirror doltStmt.Query: Peek() the first row, then carry the error via the single
	// mapping rule under test.
	_, peekErr := peek.Peek(nil)
	rows := &doltRows{rowIter: peek, err: peekResultError(peekErr)}

	got := rows.Next(make([]driver.Value, 1))
	if got == nil || errors.Is(got, io.EOF) {
		t.Fatalf("Next() = %v, want the surfaced query error, not nil/io.EOF (the swallow bug)", got)
	}
	if !strings.Contains(got.Error(), "no common ancestor") {
		t.Fatalf("Next() error = %v, want it to carry %q", got, "no common ancestor")
	}
	if iter.calls != 1 {
		t.Fatalf("underlying Next() called %d times, want 1 — a captured peek error must "+
			"not re-drive the iterator", iter.calls)
	}
}

// TestPeekResultErrorMapping pins the three-way rule that decides which peek outcomes
// become a doltRows error: only a real (non-EOF) error does. io.EOF and a nil error are
// valid empty/complete result sets and must yield no error, or every legitimately-empty
// query would fail.
func TestPeekResultErrorMapping(t *testing.T) {
	if got := peekResultError(nil); got != nil {
		t.Fatalf("peekResultError(nil) = %v, want nil", got)
	}
	if got := peekResultError(io.EOF); got != nil {
		t.Fatalf("peekResultError(io.EOF) = %v, want nil — an empty result set is not an error", got)
	}
	real := errors.New("no common ancestor")
	got := peekResultError(real)
	if got == nil {
		t.Fatalf("peekResultError(real error) = nil, want the error surfaced")
	}
	if !strings.Contains(got.Error(), "no common ancestor") {
		t.Fatalf("peekResultError(real error) = %v, want it to carry %q", got, "no common ancestor")
	}
}
