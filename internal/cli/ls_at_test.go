package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/store"
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

// TestLsAtContentionTraceFilesUnderTargetStore pins where a starved
// `ls --at` files its contention trace: under the --at TARGET store — beside
// the traces of whatever holds it — never resolved from the cwd, which for
// --at is explicitly allowed to be no workspace at all. Pre-fix, a cwd-based
// resolution silently dropped this exact record (no cwd workspace) or misfiled
// it into an unrelated one.
//
// Not parallel: it chdirs.
func TestLsAtContentionTraceFilesUnderTargetStore(t *testing.T) {
	storeDir, _ := foreignStore(t, "ws-foreign", "proj")
	loc := workspace.LocationFromStorageDir(storeDir)

	// The --at cwd contract: no workspace anywhere near the cwd.
	chdir(t, t.TempDir())

	release, err := store.LockWorkspaceExclusive(context.Background(), loc.DatabasePath)
	if err != nil {
		t.Fatalf("LockWorkspaceExclusive: %v", err)
	}
	defer func() {
		if relErr := release(); relErr != nil {
			t.Errorf("release exclusive: %v", relErr)
		}
	}()

	var out bytes.Buffer
	err = Run(context.Background(), &out, &out, []string{"ls", "--at", storeDir})
	if err == nil {
		t.Fatalf("ls --at succeeded against an exclusively held store; expected workspace-busy refusal\noutput=%s", out.String())
	}
	if !errors.Is(err, store.ErrWorkspaceBusy) {
		t.Fatalf("ls --at error %v must wrap store.ErrWorkspaceBusy", err)
	}

	// The record names the command, never its payload: its command field is
	// exactly `lit ls`, the --at path redacted — the filing location already
	// carries the target.
	traced := false
	if entries, readErr := os.ReadDir(syncTraceDir(infoForLocation(loc))); readErr == nil {
		for _, entry := range entries {
			content, fileErr := os.ReadFile(filepath.Join(syncTraceDir(infoForLocation(loc)), entry.Name()))
			if fileErr != nil {
				continue
			}
			var rec struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(content, &rec) != nil || !strings.HasPrefix(rec.Command, "lit ls") {
				continue
			}
			if rec.Command != "lit ls" {
				t.Fatalf("contention trace command = %q; the record carries only the command path `lit ls`, never the invocation's payload", rec.Command)
			}
			traced = true
			break
		}
	}
	if !traced {
		t.Fatalf("no sync trace under the --at target records the starved `lit ls`; the contention is unattributable")
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

// TestLsAtRejectsEmptyOrFlagShapedDir pins that an --at carrying no usable store
// directory is a typed UsageError naming the flag, rejected before any store
// opens — a flag-shaped token is never handed to the store layer as a path (so
// `--at --help` doesn't try to open a store named "--help").
//
// The rejection has two authors, and the table says which is which. A bare `--at`
// is a grammar error pflag itself refuses; every other shape is grammatically
// fine — pflag takes any string as a value — so runList rejects it on the parsed
// value. Both arrive as UsageError, which is the contract callers dispatch on;
// the per-case message only records where the refusal came from.
func TestLsAtRejectsEmptyOrFlagShapedDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args    []string
		wantMsg string
	}{
		{[]string{"--at"}, "flag needs an argument: --at"}, // pflag: no value at all
		{[]string{"--at="}, "--at <store-dir>"},
		{[]string{"--at", ""}, "--at <store-dir>"},
		{[]string{"--at", "--help"}, "--at <store-dir>"},
		{[]string{"--at", "--status"}, "--at <store-dir>"},
		{[]string{"--at=--nope"}, "--at <store-dir>"},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		err := runList(context.Background(), &out, tc.args)
		if err == nil {
			t.Fatalf("ls %v = nil error, want a usage error", tc.args)
		}
		var usage UsageError
		if !errors.As(err, &usage) {
			t.Fatalf("ls %v error = %#v, want a UsageError", tc.args, err)
		}
		if !strings.Contains(err.Error(), tc.wantMsg) {
			t.Fatalf("ls %v error = %v, want it to name %q", tc.args, err, tc.wantMsg)
		}
		if out.Len() != 0 {
			t.Fatalf("ls %v emitted %q before failing; want no output on the error path", tc.args, out.String())
		}
	}
}

// TestLsAtAfterTerminatorIsNotARoute pins that `--` ends flag parsing, so a later
// `--at` is a positional literal and ls stays on the cwd workspace. The rule is
// pflag's, not ls's — this test exists because runList reads --at from the parse
// rather than rescanning argv, and a rescan is exactly what would get this wrong.
//
// Not parallel: it chdirs.
func TestLsAtAfterTerminatorIsNotARoute(t *testing.T) {
	storeDir, issueID := foreignStore(t, "ws-foreign", "proj")
	chdir(t, t.TempDir()) // no workspace anywhere near the cwd

	var out bytes.Buffer
	err := runList(context.Background(), &out, []string{"--", "--at", storeDir})

	// Routing would have listed the foreign store's issue; staying on the cwd
	// means the workspace acquisition refuses instead.
	if strings.Contains(out.String(), issueID) {
		t.Fatalf("ls -- --at %s listed the foreign issue %q; `--` must end flag parsing", storeDir, issueID)
	}
	var outside OutsideWorkspaceError
	if !errors.As(err, &outside) {
		t.Fatalf("ls -- --at %s error = %#v, want OutsideWorkspaceError from the cwd path", storeDir, err)
	}
}
