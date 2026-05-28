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
	// Strip /* ... */ block comments first so a directive-looking substring
	// inside a block comment cannot be misread as a real goose directive.
	// Goose itself only recognizes directives on a real source line, so the
	// predicate must do the same.
	stripped := stripBlockComments(string(data))
	lines := strings.Split(stripped, "\n")

	// Find the line index of the `+goose Down` directive — and only count a
	// line as the directive if the trimmed line IS the directive, not if it
	// merely contains the substring (a longer comment that happens to embed
	// "-- +goose Down" must not match).
	downLine := -1
	for i, line := range lines {
		if isGooseDirective(line, "down") {
			downLine = i
			break
		}
	}
	if downLine < 0 {
		return false
	}

	// Scan after the Down directive for an executable line. Stop at a
	// subsequent `+goose Up` directive (defensive — files are Up-then-Down
	// by convention, but the predicate must not get confused if a future
	// file embeds an Up after the Down).
	for _, line := range lines[downLine+1:] {
		if isGooseDirective(line, "up") {
			return false
		}
		if isGooseDirective(line, "down") {
			// Duplicate Down directive — keep scanning; the body before it
			// might be empty but content might come after. Goose itself
			// would error on the duplicate, but that's the runtime gate's
			// problem to surface; the static gate only cares about presence
			// of executable bytes anywhere after the FIRST Down directive.
			continue
		}
		stripped := strings.TrimSpace(stripLineComment(line))
		if stripped == "" {
			continue
		}
		return true
	}
	return false
}

// isGooseDirective reports whether a source line is exactly the `+goose <kind>`
// directive (case-insensitive, surrounding whitespace tolerated). The accept
// shape mirrors what goose actually parses: a `-- +goose <kind>` line, not
// any line that happens to contain the substring.
func isGooseDirective(line, kind string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(strings.ToLower(trimmed), "-- ") {
		return false
	}
	body := strings.TrimSpace(trimmed[len("-- "):])
	return strings.EqualFold(body, "+goose "+kind)
}

// stripBlockComments removes /* ... */ comment spans, including ones that
// span multiple lines. Unterminated block comments are treated as comment to
// EOF — the same shape MySQL's parser uses — so a runaway /* with no */ does
// not silently slip through as "executable bytes."
func stripBlockComments(s string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:i])
		s = s[i+2:]
		j := strings.Index(s, "*/")
		if j < 0 {
			return out.String()
		}
		s = s[j+2:]
	}
}

// stripLineComment trims trailing line comments (`-- ...` or `# ...`). Both
// forms are valid in MySQL / Dolt; goose itself only writes `--`, but the
// gate must reject any comment-only Down regardless of which form a future
// author reaches for. Quoted-string handling is omitted because no migration
// has a reason to embed `--` or `#` inside a string literal in the Down
// section; if one does, it still has non-comment bytes left for the predicate
// to find.
func stripLineComment(line string) string {
	if i := strings.Index(line, "--"); i >= 0 {
		line = line[:i]
	}
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	return line
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
