package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// carrierColumns are the columns the test-only migration adds to issues and
// its Down drops through the carrier: one of each string type the carrier
// admits (migrations/README.md, "The stash carrier").
var carrierColumns = []struct{ name, ddl string }{
	{"stash_note", "TEXT NULL"},
	{"stash_label", "VARCHAR(191) NULL"},
}

// carrierMigration is a test-only migration written to the README's template:
// its Up adds the carrier columns and restores them from the stash, and its
// Down stashes them before dropping them. Each statement is one goose
// statement.
func carrierMigration() (up, down []string) {
	key := "CAST(JSON_OBJECT('id', issues.id) AS CHAR)"
	up = []string{migrations.StashCarrierDDL}
	down = []string{migrations.StashCarrierDDL}
	for _, c := range carrierColumns {
		up = append(up, fmt.Sprintf("ALTER TABLE issues ADD COLUMN %s %s;", c.name, c.ddl))
		down = append(down, fmt.Sprintf("REPLACE INTO schema_stash SELECT 'issues', %s, '%s', JSON_ARRAY(SUBSTRING(%s, 1)) FROM issues;", key, c.name, c.name))
	}
	for _, c := range carrierColumns {
		up = append(up,
			fmt.Sprintf("UPDATE issues JOIN schema_stash s ON s.table_name = 'issues' AND s.column_name = '%s' AND s.row_key = %s SET issues.%s = IF(JSON_TYPE(JSON_EXTRACT(s.value, '$[0]')) = 'NULL', NULL, JSON_UNQUOTE(JSON_EXTRACT(s.value, '$[0]')));", c.name, key, c.name),
			fmt.Sprintf("DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = '%s';", c.name))
		down = append(down, fmt.Sprintf("ALTER TABLE issues DROP COLUMN %s;", c.name))
	}
	return up, down
}

// carrierSeed fills the carrier columns with the values a string stash must
// return exactly: quotes, a backslash, an escaped newline, non-ASCII, ”,
// NULL, and strings that read as JSON.
const carrierSeed = `UPDATE issues SET stash_note = CONCAT('x"y\\z ünï 😀\n', id), stash_label = CONCAT('q''r 中 ', id);
UPDATE issues SET stash_note = NULL ORDER BY id LIMIT 1;
UPDATE issues SET stash_label = '' ORDER BY id LIMIT 1;
UPDATE issues SET stash_note = '', stash_label = NULL ORDER BY id DESC LIMIT 1;
UPDATE issues SET stash_note = 'null', stash_label = '[1]' ORDER BY id DESC LIMIT 1 OFFSET 1;
`

// registryWithCarrier is the embedded registry plus the test-only carrier
// migration, one version above the registry's top.
func registryWithCarrier(t *testing.T, up, down []string) (fsys fs.FS, v int64) {
	t.Helper()
	dir := t.TempDir()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	for _, e := range entries {
		b, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(path.Join(dir, e.Name()), b, 0o644); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
	}
	registry := registryMigrations(t)
	v = registry[len(registry)-1].Version + 1
	body := "-- +goose Up\n" + gooseStatements(up) + "-- +goose Down\n" + gooseStatements(down)
	if err := os.WriteFile(path.Join(dir, fmt.Sprintf("%05d_stash_carrier.sql", v)), []byte(body), 0o644); err != nil {
		t.Fatalf("write carrier migration: %v", err)
	}
	return os.DirFS(dir), v
}

// encodedBare is down with every stash value encoded as JSON_ARRAY(c).
func encodedBare(down []string) []string {
	out := make([]string, len(down))
	for i, s := range down {
		for _, c := range carrierColumns {
			s = strings.ReplaceAll(s, "JSON_ARRAY(SUBSTRING("+c.name+", 1))", "JSON_ARRAY("+c.name+")")
		}
		out[i] = s
	}
	return out
}

func gooseStatements(stmts []string) string {
	var b strings.Builder
	for _, s := range stmts {
		b.WriteString("-- +goose StatementBegin\n" + strings.TrimRight(s, "\n") + "\n-- +goose StatementEnd\n")
	}
	return b.String()
}

// TestTheStashCarrierRoundTripsAColumnItsDownDrops proves the README's
// template lossless in both directions, and proves the gate rejects it with
// the stash left out or left undrained.
func TestTheStashCarrierRoundTripsAColumnItsDownDrops(t *testing.T) {
	t.Parallel()
	seeds := seedsByVersion(t, registryMigrations(t))
	up, down := carrierMigration()
	t.Run("the template", func(t *testing.T) {
		t.Parallel()
		fsys, v := registryWithCarrier(t, up, down)
		if err := roundTripFrom(t, fsys, v, v-1, seeds[v-1]+carrierSeed); err != nil {
			t.Fatalf("round trip from v%d: %v", v, err)
		}
		if err := roundTripBelow(t, fsys, v, v-1, seeds[v-1]); err != nil {
			t.Fatalf("round trip from v%d: %v", v-1, err)
		}
	})
	without := func(stmts []string, prefix string) []string {
		var kept []string
		for _, s := range stmts {
			if !strings.HasPrefix(s, prefix) {
				kept = append(kept, s)
			}
		}
		return kept
	}
	mutants := []struct {
		name     string
		up, down []string
		want     error
	}{
		{"a Down that drops the columns without stashing them", up, without(down, "REPLACE INTO schema_stash"), errDataLost},
		{"an Up that restores without deleting its entries", without(up, "DELETE FROM schema_stash"), down, errStashNotDrained},
		// Dolt's JSON_ARRAY over a TEXT column encodes its storage address,
		// not its text; SUBSTRING(c, 1) is what makes it a string.
		{"a stash that encodes TEXT without SUBSTRING", up, encodedBare(down), errDataLost},
	}
	for _, m := range mutants {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			fsys, v := registryWithCarrier(t, m.up, m.down)
			if err := roundTripFrom(t, fsys, v, v-1, seeds[v-1]+carrierSeed); !errors.Is(err, m.want) {
				t.Fatalf("round trip error = %v, want %v", err, m.want)
			}
		})
	}
}
