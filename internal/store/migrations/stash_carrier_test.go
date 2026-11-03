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

// stashCarrierDrift names the first statement in a migration that mentions
// schema_stash and is neither row access nor StashCarrierDDL byte for byte.
// Anything else (an alter, drop, truncate, rename, or index) changes the
// carrier. With IF NOT EXISTS, whichever copy runs first on a store wins and a
// differing copy is skipped without a word, so a drifted copy is caught here
// or never. [LAW:single-enforcer]
func stashCarrierDrift(body string) error {
	var code strings.Builder
	for _, line := range strings.SplitAfter(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code.WriteString(line)
		}
	}
	canonical := strings.TrimSpace(StashCarrierDDL)
	for _, stmt := range strings.SplitAfter(code.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if !stashCarrierName.MatchString(stmt) || stashCarrierDML.MatchString(stmt) || stmt == canonical {
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
		{"row access", "DELETE FROM schema_stash WHERE table_name = 'issues';\n", false},
		{"a comment naming a drop", "-- No Down ever does DROP TABLE schema_stash;\n", false},
	}
	for _, tc := range cases {
		if err := stashCarrierDrift(tc.body); (err != nil) != tc.drift {
			t.Errorf("%s: stashCarrierDrift() = %v, want drift %v", tc.name, err, tc.drift)
		}
	}
}
