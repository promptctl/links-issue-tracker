package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// migrationSeeds holds one seed per migration, named after the migration file
// it seeds: a store valid at that version, filling every table the migration's
// Up or Down reads or rewrites. The seeds sit outside migrations.FS so goose
// never reads them as migrations.
//
//go:embed testdata/migration_seeds/*.sql
var migrationSeeds embed.FS

const migrationSeedsDir = "testdata/migration_seeds"

// schemaStashTable is the stash carrier (design-docs/schema-reversibility.md):
// it sits outside the versioned schema, so neither the shape nor the export
// comparison reads it.
const schemaStashTable = "schema_stash"

// lastVersionBeforeTheGate is the highest migration that existed when this
// gate landed. Only versions up to it may be exempt; every later migration
// passes both round trips.
const lastVersionBeforeTheGate = 11

// releasedDestructiveUp lists the released migrations whose Up destroyed data
// on every store it ran on, so a Down has nothing to return and the round trip
// from the version below is not theirs to pass. Permanent.
var releasedDestructiveUp = map[int64]string{
	4:  "its Up rewrites related-to edges into redirect_target and deletes the edges it moved",
	7:  "its Up moves every ticket into a container and deletes the edges and rows it replaced",
	10: "its Up deletes the producer_binary_version meta row",
}

// TestEveryMigrationRoundTripsLosslessly proves rule 1 of
// design-docs/schema-reversibility.md for every migration from v2 up: a Down
// leaves the schema a fresh Up to the version below produces, and Up after
// Down gives back the same data, byte for byte. Each version runs in both
// directions, each from data seeded at the version it starts from.
//
// [LAW:single-enforcer] One gate, driven by the registry: a new migration is
// covered the moment its file lands, and fails here until it ships a seed.
func TestEveryMigrationRoundTripsLosslessly(t *testing.T) {
	t.Parallel()
	registry := registryMigrations(t)
	seeds := seedsByVersion(t, registry)
	versions := make([]int64, len(registry))
	for i, m := range registry {
		versions[i] = m.Version
	}
	for v, reason := range releasedDestructiveUp {
		if v > lastVersionBeforeTheGate {
			t.Fatalf("v%d is exempt (%s), but only migrations up to v%d may be; make its round trip lossless instead",
				v, reason, lastVersionBeforeTheGate)
		}
	}

	for i := 1; i < len(versions); i++ {
		below, v := versions[i-1], versions[i]
		t.Run(fmt.Sprintf("v%d", v), func(t *testing.T) {
			t.Parallel()
			t.Run("from_its_own_version", func(t *testing.T) {
				t.Parallel()
				if err := roundTripFrom(t, migrations.FS, v, below, seeds[v]); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("from_the_version_below", func(t *testing.T) {
				t.Parallel()
				expectExempt(t, v, releasedDestructiveUp, roundTripBelow(t, migrations.FS, v, below, seeds[below]))
			})
		})
	}
	t.Run("baseline_down_removes_its_tables", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		st, provider := storeAtVersion(t, migrations.FS, migrations.Baseline)
		if _, err := provider.DownTo(ctx, 0); err != nil {
			t.Fatalf("baseline Down: %v", err)
		}
		assertBaselineTablesAbsent(t, ctx, st)
	})
}

// TestV7RoundTripsAnUnfiledEpicAtTheIdItsUpMints runs v7's round trip with
// the seed's unfiled epic at the id the Up mints again (no seed id carries a
// prefix, so it mints lit-unfiled-<sha256("lit-unfiled")[:4]>). The replay
// then meets a minted epic under the stashed id and must leave the stashed
// one, not the minted one.
func TestV7RoundTripsAnUnfiledEpicAtTheIdItsUpMints(t *testing.T) {
	t.Parallel()
	seeds := seedsByVersion(t, registryMigrations(t))
	seed := strings.ReplaceAll(seeds[7], "lit-unfiled-a1b2", "lit-unfiled-e2be")
	if seed == seeds[7] {
		t.Fatal("the v7 seed no longer holds lit-unfiled-a1b2; point this test at its unfiled epic")
	}
	if err := roundTripFrom(t, migrations.FS, 7, 6, seed); err != nil {
		t.Fatal(err)
	}
}

// TestTheRoundTripGateCatchesALossyDown runs the gate's own checks against a
// registry with one Down broken, the two ways a Down can lose: leaving the
// structure its Up added in place, and dropping seeded data without stashing it.
func TestTheRoundTripGateCatchesALossyDown(t *testing.T) {
	t.Parallel()
	const v, below = 11, 10
	seeds := seedsByVersion(t, registryMigrations(t))
	cases := []struct {
		name, down, want string
	}{
		{
			name: "a Down that leaves the added column in place",
			down: "DROP TABLE plan_reviews;",
			want: "shape after Down",
		},
		{
			name: "a Down that drops seeded data without stashing it",
			// v11's real Down drops both; the gate must reject it on data
			// alone once its shape is right, so the mutant is the real Down.
			down: "DROP TABLE plan_reviews;\nALTER TABLE issues DROP COLUMN work_plan;",
			want: "export after Up(Down)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := registryWithDown(t, v, tc.down)
			err := roundTripFrom(t, fsys, v, below, seeds[v])
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("round trip error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// roundTripFrom: seed at v, export, Down to below, compare the shape with a
// fresh Up to below, Up to v, compare the export.
func roundTripFrom(t *testing.T, fsys fs.FS, v, below int64, seed string) error {
	t.Helper()
	ctx := context.Background()
	st, provider := storeAtVersion(t, fsys, v)
	applySeed(t, st.db, seed)
	before := exportTables(t, st.db)
	held := stashEntries(t, st.db)
	if _, err := provider.DownTo(ctx, below); err != nil {
		return fmt.Errorf("Down to v%d: %w", below, err)
	}
	if got, want := schemaShape(t, st.db), freshShape(t, below); got != want {
		return fmt.Errorf("shape after Down differs from a fresh Up to v%d:\n%s", below,
			unifiedSchemaDiff("fresh Up", "after Down", want, got))
	}
	stashed := stashedSince(t, st.db, held)
	if err := upReplaying(ctx, provider, st.db, v); err != nil {
		return fmt.Errorf("Up to v%d after Down: %w", v, err)
	}
	if after := exportTables(t, st.db); after != before {
		return fmt.Errorf("%w: export after Up(Down) differs from the seeded export:\n%s", errDataLost,
			unifiedSchemaDiff("seeded", "after Up(Down)", before, after))
	}
	return stashConsumed(t, st.db, stashed)
}

// upReplaying steps provider up to v the way upByOne does: one goose step,
// then that version's stash replay.
func upReplaying(ctx context.Context, provider *goose.Provider, db *sql.DB, v int64) error {
	for {
		current, err := provider.GetDBVersion(ctx)
		if err != nil {
			return err
		}
		if current >= v {
			return nil
		}
		result, err := provider.UpByOne(ctx)
		if err != nil {
			return err
		}
		if _, err := migrations.ReplayStash(ctx, db, result.Source.Version, false); err != nil {
			return err
		}
	}
}

// freshShapes caches freshShape per version: one Up-only store per version
// for the whole package, not one per round trip.
var freshShapes sync.Map // int64 -> func() string

// freshShape is the schema shape a fresh Up of the embedded registry to v
// produces. A mutant registry differs from it only in a Down, so its fresh
// Ups give the same shape.
func freshShape(t *testing.T, v int64) string {
	t.Helper()
	build, _ := freshShapes.LoadOrStore(v, sync.OnceValue(func() string {
		st, _ := storeAtVersion(t, migrations.FS, v)
		return schemaShape(t, st.db)
	}))
	return build.(func() string)()
}

// roundTripBelow: seed at below, export, Up to v, Down to below, compare the
// export. This is what catches an Up that destroys data without stashing it.
func roundTripBelow(t *testing.T, fsys fs.FS, v, below int64, seed string) error {
	t.Helper()
	ctx := context.Background()
	st, provider := storeAtVersion(t, fsys, below)
	applySeed(t, st.db, seed)
	before := exportTables(t, st.db)
	held := stashEntries(t, st.db)
	if err := upReplaying(ctx, provider, st.db, v); err != nil {
		return fmt.Errorf("Up to v%d: %w", v, err)
	}
	stashed := stashedSince(t, st.db, held)
	if _, err := provider.DownTo(ctx, below); err != nil {
		return fmt.Errorf("Down to v%d after Up: %w", below, err)
	}
	if after := exportTables(t, st.db); after != before {
		return fmt.Errorf("%w: export after Down(Up) differs from the seeded export:\n%s", errDataLost,
			unifiedSchemaDiff("seeded", "after Down(Up)", before, after))
	}
	return stashConsumed(t, st.db, stashed)
}

// stashEntries is the stash carrier's keys, empty when no step has created it.
func stashEntries(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	if !slices.Contains(allTables(t, db), schemaStashTable) {
		return keys
	}
	rows, err := db.QueryContext(context.Background(), "SELECT table_name, row_key, column_name FROM "+schemaStashTable)
	if err != nil {
		t.Fatalf("read stash entries: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, row, column string
		if err := rows.Scan(&table, &row, &column); err != nil {
			t.Fatalf("scan stash entry: %v", err)
		}
		keys[table+"\x00"+row+"\x00"+column] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read stash entries: %v", err)
	}
	return keys
}

// stashedSince is the entries in the carrier now that held did not have: what
// the step just run stashed. Entries a lower migration left are not its to
// hand back.
func stashedSince(t *testing.T, db *sql.DB, held map[string]bool) map[string]bool {
	t.Helper()
	now := stashEntries(t, db)
	for key := range held {
		delete(now, key)
	}
	return now
}

// stashConsumed fails a round trip whose return step left behind an entry the
// outbound step wrote. Whichever direction stashes, the step back restores
// from and deletes every entry it was handed; what it stashes itself is the
// next step's to restore. The export leaves the carrier out, so a stale entry
// is caught here or never.
func stashConsumed(t *testing.T, db *sql.DB, stashed map[string]bool) error {
	t.Helper()
	left := 0
	for key := range stashEntries(t, db) {
		if stashed[key] {
			left++
		}
	}
	if left != 0 {
		return fmt.Errorf("%w: %d stash entries the round trip handed back were left behind", errStashNotDrained, left)
	}
	return nil
}

// errStashNotDrained marks a round trip whose steps left stash entries behind.
var errStashNotDrained = errors.New("stash not drained")

// errDataLost marks the one failure an exemption covers: the round trip ran
// and the export came back different. A wrong shape or a failing step is
// never exempt.
var errDataLost = errors.New("data lost")

// expectExempt fails on a round-trip error unless v is exempt and the error
// is lost data, and fails an exempt version that passes, so an exemption list
// can only shrink.
func expectExempt(t *testing.T, v int64, exempt map[int64]string, err error) {
	t.Helper()
	reason, isExempt := exempt[v]
	switch {
	case err != nil && !(isExempt && errors.Is(err, errDataLost)):
		t.Fatal(err)
	case err == nil && isExempt:
		t.Fatalf("v%d now round-trips; remove it from its exemption list (%s)", v, reason)
	case err != nil:
		t.Logf("exempt (%s): %v", reason, err)
	}
}

// storeAtVersion is a store whose schema came from goose's Ups alone, up to
// v. It starts from a migrated copy, drops every table, and runs the Ups
// again, so no Down ever builds a store the gate compares against.
func storeAtVersion(t *testing.T, fsys fs.FS, v int64) (*Store, *goose.Provider) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, testEnv(), testStderr(), migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tables := allTables(t, st.db)
	stmts := append([]string{"SET FOREIGN_KEY_CHECKS = 0"}, mapQuoted("DROP TABLE ", tables)...)
	for _, stmt := range append(stmts, "SET FOREIGN_KEY_CHECKS = 1") {
		if _, err := st.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("empty the store (%s): %v", stmt, err)
		}
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, st.db, fsys)
	if err != nil {
		t.Fatalf("goose.NewProvider: %v", err)
	}
	if _, err := provider.UpTo(ctx, v); err != nil {
		t.Fatalf("Up to v%d: %v", v, err)
	}
	return st, provider
}

// exportTables is every application table's rows as JSON, in catalog order.
func exportTables(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	var out []RawTable
	for _, name := range comparedTables(t, db) {
		table, err := dumpTable(ctx, db, name)
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		out = append(out, table)
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	return string(b) + "\n"
}

// schemaShape is every compared table's SHOW CREATE TABLE, in name order.
func schemaShape(t *testing.T, db *sql.DB) string {
	t.Helper()
	ddl, err := tablesDDL(context.Background(), db, comparedTables(t, db))
	if err != nil {
		t.Fatalf("schema shape: %v", err)
	}
	return ddl
}

// comparedTables is what the round trip compares: every table but goose's
// bookkeeping and the stash carrier.
func comparedTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	tables, err := applicationTables(context.Background(), db)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	return slices.DeleteFunc(tables, func(name string) bool { return name == schemaStashTable })
}

func allTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	tables, err := listTables(context.Background(), db)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	return tables
}

func mapQuoted(prefix string, names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = prefix + "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return out
}

// applySeed runs a seed's statements, one per ';'-terminated line run. A seed
// with none would compare empty tables to empty tables and prove nothing.
func applySeed(t *testing.T, db *sql.DB, seed string) {
	t.Helper()
	ran := 0
	for _, stmt := range strings.Split(seed, ";\n") {
		if strings.TrimSpace(stripSQLComments(stmt)) == "" {
			continue
		}
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seed statement failed: %v\n%s", err, stmt)
		}
		ran++
	}
	if ran == 0 {
		t.Fatal("seed holds no statements; it must fill every table its migration's Up or Down reads or rewrites")
	}
	for _, name := range comparedTables(t, db) {
		var any int
		err := db.QueryRowContext(context.Background(), "SELECT 1 FROM `"+strings.ReplaceAll(name, "`", "``")+"` LIMIT 1").Scan(&any)
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("seed leaves table %s empty; a round trip over an empty table proves nothing about it", name)
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
	}
}

func stripSQLComments(stmt string) string {
	var kept []string
	for _, line := range strings.Split(stmt, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// registryMigrations is the embedded registry, ascending by version. Gaps
// stay gaps: "the version below" is the next lower one that exists.
func registryMigrations(t *testing.T) []migrations.Migration {
	t.Helper()
	registry, err := migrations.Embedded()
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	return registry.Migrations()
}

// seedsByVersion pairs every migration with its seed, the file of the same
// name, failing by name on a migration with no seed and on a seed with no
// migration.
func seedsByVersion(t *testing.T, registry []migrations.Migration) map[int64]string {
	t.Helper()
	entries, err := fs.ReadDir(migrationSeeds, migrationSeedsDir)
	if err != nil {
		t.Fatalf("read seeds: %v", err)
	}
	seedFiles := map[string]bool{}
	for _, e := range entries {
		seedFiles[e.Name()] = true
	}
	seeds := map[int64]string{}
	for _, m := range registry {
		if !seedFiles[m.Name] {
			t.Fatalf("migration v%d has no seed: add %s/%s, a store valid at v%d filling every table its Up or Down reads or rewrites",
				m.Version, migrationSeedsDir, m.Name, m.Version)
		}
		delete(seedFiles, m.Name)
		b, err := migrationSeeds.ReadFile(path.Join(migrationSeedsDir, m.Name))
		if err != nil {
			t.Fatalf("read seed %s: %v", m.Name, err)
		}
		seeds[m.Version] = string(b)
	}
	for file := range seedFiles {
		t.Fatalf("seed %s/%s names no migration in the registry", migrationSeedsDir, file)
	}
	return seeds
}

// registryWithDown is the embedded registry with migration v's Down section
// replaced, for proving the gate rejects a lossy Down.
func registryWithDown(t *testing.T, v int64, down string) fs.FS {
	t.Helper()
	prefix := fmt.Sprintf("%05d_", v)
	return os.DirFS(copyRegistry(t, func(name, body string) string {
		if !strings.HasPrefix(name, prefix) {
			return body
		}
		up, _, ok := strings.Cut(body, "-- +goose Down")
		if !ok {
			t.Fatalf("%s has no Down section", name)
		}
		return up + "-- +goose Down\n" + gooseStatements(strings.Split(down, "\n"))
	}))
}

// copyRegistry writes the embedded registry into a temp dir, each file's body
// passed through edit, and returns the dir.
func copyRegistry(t *testing.T, edit func(name, body string) string) string {
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
		if err := os.WriteFile(path.Join(dir, e.Name()), []byte(edit(e.Name(), string(b))), 0o644); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
	}
	return dir
}

// assertBaselineTablesAbsent checks that every table the baseline creates is
// gone after its Down, anchored on the parsed baseline file rather than a list.
func assertBaselineTablesAbsent(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	schema, err := baselineSchema()
	if err != nil {
		t.Fatalf("baselineSchema() error = %v", err)
	}
	var stillPresent []string
	for table := range schema {
		exists, err := st.tableExists(ctx, table)
		if err != nil {
			t.Fatalf("tableExists(%q) error = %v", table, err)
		}
		if exists {
			stillPresent = append(stillPresent, table)
		}
	}
	if len(stillPresent) > 0 {
		t.Fatalf("baseline Down ran without error but these tables survived: %s\n"+
			"00001_baseline.sql's `+goose Down` is incomplete — every CREATE TABLE "+
			"in the Up must have a matching DROP TABLE in the Down.",
			strings.Join(stillPresent, ", "))
	}
}
