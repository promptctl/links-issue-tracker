package store

import (
	"strings"
	"testing"
)

// TestParseAddColumnTargetsRecognizesPlainForm pins the happy path: the
// plain `ADD COLUMN <name> <type>` shape every migration in this registry
// uses parses to exactly the targets present, with no error.
func TestParseAddColumnTargetsRecognizesPlainForm(t *testing.T) {
	t.Parallel()
	up := "ALTER TABLE issues ADD COLUMN lane text NOT NULL DEFAULT '';"
	adds, err := parseAddColumnTargets("00002_add_lane.sql", up)
	if err != nil {
		t.Fatalf("parseAddColumnTargets() error = %v", err)
	}
	if len(adds) != 1 || adds[0].table != "issues" || adds[0].column != "lane" {
		t.Fatalf("adds = %+v, want [{issues lane}]", adds)
	}
	if adds[0].stmt != up {
		t.Errorf("stmt = %q, want the full statement %q for repair to re-execute verbatim", adds[0].stmt, up)
	}
}

// TestParseAddColumnTargetsCapturesEachStatementSeparately pins the repair
// path's prerequisite: when a migration's Up section has more than one
// statement (a later ADD COLUMN after some earlier SQL), each target's stmt
// is its OWN standalone statement — not a slice starting from byte 0 of the
// whole Up section — so repairVersionContentDrift can execute it alone
// without re-running unrelated preceding SQL.
func TestParseAddColumnTargetsCapturesEachStatementSeparately(t *testing.T) {
	t.Parallel()
	up := "ALTER TABLE issues ADD COLUMN resolution VARCHAR(32) NULL;\n" +
		"ALTER TABLE issues ADD CONSTRAINT issues_resolution_check CHECK (resolution IS NULL OR resolution IN ('duplicate','superseded','obsolete','wontfix'));"
	adds, err := parseAddColumnTargets("00003_add_resolution.sql", up)
	if err != nil {
		t.Fatalf("parseAddColumnTargets() error = %v", err)
	}
	if len(adds) != 1 {
		t.Fatalf("adds = %+v, want exactly one ADD COLUMN target", adds)
	}
	want := "ALTER TABLE issues ADD COLUMN resolution VARCHAR(32) NULL;"
	if adds[0].stmt != want {
		t.Errorf("stmt = %q, want %q (the ADD COLUMN statement alone, not including the following ADD CONSTRAINT)", adds[0].stmt, want)
	}
}

// TestParseAddColumnTargetsToleratesSemicolonInStringLiteral pins
// terminatedStatement's quote-awareness: a DEFAULT value containing a
// semicolon must not truncate the captured repair statement early — the
// same quote-tracking discipline parenBlock/splitTopLevel already use
// elsewhere in the toolkit.
func TestParseAddColumnTargetsToleratesSemicolonInStringLiteral(t *testing.T) {
	t.Parallel()
	up := "ALTER TABLE issues ADD COLUMN note TEXT NOT NULL DEFAULT 'a;b';\n" +
		"ALTER TABLE issues ADD COLUMN lane text NOT NULL DEFAULT '';"
	adds, err := parseAddColumnTargets("00099_semicolon.sql", up)
	if err != nil {
		t.Fatalf("parseAddColumnTargets() error = %v", err)
	}
	if len(adds) != 2 {
		t.Fatalf("adds = %+v, want exactly two ADD COLUMN targets", adds)
	}
	want := "ALTER TABLE issues ADD COLUMN note TEXT NOT NULL DEFAULT 'a;b';"
	if adds[0].stmt != want {
		t.Errorf("stmt = %q, want %q (the semicolon inside the string literal must not terminate the statement)", adds[0].stmt, want)
	}
	if adds[0].column != "note" {
		t.Errorf("column = %q, want %q", adds[0].column, "note")
	}
	if adds[1].column != "lane" {
		t.Errorf("second target column = %q, want %q (the following statement must still parse independently)", adds[1].column, "lane")
	}
}

// TestParseAddColumnTargetsRejectsUnrecognizedForm pins the loud-failure
// side raised in PR review: an "ADD COLUMN" occurrence in a shape
// alterAddColumnRe cannot parse (here, IF NOT EXISTS) must fail loudly by
// name, not silently register zero targets for the migration.
func TestParseAddColumnTargetsRejectsUnrecognizedForm(t *testing.T) {
	t.Parallel()
	up := "ALTER TABLE issues ADD COLUMN IF NOT EXISTS lane text NOT NULL DEFAULT '';"
	_, err := parseAddColumnTargets("00099_unsupported.sql", up)
	if err == nil {
		t.Fatal("parseAddColumnTargets() error = nil; want a loud failure naming the unrecognized ADD COLUMN form")
	}
	if !strings.Contains(err.Error(), "00099_unsupported.sql") {
		t.Errorf("error = %q; want it to name the migration file", err.Error())
	}
}

// TestParseAddColumnTargetsRejectsMultiColumnForm covers the second shape
// named in review: a parenthesized multi-column ADD COLUMN list.
func TestParseAddColumnTargetsRejectsMultiColumnForm(t *testing.T) {
	t.Parallel()
	up := "ALTER TABLE issues ADD COLUMN (lane text NOT NULL DEFAULT '', resolution VARCHAR(32) NULL);"
	_, err := parseAddColumnTargets("00099_unsupported.sql", up)
	if err == nil {
		t.Fatal("parseAddColumnTargets() error = nil; want a loud failure naming the unrecognized multi-column ADD COLUMN form")
	}
}
