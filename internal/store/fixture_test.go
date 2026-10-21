package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
)

// The package's migrated-store templates: each built once per test binary by
// the real Open (so a template cannot drift from what Open produces — it IS
// an Open artifact [LAW:one-source-of-truth]), then handed to tests as
// per-test directory copies. Copying ~0.1MB costs ~6ms where replaying the
// bootstrap+migration chain costs ~390ms, and every creation skipped is also
// one fewer pass through the process-wide engine-construction mutex
// (vendored-driver patch 5), so the savings compound under t.Parallel.
//
// There are two templates, not one, because copies of a single template share
// its Dolt root commit: two stores seeded from the same template have RELATED
// history, while production workspaces are created independently and have
// unrelated history. Single-store tests draw from storeTemplate; a
// two-workspace sync test takes its second side from unrelatedStoreTemplate
// (unrelatedDoltDir) so the pair stays production-shaped.
var (
	// storeTemplateCommits is every commit in storeTemplate's log, recorded by
	// its build: the evidence unrelatedStoreTemplate's build compares against.
	storeTemplateCommits map[string]bool

	storeTemplate = dirtemplate.New("dolt", func(dir string) (err error) {
		storeTemplateCommits, err = buildStoreTemplate(dir)
		return err
	})

	unrelatedStoreTemplate = dirtemplate.New("dolt", func(dir string) error {
		commits, err := buildStoreTemplate(dir)
		if err != nil {
			return err
		}
		if _, err := storeTemplate.Dir(); err != nil {
			return fmt.Errorf("build the template this one must be unrelated to: %w", err)
		}
		// The unrelated-history guarantee is load-bearing for sync tests, so
		// it is checked, not assumed — and against the WHOLE log, not the
		// heads: a shared root commit would be a merge base that silently
		// moves sync tests onto the related-history path even with distinct
		// heads. If Dolt ever produced a common commit for two independent
		// creations, this build fails loudly here. [LAW:no-silent-failure]
		for hash := range commits {
			if storeTemplateCommits[hash] {
				return fmt.Errorf("independently built templates share commit %s — independent creations no longer yield unrelated histories", hash)
			}
		}
		return nil
	})
)

// migratedDoltDir returns a fresh, private path holding a database
// indistinguishable from one Open just created: a copy of the package
// template, inside t.TempDir() so each test keeps its own engine path
// (one engine per path) and the copy vanishes with the test.
//
// Adoptable piecemeal: any call site of the form
// Open(ctx, filepath.Join(t.TempDir(), "dolt"), id) becomes
// Open(ctx, migratedDoltDir(t), id). Tests whose subject is creation or
// migration itself keep building from nothing — that cost is their premise.
// The SECOND store of a two-workspace test comes from unrelatedDoltDir,
// never from a second migratedDoltDir call.
func migratedDoltDir(t testing.TB) string {
	t.Helper()
	return storeTemplate.Copy(t)
}

// unrelatedDoltDir is migratedDoltDir's counterpart for the other side of a
// two-workspace test: its copy comes from an independently built template, so
// its Dolt history shares no commit with migratedDoltDir copies — the shape
// two production workspaces actually have. Seeding both sides of a sync test
// from ONE template would silently move it onto the related-history path.
func unrelatedDoltDir(t *testing.T) string {
	t.Helper()
	return unrelatedStoreTemplate.Copy(t)
}

// buildStoreTemplate opens a new store at dir and returns every commit in its
// log, closing it so the template holds no engine.
func buildStoreTemplate(dir string) (map[string]bool, error) {
	ctx := context.Background()
	st, err := Open(ctx, dir, "test-workspace-id")
	if err != nil {
		return nil, fmt.Errorf("template Open: %w", err)
	}
	commits, logErr := templateCommits(ctx, st)
	if err := st.Close(); err != nil {
		return nil, fmt.Errorf("template Close: %w", err)
	}
	if logErr != nil {
		return nil, fmt.Errorf("template log: %w", logErr)
	}
	return commits, nil
}

// templateCommits reads every commit hash in the store's log, the evidence
// the unrelated-templates check compares.
func templateCommits(ctx context.Context, st *Store) (map[string]bool, error) {
	rows, err := st.db.QueryContext(ctx, "SELECT commit_hash FROM dolt_log")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	commits := map[string]bool{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		commits[hash] = true
	}
	return commits, rows.Err()
}

// TestMain exists only to remove the templates after the run. A template left
// behind is a frozen directory nothing else will ever delete, so a removal
// that fails fails the run. [LAW:no-silent-failure]
func TestMain(m *testing.M) {
	code := m.Run()
	if err := errors.Join(storeTemplate.Remove(), unrelatedStoreTemplate.Remove()); err != nil {
		fmt.Fprintf(os.Stderr, "remove the store templates: %v\n", err)
		code = max(code, 1)
	}
	os.Exit(code)
}
