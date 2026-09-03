package store

// The SQL-DDL text-parsing toolkit: pure functions from migration SQL text to
// values — ALTER-ADD-COLUMN target matching, statement termination, goose Up
// section extraction, CREATE TABLE column parsing, and the shared quote-aware
// scanning they build on. Nothing here touches a Store, the database, or the
// embedded registry; migration_runner.go composes these over registry content.
//
// [LAW:decomposition] Split out of migration_runner.go (links-store-mb6e.5) so
// the phase-classification/quarantine orchestration engine and this
// self-contained parsing toolkit are separately readable and testable.

import (
	"fmt"
	"regexp"
	"strings"
)

// alterAddColumnRe matches an `ALTER TABLE <table> ADD COLUMN <column>`
// statement's target table and column, tolerating optional backtick
// quoting. It recognizes only the plain ADD COLUMN <name> <type> shape —
// the one every migration in this registry, and the README's own migration
// skeleton, uses. parseAddColumnTargets below asserts that every literal
// "ADD COLUMN" occurrence in a Up section was actually parsed by this
// pattern, so a shape it cannot parse (IF NOT EXISTS, a parenthesized
// multi-column list) fails loudly rather than silently registering as "no
// content to verify" for that version.
//
// One shape is a deliberate, non-silent gap rather than a parsed one:
// MySQL's `ADD <col> <type>` with no COLUMN keyword contains no "ADD
// COLUMN" text for the literal-count assertion to catch, and is ambiguous
// with `ADD CONSTRAINT` / `ADD INDEX` / `ADD KEY` / `ADD UNIQUE` without a
// fuller statement parser to disambiguate. No migration here uses it; a
// future one that does needs a matching case added to this parser, or its
// content is invisible to verifyAppliedVersionsMatchRegistry.
var alterAddColumnRe = regexp.MustCompile("(?is)ALTER\\s+TABLE\\s+`?([A-Za-z_][A-Za-z0-9_]*)`?\\s+ADD\\s+COLUMN\\s+`?([A-Za-z_][A-Za-z0-9_]*)`?")

// sqlKeywordAfterAddColumn holds identifier-shaped tokens that can
// immediately follow "ADD COLUMN" in a clause shape alterAddColumnRe does
// not intend to parse, but whose leading word is itself a valid identifier
// and so would otherwise be greedily captured as a bogus column name — right
// now, just the "IF" of "ADD COLUMN IF NOT EXISTS". parseAddColumnTargets
// discards a match landing on one of these so its literal-count assertion
// sees the occurrence as unparsed (and fails loudly) rather than accepting
// a nonsense target like "issues.if".
var sqlKeywordAfterAddColumn = map[string]bool{"if": true}

type tableColumnTarget struct {
	table  string
	column string
	// stmt is the exact ALTER TABLE ... ADD COLUMN ... statement text this
	// target was parsed from, terminated at its closing ';'. repairVersionContentDrift
	// re-executes it verbatim, so a repaired column is byte-identical to what
	// applying the registry's own migration would have produced.
	stmt string
}

// parseAddColumnTargets extracts every ALTER TABLE ... ADD COLUMN target
// from a migration's Up section text.
//
// [LAW:no-silent-failure] It fails loudly, naming the file, when the Up
// section contains more literal (case-insensitive) "ADD COLUMN" occurrences
// than alterAddColumnRe actually parsed (after sqlKeywordAfterAddColumn
// discards keyword-shaped false captures) — a shape like "ADD COLUMN IF NOT
// EXISTS" or a parenthesized multi-column list would otherwise silently
// register zero (or, for the IF case, one WRONG) target for that version,
// which is indistinguishable from a migration that legitimately adds no
// column (e.g. an index- or backfill-only migration) and would let a real
// content mismatch on that version go undetected. The same loud-failure gate
// also protects the repair path: a target whose statement text cannot be
// isolated is never silently skipped either.
func parseAddColumnTargets(name, up string) ([]tableColumnTarget, error) {
	var adds []tableColumnTarget
	for _, m := range alterAddColumnRe.FindAllStringSubmatchIndex(up, -1) {
		column := strings.ToLower(up[m[4]:m[5]])
		if sqlKeywordAfterAddColumn[column] {
			continue
		}
		stmt, ok := terminatedStatement(up, m[0])
		if !ok {
			return nil, fmt.Errorf(
				"migration %q: ADD COLUMN statement starting at byte %d has no terminating ';' — "+
					"cannot isolate it for repair",
				name, m[0],
			)
		}
		adds = append(adds, tableColumnTarget{table: strings.ToLower(up[m[2]:m[3]]), column: column, stmt: stmt})
	}
	if literal := strings.Count(strings.ToUpper(up), "ADD COLUMN"); literal > len(adds) {
		return nil, fmt.Errorf(
			"migration %q: found %d \"ADD COLUMN\" occurrence(s) in its Up section but "+
				"alterAddColumnRe recognized only %d — a form such as \"ADD COLUMN IF NOT "+
				"EXISTS\" or a parenthesized multi-column list is not parsed; widen "+
				"alterAddColumnRe or rewrite the migration to the plain ADD COLUMN <name> "+
				"<type> shape",
			name, literal, len(adds),
		)
	}
	return adds, nil
}

// terminatedStatement returns the text from start to (and including) the
// next UNQUOTED ';' in s, trimmed, or ok=false if none exists. Quote-aware
// like parenBlock/splitTopLevel below, so a semicolon inside a string
// literal (e.g. a future migration's `DEFAULT ';'`) cannot truncate the
// statement mid-value and hand repairVersionContentDrift invalid SQL.
//
// Like parenBlock/splitTopLevel, this recognizes only doubled-quote (”)
// escaping, not backslash escapes (\') — the only form any migration in
// this registry uses. A migration whose ADD COLUMN default contains a
// backslash-escaped quote would toggle out of the string early and this
// would overshoot the real statement boundary; widen all three quote
// scanners together if that shape is ever needed, not just this one.
func terminatedStatement(s string, start int) (stmt string, ok bool) {
	inQuote := false
	for i := start; i < len(s); i++ {
		switch {
		case inQuote:
			if s[i] == '\'' {
				inQuote = false
			}
		case s[i] == '\'':
			inQuote = true
		case s[i] == ';':
			return strings.TrimSpace(s[start : i+1]), true
		}
	}
	return "", false
}

// gooseUpSection returns the SQL between the goose Up and Down markers, so the
// parser never reads the Down (DROP TABLE) statements as table definitions.
func gooseUpSection(sql string) string {
	lower := strings.ToLower(sql)
	up := strings.Index(lower, "-- +goose up")
	if up < 0 {
		return sql
	}
	body := sql[up:]
	if down := strings.Index(strings.ToLower(body), "-- +goose down"); down >= 0 {
		return body[:down]
	}
	return body
}

// parseCreateTableColumns extracts table -> column-names from CREATE TABLE
// statements. It reads only column identifiers (the first token of each
// top-level item that is not a table-level constraint keyword); CREATE INDEX
// and everything else is ignored. ASCII-lowercasing preserves byte indices, so
// the case-insensitive keyword scan and the original-text slicing stay aligned.
func parseCreateTableColumns(sql string) map[string][]string {
	out := map[string][]string{}
	lower := strings.ToLower(sql)
	const kw = "create table"
	for pos := 0; ; {
		i := strings.Index(lower[pos:], kw)
		if i < 0 {
			break
		}
		cursor := pos + i + len(kw)
		name, afterName := firstIdentifier(sql[cursor:])
		open := strings.IndexByte(afterName, '(')
		if name == "" || open < 0 {
			pos = cursor
			continue
		}
		consumedToName := len(sql[cursor:]) - len(afterName)
		body, blockLen := parenBlock(afterName[open:])
		out[strings.ToLower(name)] = columnNames(body)
		pos = cursor + consumedToName + open + blockLen
	}
	return out
}

// columnNames returns the column identifiers in a CREATE TABLE body, skipping
// table-level constraint clauses.
func columnNames(body string) []string {
	var cols []string
	for _, item := range splitTopLevel(body) {
		name, _ := firstIdentifier(item)
		if name == "" || isConstraintKeyword(name) {
			continue
		}
		cols = append(cols, strings.ToLower(name))
	}
	return cols
}

// splitTopLevel splits a CREATE TABLE body at depth-0, unquoted commas, so a
// CHECK clause's internal commas (inside parens or string literals) do not
// fragment a single item.
func splitTopLevel(body string) []string {
	var parts []string
	depth, inQuote, start := 0, false, 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, body[start:])
}

// parenBlock takes a string beginning with '(' and returns the content between
// it and its matching ')', plus the total bytes consumed (including both
// parens). Quote- and depth-aware. An unbalanced input yields an empty body.
func parenBlock(s string) (string, int) {
	depth, inQuote := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], i + 1
			}
		}
	}
	return "", len(s)
}

// firstIdentifier returns the leading SQL identifier (backticks stripped) and
// the remainder after it, skipping leading whitespace.
func firstIdentifier(s string) (string, string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	start := i
	for i < len(s) && (isIdentByte(s[i]) || s[i] == '`') {
		i++
	}
	return strings.Trim(s[start:i], "`"), s[i:]
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// isConstraintKeyword reports whether a CREATE TABLE item's leading token names
// a table-level constraint clause rather than a column.
func isConstraintKeyword(token string) bool {
	switch strings.ToUpper(token) {
	case "CONSTRAINT", "PRIMARY", "FOREIGN", "KEY", "CHECK", "UNIQUE", "INDEX":
		return true
	default:
		return false
	}
}
