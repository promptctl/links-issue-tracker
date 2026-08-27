package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// foreignStore stands up a real lit store at an explicit storage directory —
// a real Dolt database plus the config.json that carries its workspace_id — so
// `lit ls --at` can be exercised against a store the process is not cd'd into,
// exactly as `lit stores` output would name it. It returns the storage directory
// and the id of one seeded active issue.
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

	st, err := engine.Open(ctx, engine.ReadWrite, loc.DatabasePath, wsID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
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

// TestLsAtListsForeignStoreIssues is the ticket criterion: pointed at a store
// location that is not the cwd's repo, `ls --at <dir>` lists that store's issues.
func TestLsAtListsForeignStoreIssues(t *testing.T) {
	storeDir, issueID := foreignStore(t, "ws-foreign", "proj")

	var out bytes.Buffer
	if err := runList(context.Background(), &out, []string{"--at", storeDir}); err != nil {
		t.Fatalf("ls --at error = %v", err)
	}
	if !strings.Contains(out.String(), issueID) {
		t.Fatalf("ls --at output = %q, want it to list seeded issue %q", out.String(), issueID)
	}
}

// TestLsAtLeavesStoreWritable is the read-only guarantee: after reading a
// store by path, the store must still open for write and accept a new issue. A
// leaked lock or a write engine taken by the read would make this reopen fail —
// proving the cross-project read never contended with the store's own writer.
func TestLsAtLeavesStoreWritable(t *testing.T) {
	storeDir, _ := foreignStore(t, "ws-foreign", "proj")

	var out bytes.Buffer
	if err := runList(context.Background(), &out, []string{"--at", storeDir}); err != nil {
		t.Fatalf("ls --at error = %v", err)
	}

	ctx := context.Background()
	loc := workspace.LocationFromStorageDir(storeDir)
	st, err := engine.Open(ctx, engine.ReadWrite, loc.DatabasePath, "ws-foreign")
	if err != nil {
		t.Fatalf("engine.Open() after read error = %v — read left the store un-writable", err)
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

// TestLsAtRejectsMissingStore pins the loud-failure contract: a path with no
// lit store is an actionable error naming the path, not an empty success.
func TestLsAtRejectsMissingStore(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nope", "links")

	var out bytes.Buffer
	err := runList(context.Background(), &out, []string{"--at", missing})
	if err == nil {
		t.Fatalf("ls --at (missing) returned nil error with output %q; want a surfaced failure", out.String())
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("ls --at (missing) error = %v, want it to name the path %q", err, missing)
	}
	if out.Len() != 0 {
		t.Fatalf("ls --at (missing) emitted %q before failing; want no output on the error path", out.String())
	}
}

// TestLsAtRejectsEmptyOrFlagShapedDir pins that an --at with no value or a
// flag-shaped value (`--at --help`, `--at --status`) is a usage error naming the
// flag, rejected before any store opens — a flag-shaped token is never handed to
// the store layer as a path (so `--at --help` doesn't try to open a store named
// "--help").
func TestLsAtRejectsEmptyOrFlagShapedDir(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--at"}, {"--at="}, {"--at", ""},
		{"--at", "--help"}, {"--at", "--status"}, {"--at=--nope"},
	} {
		var out bytes.Buffer
		err := runList(context.Background(), &out, args)
		if err == nil {
			t.Fatalf("ls %v = nil error, want a usage error", args)
		}
		if !strings.Contains(err.Error(), "--at <store-dir>") {
			t.Fatalf("ls %v error = %v, want it to name the --at usage", args, err)
		}
	}
}

// TestExtractAtDir pins the routing scan directly: it recognizes both --at forms,
// honors the `--` terminator (a later --at is a positional literal, not a route),
// and reports a present-but-empty --at so the caller can reject it.
func TestExtractAtDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args    []string
		wantDir string
		wantOK  bool
	}{
		{[]string{"--at", "/p"}, "/p", true},
		{[]string{"--at=/p"}, "/p", true},
		{[]string{"--search", "x", "--at", "/p"}, "/p", true},
		{[]string{"--status", "open"}, "", false},
		{[]string{}, "", false},
		{[]string{"--at"}, "", true},                 // present, no value
		{[]string{"--at", "--help"}, "--help", true}, // flag-shaped value (caller rejects)
		{[]string{"--", "--at", "/p"}, "", false},    // terminator: not a route
	}
	for _, tc := range cases {
		gotDir, gotOK := extractAtDir(tc.args)
		if gotDir != tc.wantDir || gotOK != tc.wantOK {
			t.Errorf("extractAtDir(%v) = (%q, %v), want (%q, %v)", tc.args, gotDir, gotOK, tc.wantDir, tc.wantOK)
		}
	}
}
