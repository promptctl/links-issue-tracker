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

	// The template must exist before we can digest it; force the build via a
	// throwaway copy, then record the template's pre-mutation digest.
	_ = migratedDoltDir(t)
	templateBefore := digestDir(t, storeTemplates[0].dir)

	// Consumer 1: write every kind of state a store test plausibly leaks —
	// an issue, a relation, a comment — then close.
	first, err := Open(ctx, migratedDoltDir(t), "residue-writer")
	if err != nil {
		t.Fatalf("Open first copy: %v", err)
	}
	epic, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "residue", Title: "Residue epic", Topic: "residue", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue epic: %v", err)
	}
	child, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "residue", Title: "Residue child", Topic: "residue", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue child: %v", err)
	}
	if _, err := first.AddRelation(ctx, AddRelationInput{SrcID: child.ID, DstID: epic.ID, Type: "parent-child", CreatedBy: "residue"}); err != nil {
		t.Fatalf("AddRelation: %v", err)
	}
	if _, _, err := first.AddComment(ctx, AddCommentInput{IssueID: child.ID, Body: "residue that must not travel", CreatedBy: "residue"}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first copy: %v", err)
	}

	// Consumer 2: a later copy must be indistinguishable from a store Open
	// just created — same schema version as a from-scratch store, zero issues.
	second, err := Open(ctx, migratedDoltDir(t), "residue-reader")
	if err != nil {
		t.Fatalf("Open second copy: %v", err)
	}
	defer second.Close()
	issues, err := second.ListIssues(ctx, ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues on second copy: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("second copy is not pristine: %d issue(s) leaked across copies, first: %+v", len(issues), issues[0])
	}

	// From-scratch equivalence: the copy's schema version must match a store
	// built by replaying the whole chain right now.
	scratch, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "residue-scratch")
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
	if after := digestDir(t, storeTemplates[0].dir); after != templateBefore {
		t.Fatalf("template mutated by a consumer: digest %s -> %s", templateBefore, after)
	}
}
