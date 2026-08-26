package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
// unrelated history. Single-store tests draw from slot 0; a two-workspace
// sync test takes its second side from slot 1 (unrelatedDoltDir) so the pair
// stays production-shaped.
var storeTemplates [2]struct {
	once    sync.Once
	dir     string
	commits map[string]bool
	err     error
}

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
func migratedDoltDir(t *testing.T) string {
	t.Helper()
	return copyOfStoreTemplate(t, 0)
}

// unrelatedDoltDir is migratedDoltDir's counterpart for the other side of a
// two-workspace test: its copy comes from an independently built template, so
// its Dolt history shares no commit with migratedDoltDir copies — the shape
// two production workspaces actually have. Seeding both sides of a sync test
// from ONE template would silently move it onto the related-history path.
func unrelatedDoltDir(t *testing.T) string {
	t.Helper()
	return copyOfStoreTemplate(t, 1)
}

func copyOfStoreTemplate(t *testing.T, slot int) string {
	t.Helper()
	if err := ensureStoreTemplate(slot); err != nil {
		t.Fatalf("build store template %d: %v", slot, err)
	}
	dst := filepath.Join(t.TempDir(), "dolt")
	// os.CopyFS re-derives file modes (0o666 before umask), so the copies
	// are writable even though the template's own files are locked read-only.
	if err := os.CopyFS(dst, os.DirFS(storeTemplates[slot].dir)); err != nil {
		t.Fatalf("copy store template: %v", err)
	}
	return dst
}

func ensureStoreTemplate(slot int) error {
	tpl := &storeTemplates[slot]
	tpl.once.Do(func() {
		tpl.dir, tpl.commits, tpl.err = buildStoreTemplate()
		// The unrelated-history guarantee is load-bearing for sync tests, so
		// it is checked, not assumed — and against the WHOLE log, not the
		// heads: a shared root commit would be a merge base that silently
		// moves sync tests onto the related-history path even with distinct
		// heads. If Dolt ever produced a common commit for two independent
		// creations, slot 1's build fails loudly here. [LAW:no-silent-failure]
		if tpl.err == nil && slot == 1 {
			if err := ensureStoreTemplate(0); err == nil {
				for hash := range storeTemplates[0].commits {
					if tpl.commits[hash] {
						tpl.err = fmt.Errorf("independently built templates share commit %s — independent creations no longer yield unrelated histories", hash)
						break
					}
				}
			}
		}
	})
	return tpl.err
}

func buildStoreTemplate() (dir string, commits map[string]bool, _ error) {
	base, err := os.MkdirTemp("", "lit-store-template-")
	if err != nil {
		return "", nil, err
	}
	// base lives outside every t.TempDir on purpose (no test's cleanup may
	// reach a shared template), so a failed build must sweep it itself —
	// TestMain only removes templates that finished. Success-gated, same
	// pattern as Open/OpenSync's lock release. [LAW:no-silent-failure]
	success := false
	defer func() {
		if !success {
			// Thaw first: a build that failed mid-freeze left read-only
			// directories RemoveAll cannot unlink from.
			_ = freezeTemplate(base, 0o644, 0o755)
			_ = os.RemoveAll(base)
		}
	}()
	dir = filepath.Join(base, "dolt")
	ctx := context.Background()
	st, err := Open(ctx, dir, "test-workspace-id")
	if err != nil {
		return "", nil, fmt.Errorf("template Open: %w", err)
	}
	commits, logErr := templateCommits(ctx, st)
	if err := st.Close(); err != nil {
		return "", nil, fmt.Errorf("template Close: %w", err)
	}
	if logErr != nil {
		return "", nil, fmt.Errorf("template log: %w", logErr)
	}
	// Lock the template read-only so a test that accidentally opens the
	// template path itself — instead of a copy — fails loudly at its first
	// write rather than leaking state into every later copy. Directories are
	// frozen too, not just files: rename(2) — the write-new-then-rename-over
	// pattern Dolt uses for atomic swaps — is gated by the containing
	// directory's write bit, so file bits alone would let those writes
	// through silently. [LAW:no-silent-failure]
	if err := freezeTemplate(dir, 0o444, 0o555); err != nil {
		return "", nil, fmt.Errorf("freeze template: %w", err)
	}
	success = true
	return dir, commits, nil
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

// freezeTemplate stamps one mode onto every file and another onto every
// directory under root — the same walk freezes a finished template and thaws
// it for removal, so the two modes cannot drift apart. [LAW:one-source-of-truth]
func freezeTemplate(root string, fileMode, dirMode fs.FileMode) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, dirMode)
		}
		return os.Chmod(path, fileMode)
	})
}

// TestMain exists only to remove the templates after the run; they live
// outside any t.TempDir precisely so no single test's cleanup can delete one
// out from under the others. The thaw before RemoveAll is what makes removal
// possible at all — unlinking from a 0o555 directory is exactly what the
// freeze forbids.
func TestMain(m *testing.M) {
	code := m.Run()
	for i := range storeTemplates {
		if dir := storeTemplates[i].dir; dir != "" {
			_ = freezeTemplate(dir, 0o644, 0o755)
			_ = os.RemoveAll(filepath.Dir(dir))
		}
	}
	os.Exit(code)
}
