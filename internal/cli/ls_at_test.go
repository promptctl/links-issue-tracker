package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// foreignStore stands up a real lit store at an explicit storage directory —
// a real Dolt database plus the config.json that carries its workspace_id — so
// runLsAt can be exercised against a store the process is not cd'd into, exactly
// as `lit stores` output would name it. It returns the storage directory and the
// id of one seeded active issue.
func foreignStore(t *testing.T, wsID, prefix string) (storeDir, issueID string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LIT_CONFIG_GLOBAL_PATH", "")
	t.Setenv("LIT_CONFIG_PROJECT_PATH", "")
	ctx := context.Background()

	storeDir = filepath.Join(t.TempDir(), "links")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(storeDir) error = %v", err)
	}
	loc := workspace.LocationFromStorageDir(storeDir)
	if err := os.WriteFile(loc.ConfigPath,
		[]byte(`{"workspace_id":"`+wsID+`","issue_prefix":"`+prefix+`","schema_version":1}`), 0o644); err != nil {
		t.Fatalf("WriteFile(config.json) error = %v", err)
	}

	st, err := store.Open(ctx, loc.DatabasePath, wsID)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	ap := &app.App{
		Workspace: workspace.Info{
			Location:    loc,
			RootDir:     storeDir,
			WorkspaceID: wsID,
			IssuePrefix: testIssuePrefix(t, prefix),
		},
		Store: st,
	}
	issueID = seedOpenIssueRaw(t, ctx, ap, "Ticket in the foreign store")
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}
	return storeDir, issueID
}

// TestRunLsAtListsForeignStoreIssues is the ticket criterion: pointed at a store
// location that is not the cwd's repo, the command lists that store's issues.
func TestRunLsAtListsForeignStoreIssues(t *testing.T) {
	storeDir, issueID := foreignStore(t, "ws-foreign", "proj")

	var out bytes.Buffer
	if err := runLsAt(context.Background(), &out, []string{storeDir}); err != nil {
		t.Fatalf("runLsAt() error = %v", err)
	}
	if !strings.Contains(out.String(), issueID) {
		t.Fatalf("runLsAt() output = %q, want it to list seeded issue %q", out.String(), issueID)
	}
}

// TestRunLsAtLeavesStoreWritable is the read-only guarantee: after reading a
// store by path, the store must still open for write and accept a new issue. A
// leaked lock or a write engine taken by the read would make this reopen fail —
// proving the cross-project read never contended with the store's own writer.
func TestRunLsAtLeavesStoreWritable(t *testing.T) {
	storeDir, _ := foreignStore(t, "ws-foreign", "proj")

	var out bytes.Buffer
	if err := runLsAt(context.Background(), &out, []string{storeDir}); err != nil {
		t.Fatalf("runLsAt() error = %v", err)
	}

	ctx := context.Background()
	loc := workspace.LocationFromStorageDir(storeDir)
	st, err := store.Open(ctx, loc.DatabasePath, "ws-foreign")
	if err != nil {
		t.Fatalf("store.Open() after read error = %v — read left the store un-writable", err)
	}
	defer func() { _ = st.Close() }()
	ap := &app.App{
		Workspace: workspace.Info{
			Location:    loc,
			RootDir:     storeDir,
			WorkspaceID: "ws-foreign",
			IssuePrefix: testIssuePrefix(t, "proj"),
		},
		Store: st,
	}
	if id := seedOpenIssueRaw(t, ctx, ap, "Written after a foreign read"); id == "" {
		t.Fatal("writer produced no issue id after a foreign read")
	}
}

// TestRunLsAtRejectsMissingStore pins the loud-failure contract: a path with no
// lit store is an actionable error naming the path, not an empty success.
func TestRunLsAtRejectsMissingStore(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "links")

	var out bytes.Buffer
	err := runLsAt(context.Background(), &out, []string{missing})
	if err == nil {
		t.Fatalf("runLsAt(missing) returned nil error with output %q; want a surfaced failure", out.String())
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("runLsAt(missing) error = %v, want it to name the path %q", err, missing)
	}
	if out.Len() != 0 {
		t.Fatalf("runLsAt(missing) emitted %q before failing; want no output on the error path", out.String())
	}
}

// TestRunLsAtRequiresExactlyOnePath rejects a call with no path or extra args
// before any store opens, so the usage error cannot depend on a store being
// resolvable first.
func TestRunLsAtRequiresExactlyOnePath(t *testing.T) {
	for _, args := range [][]string{{}, {"a", "b"}} {
		var out bytes.Buffer
		if err := runLsAt(context.Background(), &out, args); err == nil {
			t.Fatalf("runLsAt(%v) = nil error, want a usage error", args)
		}
	}
}
