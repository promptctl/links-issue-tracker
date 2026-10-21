package store

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/environ"
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
// (unrelatedDoltDir) so the pair stays production-shaped;
// TestTheStoreTemplatesShareNoCommit proves the histories unrelated.
//
// [LAW:one-type-per-behavior] Both are the same build run twice: independence
// comes from two creations, not from anything either build does differently.
var (
	storeTemplate          = dirtemplate.New("dolt", buildStoreTemplate)
	unrelatedStoreTemplate = dirtemplate.New("dolt", buildStoreTemplate)
)

// migratedDoltDir returns a fresh, private path holding a database
// indistinguishable from one Open just created: a copy of the package
// template, inside t.TempDir() so each test keeps its own engine path
// (one engine per path) and the copy vanishes with the test.
//
// Adoptable piecemeal: any call site of the form
// Open(ctx, filepath.Join(t.TempDir(), "dolt"), id) becomes
// Open(ctx, migratedDoltDir(t), id). Tests whose subject is Open's own
// creation or migration of a store keep building from nothing — that cost is
// their premise. A test that drives migrations itself, down and back up from
// a migrated store (newContainmentFixture), starts from a copy like any other.
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

// buildStoreTemplate opens a new store at dir and closes it, so the template
// holds no engine.
func buildStoreTemplate(dir string) error {
	st, err := Open(context.Background(), testEnv(), dir, "test-workspace-id")
	if err != nil {
		return fmt.Errorf("template Open: %w", err)
	}
	return st.Close()
}

func TestMain(m *testing.M) {
	os.Exit(dirtemplate.Run(m, storeTemplate, unrelatedStoreTemplate))
}

// testEnv is the test process's environment, read when it is called, so a
// test's t.Setenv reaches the git the store it opens runs.
func testEnv() environ.Env { return environ.Of(os.Environ()) }
