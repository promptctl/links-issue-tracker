package migrations

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// stashCarrierDDLStatement is any statement that defines or changes the stash
// carrier, whatever its spelling.
var stashCarrierDDLStatement = regexp.MustCompile(`(?i)\b(create|alter|drop|rename)\s+table\s+(if\s+(not\s+)?exists\s+)?` + "`?" + `schema_stash\b`)

// stashCarrierDrift names the first statement in a migration that defines or
// changes schema_stash other than as StashCarrierDDL, byte for byte. With
// IF NOT EXISTS, whichever copy runs first on a store wins and a differing
// copy is skipped without a word, so a drifted copy is caught here or never.
// [LAW:single-enforcer]
func stashCarrierDrift(body string) error {
	for _, loc := range stashCarrierDDLStatement.FindAllStringIndex(body, -1) {
		if !strings.HasPrefix(body[loc[0]:], StashCarrierDDL) {
			line := body[loc[0]:]
			line, _, _ = strings.Cut(line, "\n")
			return fmt.Errorf("%q is not migrations.StashCarrierDDL (stash_carrier.ddl) byte for byte", line)
		}
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
	}
	for _, tc := range cases {
		if err := stashCarrierDrift(tc.body); (err != nil) != tc.drift {
			t.Errorf("%s: stashCarrierDrift() = %v, want drift %v", tc.name, err, tc.drift)
		}
	}
}
