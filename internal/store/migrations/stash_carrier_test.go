package migrations

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// stashCarrierName is any mention of the carrier, quoted or schema-qualified.
var stashCarrierName = regexp.MustCompile(`(?i)\bschema_stash\b`)

// stashCarrierDML is the leading keyword of a statement that only reads or
// writes the carrier's rows.
var stashCarrierDML = regexp.MustCompile(`(?i)^(replace|insert|update|delete|select)\b`)

// stashCarrierScopedDelete is a DELETE that names the table and column whose
// entries it clears. Any other DELETE also clears entries a lower migration
// stashed and has not restored yet.
var stashCarrierScopedDelete = regexp.MustCompile(`(?is)^delete\b.*\bwhere\b.*\btable_name\s*=.*\bcolumn_name\s*=`)

// stashCarrierDrift names the first statement in a migration that mentions
// schema_stash and is neither row access nor StashCarrierDDL byte for byte.
// Anything else (an alter, drop, truncate, rename, or index) changes the
// carrier, and a DELETE must name its table and column. With IF NOT EXISTS, whichever copy runs first on a store wins and a
// differing copy is skipped without a word, so a drifted copy is caught here
// or never. [LAW:single-enforcer]
func stashCarrierDrift(body string) error {
	var code strings.Builder
	inBlock := false
	for _, line := range strings.Split(body, "\n") {
		var content string
		content, inBlock = lineContent(line, inBlock)
		code.WriteString(content + "\n")
	}
	canonical := strings.TrimSpace(StashCarrierDDL)
	for _, stmt := range strings.SplitAfter(code.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if !stashCarrierName.MatchString(stmt) || stmt == canonical {
			continue
		}
		if stashCarrierDML.MatchString(stmt) && (!strings.EqualFold(stmt[:6], "delete") || stashCarrierScopedDelete.MatchString(stmt)) {
			continue
		}
		first, _, _ := strings.Cut(stmt, "\n")
		return fmt.Errorf("%q is not migrations.StashCarrierDDL (stash_carrier.ddl) byte for byte", first)
	}
	return nil
}

func TestEveryStashCarrierCopyIsTheCanonicalDDL(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	for _, e := range entries {
		b, err := fs.ReadFile(FS, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := stashCarrierDrift(string(b)); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
	}
}

func TestStashCarrierDriftNamesEveryOtherDefinition(t *testing.T) {
	drifted := strings.Replace(StashCarrierDDL, "VARCHAR(767)", "VARCHAR(766)", 1)
	cases := []struct {
		name, body string
		drift      bool
	}{
		{"the canonical DDL", "-- +goose StatementBegin\n" + StashCarrierDDL + "-- +goose StatementEnd\n", false},
		{"no carrier at all", "ALTER TABLE issues DROP COLUMN lane;\n", false},
		{"a one-character drift", drifted, true},
		{"a create without IF NOT EXISTS", "CREATE TABLE schema_stash (x INT);\n", true},
		{"an alter", StashCarrierDDL + "ALTER TABLE schema_stash ADD COLUMN y INT;\n", true},
		{"a drop", "drop table if exists `schema_stash`;\n", true},
		{"a drop in a list", "DROP TABLE plan_reviews, schema_stash;\n", true},
		{"a truncate", "TRUNCATE TABLE schema_stash;\n", true},
		{"a rename onto it", "RENAME TABLE tmp TO schema_stash;\n", true},
		{"an index", "CREATE INDEX i ON schema_stash (value);\n", true},
		{"a qualified drop", "DROP TABLE `db`.`schema_stash`;\n", true},
		{"row access", "DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'lane';\n", false},
		{"a comment naming a drop", "-- No Down ever does DROP TABLE schema_stash;\n", false},
		{"an inline comment naming it", "ALTER TABLE issues DROP COLUMN c; -- restored from schema_stash later\n", false},
		{"a block comment naming a drop", "/* never DROP TABLE schema_stash; */\n", false},
		{"an unscoped delete", "DELETE FROM schema_stash;\n", true},
		{"a delete scoped by table alone", "DELETE FROM schema_stash WHERE table_name = 'issues';\n", true},
	}
	for _, tc := range cases {
		if err := stashCarrierDrift(tc.body); (err != nil) != tc.drift {
			t.Errorf("%s: stashCarrierDrift() = %v, want drift %v", tc.name, err, tc.drift)
		}
	}
}

// TestEveryReleasedDownStashesWhatItsUpReplays holds each version's Down to
// its declared stash: the Down carries every Restores entry's StashSQL as
// generated.
func TestEveryReleasedDownStashesWhatItsUpReplays(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	seen := map[int64]bool{}
	for _, e := range entries {
		v, ok := ParseVersion(e.Name())
		if !ok || len(Restores[v]) == 0 {
			continue
		}
		seen[v] = true
		b, err := fs.ReadFile(FS, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		_, down, _ := strings.Cut(string(b), "-- +goose Down")
		for _, st := range Restores[v] {
			if want := GooseStatements(st.StashSQL()); !strings.Contains(down, want) {
				t.Errorf("%s: its Down does not stash %s as migrations generates it:\n%s", e.Name(), st.Name(), want)
			}
		}
	}
	for v := range Restores {
		if !seen[v] {
			t.Errorf("Restores declares v%d, which is not in the registry", v)
		}
	}
}
