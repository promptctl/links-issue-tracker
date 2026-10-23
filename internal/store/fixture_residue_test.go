package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/fsperm"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
)

// digestDir folds every file's path and bytes into one hash, so any byte of
// residue a test leaks into the template shows up as a digest mismatch.
func digestDir(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("rel: %v", err)
		}
		io.WriteString(h, rel)
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			t.Fatalf("read %s: %v", path, err)
		}
		f.Close()
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// The isolation proof the fixture must carry: a consumer that writes through
// its copy leaves no residue in the template and none in the next consumer's
// copy. If migratedDoltDir ever handed two tests one directory, or a write
// reached the template, the pristine-state assertions or the digest
// comparison here would fail — this is the test that breaks first.
func TestFixtureResidueCannotCrossCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	templateDir := storeTemplateDir(t)
	templateBefore := digestDir(t, templateDir)

	// Consumer 1: write every kind of state a store test plausibly leaks —
	// an issue, a relation, a comment — then close.
	first, err := Open(ctx, testEnv(), migratedDoltDir(t), "residue-writer")
	if err != nil {
		t.Fatalf("Open first copy: %v", err)
	}
	epic, err := first.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "residue", Title: "Residue epic", Topic: "residue", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue epic: %v", err)
	}
	// Filing the child under the epic writes the parent-child relation row,
	// so the residue spans issues, relations and comments.
	child, err := first.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "residue", Title: "Residue child", Topic: "residue", IssueType: "task", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue child: %v", err)
	}
	if _, _, err := first.AddComment(ctx, storage.AddCommentInput{IssueID: child.ID, Body: storagetest.CommentBody(t, "residue that must not travel"), CreatedBy: "residue"}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first copy: %v", err)
	}

	// Consumer 2: a later copy must be indistinguishable from a store Open
	// just created — same schema version as a from-scratch store, zero issues.
	second, err := Open(ctx, testEnv(), migratedDoltDir(t), "residue-reader")
	if err != nil {
		t.Fatalf("Open second copy: %v", err)
	}
	defer second.Close()
	issues, err := second.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues on second copy: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("second copy is not pristine: %d issue(s) leaked across copies, first: %+v", len(issues), issues[0])
	}

	// From-scratch equivalence: the copy's schema version must match a store
	// built by replaying the whole chain right now.
	scratch, err := Open(ctx, testEnv(), filepath.Join(t.TempDir(), "dolt"), "residue-scratch")
	if err != nil {
		t.Fatalf("Open from scratch: %v", err)
	}
	defer scratch.Close()
	version := func(st *Store) int64 {
		var v int64
		if err := st.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version_id), 0) FROM "+gooseVersionTable).Scan(&v); err != nil {
			t.Fatalf("read %s: %v", gooseVersionTable, err)
		}
		return v
	}
	if got, want := version(second), version(scratch); got != want {
		t.Fatalf("copy schema version = %d, from-scratch store = %d", got, want)
	}

	// And the template itself is byte-identical to before consumer 1 wrote:
	// no write path reaches the shared original.
	if after := digestDir(t, templateDir); after != templateBefore {
		t.Fatalf("template mutated by a consumer: digest %s -> %s", templateBefore, after)
	}
}

// TestFrozenTemplateRefusesDirectOpen pins the freeze layer the same way the
// residue test pins copy isolation: opening the template path directly —
// instead of a copy — must fail, and must leave the template byte-identical.
// dirtemplate's own tests pin that the freeze happens; this one pins that a
// frozen directory is enough to make Dolt's Open refuse, so a template that
// stops being frozen turns CI red instead of quietly poisoning every parallel
// consumer. [LAW:no-silent-failure]
func TestFrozenTemplateRefusesDirectOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	templateDir := storeTemplateDir(t)
	fsperm.RequireReadOnly(t, aFrozenFile(t, templateDir))
	before := digestDir(t, templateDir)

	st, err := Open(ctx, testEnv(), templateDir, "freeze-probe")
	if err == nil {
		_ = st.Close()
		t.Fatal("Open on the frozen template path succeeded; the freeze no longer makes the direct-open mistake loud")
	}

	if after := digestDir(t, templateDir); after != before {
		t.Fatalf("the refused open still mutated the template: digest %s -> %s", before, after)
	}
}

// The unrelated-history guarantee is load-bearing for sync tests, so it is
// checked, not assumed — and against the WHOLE log, not the heads: a shared
// root commit would be a merge base that silently moves sync tests onto the
// related-history path even with distinct heads. [LAW:no-silent-failure]
func TestTheStoreTemplatesShareNoCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	related := commitLog(t, ctx, migratedDoltDir(t))
	if len(related) == 0 {
		t.Fatal("read no commits from a migratedDoltDir copy; the comparison below would pass on nothing")
	}
	for hash := range commitLog(t, ctx, unrelatedDoltDir(t)) {
		if related[hash] {
			t.Fatalf("the two store templates share commit %s: independent creations no longer yield unrelated histories", hash)
		}
	}
}

// commitLog is every commit hash in the log of the store at dir.
func commitLog(t *testing.T, ctx context.Context, dir string) map[string]bool {
	t.Helper()
	st, err := Open(ctx, testEnv(), dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	defer st.Close()
	rows, err := st.db.QueryContext(ctx, "SELECT commit_hash FROM dolt_log")
	if err != nil {
		t.Fatalf("read the log of %s: %v", dir, err)
	}
	defer rows.Close()
	commits := map[string]bool{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatalf("scan the log of %s: %v", dir, err)
		}
		commits[hash] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the log of %s: %v", dir, err)
	}
	return commits
}

// aFrozenFile is any regular file of the frozen template at root.
func aFrozenFile(t *testing.T, root string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return err
		}
		if d.Type().IsRegular() {
			found = path
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("find a file in the template %s: %q, %v", root, found, err)
	}
	return found
}

// storeTemplateDir is the frozen template migratedDoltDir copies, built if no
// test has built it yet.
func storeTemplateDir(t *testing.T) string {
	t.Helper()
	dir, err := storeTemplate.Dir()
	if err != nil {
		t.Fatalf("build the store template: %v", err)
	}
	return dir
}
