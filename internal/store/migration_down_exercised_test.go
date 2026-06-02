package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// TestEveryMigrationDownIsExercised proves that every migration in the embedded
// registry has a Down section that not only exists (TestEveryMigrationHasDownSection's
// job, in the migrations package) but actually runs against a real workspace. An
// unexercised Down is worse than no Down — the registry claims invertibility the
// runtime cannot deliver.
//
// Strategy: enumerate the actual versions in migrations.FS (descending), open
// ONE fresh workspace at registry-max, then step down one version at a time
// inside it — exercising each Down exactly once with linear total cost rather
// than the O(n²) cost of opening a fresh workspace per version. The FS-driven
// enumeration also makes the test robust to a non-contiguous registry (gaps
// between version numbers): only the versions that actually exist on disk
// are stepped through; a missing intermediate number is never asserted-on.
//
// [LAW:single-enforcer] One runtime gate proves every Down section is
// executable. There is no per-migration test to forget when a new file lands.
//
// [LAW:dataflow-not-control-flow] The same per-step sequence (DownTo →
// verify) runs for every migration. Variability lives in the version list
// (data, derived from the FS), not in which stages execute.
//
// [LAW:one-source-of-truth] The version list comes from the same FS goose
// reads. There is no parallel "expected versions" constant to drift.
func TestEveryMigrationDownIsExercised(t *testing.T) {
	versions, err := registryVersionsDescending()
	if err != nil {
		t.Fatalf("enumerate migration versions: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("no migrations in registry")
	}

	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	provider, err := newGooseProvider(st.db)
	if err != nil {
		t.Fatalf("newGooseProvider() error = %v", err)
	}

	for i, v := range versions {
		// target is the version we want to land at after running v's Down:
		// the next-lower version that exists in the registry, or 0 if v is
		// the lowest. Using the next existing version (not v-1) makes the
		// gate honest about registries with gaps.
		var target int64
		if i+1 < len(versions) {
			target = versions[i+1]
		}
		t.Run(versionTestName(v), func(t *testing.T) {
			results, err := provider.DownTo(ctx, target)
			if err != nil {
				t.Fatalf("DownTo(%d) for migration v%d failed — its `+goose Down` "+
					"section is present (TestEveryMigrationHasDownSection passed) but does not "+
					"actually run against a real workspace: %v", target, v, err)
			}
			if len(results) == 0 {
				t.Fatalf("DownTo(%d) for migration v%d returned no results — "+
					"the Down section was not exercised", target, v)
			}
			// Baseline case: after DownTo(0) the baseline tables MUST be gone.
			// This is the only place we can verify shape without hard-coding
			// per-migration expectations; for non-baseline migrations the
			// "did Down work" check is "DownTo did not error" plus the next
			// iteration's step succeeding against the now-lower-version state.
			if v == migrations.Baseline {
				assertBaselineTablesAbsent(t, ctx, st)
			}
		})
	}
}

// registryVersionsDescending enumerates the versions of every *.sql in the
// embedded registry, sorted highest-first. This is the FS-truth list the
// runtime gate iterates — not a synthetic range — so a non-contiguous
// registry is exercised correctly.
func registryVersionsDescending() ([]int64, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var versions []int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, ok := migrations.ParseVersion(e.Name())
		if !ok {
			// [LAW:one-source-of-truth] Match migrations.MaxVersion's
			// strictness: a non-parseable *.sql in the registry is a
			// registry-shape error, not a skip. Allowing it through here
			// would let a misnamed file evade the runtime gate silently.
			return nil, fmt.Errorf("migration file %q does not begin with a numeric version", e.Name())
		}
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })
	return versions, nil
}

// assertBaselineTablesAbsent verifies that the baseline tables (parsed from
// the embedded baseline file, the same oracle adoption uses) are no longer
// present after the baseline Down has run. Anchoring against the parsed
// baseline rather than a hand-maintained list keeps this aligned with
// 00001_baseline.sql automatically.
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

func versionTestName(v int64) string {
	entries, err := migrations.FS.ReadDir(".")
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			if pv, ok := migrations.ParseVersion(e.Name()); ok && pv == v {
				return e.Name()
			}
		}
	}
	return fmt.Sprintf("v%d", v)
}
