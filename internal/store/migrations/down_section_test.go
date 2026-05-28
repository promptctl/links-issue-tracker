package migrations

import (
	"strings"
	"testing"
)

// hasDownSection reports whether a goose migration file contains a `+goose Down`
// section followed by at least one non-empty, non-comment SQL statement before
// EOF or the next `+goose Up`.
//
// [LAW:types-are-the-program] "Migration is invertible" is a property of the
// file's bytes; this predicate is the type-checker. A file that fails this
// check is not a migration the downgrade pipeline can invert, and the CI gate
// makes that an unrepresentable shape in the registry.
//
// [LAW:one-source-of-truth] The predicate works on the same bytes goose reads.
// There is no parallel "registry of invertible migrations" that could drift.
func hasDownSection(data []byte) bool {
	// Walk the file line-by-line, carrying block-comment state across lines.
	// Directives are recognized only when they appear as a real source line
	// (not inside /* ... */); executable content is recognized only outside
	// every comment form. The shape mirrors what goose actually parses, so
	// adversarial inputs like `-- +goose D/*x*/own` cannot fabricate a
	// directive by collapsing under a naive substring strip.
	lines := strings.Split(string(data), "\n")
	inBlock := false
	downSeen := false
	for _, line := range lines {
		// First: account for any open block comment that started on a
		// previous line. We do not advance the block-comment state via
		// directive detection or content extraction; lineContent does both.
		content, exitedBlock := lineContent(line, inBlock)
		// Directive detection: only on lines that were NOT inside a block
		// comment at start. A directive cannot live inside /* ... */; if
		// the line opens a block comment partway through, the directive
		// would have to come before that opening, which is what
		// rawNonBlockPrefix preserves before any stripping. We re-derive
		// it cheaply: a directive must be the entire (trimmed) line, with
		// no /* ... */ artifacts.
		if !inBlock && isGooseDirective(line, "down") {
			downSeen = true
			inBlock = exitedBlock
			continue
		}
		if !inBlock && isGooseDirective(line, "up") {
			if downSeen {
				// Down section ended without yielding executable content.
				return false
			}
			inBlock = exitedBlock
			continue
		}
		inBlock = exitedBlock
		if !downSeen {
			continue
		}
		if strings.TrimSpace(content) != "" {
			return true
		}
	}
	return false
}

// isGooseDirective reports whether a source line is exactly the `+goose <kind>`
// directive (case-insensitive, surrounding whitespace tolerated). The accept
// shape mirrors what goose actually parses: a `-- +goose <kind>` line whose
// trimmed body is literally that directive, not any line that contains the
// substring or whose interior `/* ... */` could be stripped to match.
func isGooseDirective(line, kind string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(strings.ToLower(trimmed), "-- ") {
		return false
	}
	body := strings.TrimSpace(trimmed[len("-- "):])
	return strings.EqualFold(body, "+goose "+kind)
}

// lineContent strips comments from one source line and returns the executable
// content plus the inBlock state at end-of-line. Stripping rules:
//   - If inBlock at start, search for `*/`; bytes before the closer are
//     comment; bytes after are processed normally on the rest of the line.
//   - Outside a block, the FIRST occurrence of `--`, `#`, or `/*` wins:
//     `--` and `#` terminate the line as comment; `/*` opens a block (and
//     `lineContent` recurses on whatever comes after a same-line `*/`).
//   - Quote-string awareness is out of scope: migrations don't legitimately
//     embed comment markers inside string literals in their Down sections,
//     and if one does, the gate is conservative (extra stripping → reject),
//     which is safe given the gate's purpose.
func lineContent(line string, inBlock bool) (string, bool) {
	var out strings.Builder
	rest := line
	if inBlock {
		idx := strings.Index(rest, "*/")
		if idx < 0 {
			return "", true
		}
		rest = rest[idx+2:]
	}
	for {
		// Find the earliest of `--`, `#`, `/*`.
		lineDash := strings.Index(rest, "--")
		hash := strings.Index(rest, "#")
		block := strings.Index(rest, "/*")
		next, kind := earliest(lineDash, hash, block)
		if next < 0 {
			out.WriteString(rest)
			return out.String(), false
		}
		out.WriteString(rest[:next])
		switch kind {
		case "--", "#":
			return out.String(), false
		case "/*":
			rest = rest[next+2:]
			closer := strings.Index(rest, "*/")
			if closer < 0 {
				return out.String(), true
			}
			rest = rest[closer+2:]
		}
	}
}

func earliest(a, b, c int) (int, string) {
	pick := -1
	kind := ""
	for _, p := range []struct {
		idx  int
		kind string
	}{{a, "--"}, {b, "#"}, {c, "/*"}} {
		if p.idx < 0 {
			continue
		}
		if pick < 0 || p.idx < pick {
			pick = p.idx
			kind = p.kind
		}
	}
	return pick, kind
}

// TestEveryMigrationHasDownSection enforces the +goose Down discipline that
// the lit-downgrade epic requires: every migration in the embedded registry
// must ship a Down section with at least one statement, so goose.DownTo can
// reverse arbitrary forward progress.
//
// [LAW:single-enforcer] This is the single static enforcer of the discipline;
// no other code checks for Down-section presence. The runtime sibling
// (TestEveryMigrationDownIsExercised, in internal/store) proves the section
// is not merely present but also actually runs.
func TestEveryMigrationHasDownSection(t *testing.T) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded registry: %v", err)
	}
	var sqlFiles int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		sqlFiles++
		data, err := FS.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %q: %v", entry.Name(), err)
		}
		if !hasDownSection(data) {
			t.Errorf(`migration %q has no `+"`+goose Down`"+` section, or its Down body is empty / comment-only.

Every migration in internal/store/migrations/ MUST ship a Down section so
the lit downgrade pipeline (links-downgrade-t244) can invert it. The Down
section must contain at least one non-empty, non-comment SQL statement
between the `+"`-- +goose Down`"+` marker and EOF (or the next
`+"`-- +goose Up`"+` marker).

If this migration loses information (e.g. drops a column), the Down
section should either reconstruct the schema with documented loss, or
the migration's loss contract should be documented in
internal/store/migrations/README.md. The presence of the Down section
itself is non-negotiable.`, entry.Name())
		}
	}
	if sqlFiles == 0 {
		t.Fatal("no *.sql files found in embedded registry")
	}
}

// TestHasDownSectionRejectsMissingShapes pins the predicate against synthetic
// fixtures. A static checker is only useful if its rejection set is exactly the
// shape the producer (goose convention) does NOT emit; without the negative
// fixtures a buggy predicate could pass every real file by accident.
func TestHasDownSectionRejectsMissingShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "up only — no down marker at all",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n",
			want: false,
		},
		{
			name: "down marker present but empty body",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n",
			want: false,
		},
		{
			name: "down marker followed only by comments",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n-- nothing here\n-- still nothing\n",
			want: false,
		},
		{
			name: "down marker followed only by hash-style comments",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n# nothing here\n# still nothing\n",
			want: false,
		},
		{
			name: "down marker followed only by block comments",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n/* placeholder\n   spanning lines */\n",
			want: false,
		},
		{
			name: "down marker followed by mix of all comment styles — still no SQL",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n-- line\n# hash\n/* block */\n",
			want: false,
		},
		{
			name: "down marker followed only by goose statement-block markers",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n-- +goose StatementBegin\n-- +goose StatementEnd\n",
			want: false,
		},
		{
			name: "down marker with a real DROP",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\nDROP TABLE x;\n",
			want: true,
		},
		{
			name: "down marker with statement-block-wrapped DROP",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n-- +goose StatementBegin\nDROP TABLE x;\n-- +goose StatementEnd\n",
			want: true,
		},
		{
			name: "directive-looking substring inside a block comment does not count",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n/* this block mentions -- +goose Down but is not a directive */\n",
			want: false,
		},
		{
			name: "directive-looking substring as part of a longer comment line does not count",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- TODO: someday add a -- +goose Down section\n",
			want: false,
		},
		{
			name: "block-comment-spliced fake directive must not collapse into a real one",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose D/*x*/own\nDROP TABLE x;\n",
			want: false,
		},
		{
			name: "unterminated block comment after down marker eats the rest",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n/* unterminated\nDROP TABLE x;\n",
			want: false,
		},
		{
			name: "multi-line block comment then a real DROP after the closer",
			body: "-- +goose Up\nCREATE TABLE x (id INT);\n-- +goose Down\n/* multi\n line block */\nDROP TABLE x;\n",
			want: true,
		},
		{
			name: "case-insensitive marker",
			body: "-- +GOOSE UP\nCREATE TABLE x (id INT);\n-- +Goose Down\nDROP TABLE x;\n",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasDownSection([]byte(tc.body)); got != tc.want {
				t.Errorf("hasDownSection() = %v, want %v\nbody:\n%s", got, tc.want, tc.body)
			}
		})
	}
}
