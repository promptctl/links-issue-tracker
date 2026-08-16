// Copyright 2026 promptctl
//
// SPDX-License-Identifier: MIT
//
// This file is original work by the links-issue-tracker project, added to this
// vendored copy of github.com/dolthub/driver. It is not derived from upstream
// dolthub/driver and it is not derived from github.com/go-sql-driver/mysql,
// whose MySQLError this replaces — see README.lit-patch.md, Patch 4.

package embedded

import "strconv"

// MySQLError is the error this driver reports when the embedded engine rejects a
// statement. It carries the two fields the MySQL protocol's ERR packet gives a
// client a reason to branch on: the server error code, and the human-readable
// message describing what the server refused.
//
// [LAW:types-are-the-program] The code is uint16 because that is the protocol's
// own width for it — two bytes on the wire — so no value this type can hold is
// one the server could not have sent. The type carries no SQL state: nothing in
// this driver produces one and nothing in lit reads one, and a field that is
// always empty is a state the type would admit but the domain never occupies.
//
// [LAW:one-source-of-truth] This is the single definition of "a MySQL error
// crossing the driver boundary". translateError is its only producer; callers
// reach it through errors.As, never by constructing one themselves.
type MySQLError struct {
	// Number is the MySQL server error code, e.g. 1146 for a table that does
	// not exist. It is the field callers match on: the code is stable across
	// server versions and locales, while Message is neither.
	Number uint16

	// Message is the server's description of the failure, already interpolated
	// with whatever identifiers it names. It is for humans to read, not for
	// callers to match on.
	Message string
}

// Error renders the error in MySQL's own conventional form, "Error <code>:
// <message>" — the shape the mysql client and every tool built against the
// protocol print, so a code copied out of a lit diagnostic matches what a user
// finds when they search for it.
//
// [LAW:comments-carry-meaning] The format string looks arbitrary and is not:
// it is a wire-protocol convention this type is deliberately conforming to.
func (e *MySQLError) Error() string {
	return "Error " + strconv.FormatUint(uint64(e.Number), 10) + ": " + e.Message
}
