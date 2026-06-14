package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/doltcli"
)

func TestOpenSyncDoesNotCreateStartupCommitWhenSchemaIsCurrent(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	repoPath := filepath.Join(doltRoot, "links")
	beforeLog, err := doltcli.Run(ctx, repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log before sync open error = %v", err)
	}

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := syncStore.Close(); err != nil {
		t.Fatalf("Close() sync error = %v", err)
	}

	afterLog, err := doltcli.Run(ctx, repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log after sync open error = %v", err)
	}

	if countNonEmptyLines(afterLog) != countNonEmptyLines(beforeLog) {
		t.Fatalf("OpenSync() created extra commit:\nbefore:\n%s\nafter:\n%s", beforeLog, afterLog)
	}
}

func TestOpenSyncCreatesDatabaseWhenMissing(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := syncStore.Close(); err != nil {
		t.Fatalf("Close() sync error = %v", err)
	}

	repoPath := filepath.Join(doltRoot, "links")
	status, err := doltcli.Run(ctx, repoPath, "status")
	if err != nil {
		t.Fatalf("dolt status after sync open error = %v", err)
	}
	if !strings.Contains(status, "On branch master") {
		t.Fatalf("unexpected dolt status output after sync open: %q", status)
	}
}

func TestEnsureDatabaseRenamesEmbeddedMainBranchToMaster(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	if err := os.MkdirAll(doltRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(doltRoot) error = %v", err)
	}

	bootstrap, err := sql.Open(doltDriverName, buildDoltDSN(doltRoot, "test-workspace-id", false))
	if err != nil {
		t.Fatalf("sql.Open() bootstrap error = %v", err)
	}
	if _, err := bootstrap.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", doltDatabaseName)); err != nil {
		t.Fatalf("bootstrap create database error = %v", err)
	}
	if err := bootstrap.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("bootstrap close error = %v", err)
	}

	repoPath := filepath.Join(doltRoot, "links")
	statusBefore, err := doltcli.Run(ctx, repoPath, "status")
	if err != nil {
		t.Fatalf("dolt status before EnsureDatabase error = %v", err)
	}
	if strings.Contains(statusBefore, "On branch master") {
		t.Fatalf("unexpected dolt status before EnsureDatabase: %q", statusBefore)
	}

	if _, err := EnsureDatabase(ctx, doltRoot, "test-workspace-id"); err != nil {
		t.Fatalf("EnsureDatabase() error = %v", err)
	}

	statusAfter, err := doltcli.Run(ctx, repoPath, "status")
	if err != nil {
		t.Fatalf("dolt status after EnsureDatabase error = %v", err)
	}
	if !strings.Contains(statusAfter, "On branch master") {
		t.Fatalf("unexpected dolt status after EnsureDatabase: %q", statusAfter)
	}
}

func TestSyncRemoteLifecycle(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	if err := syncStore.SyncAddRemote(ctx, "origin", "https://example.com/repo.git"); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	remotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		t.Fatalf("SyncListRemotes() after add error = %v", err)
	}
	if len(remotes) != 1 || remotes[0].Name != "origin" {
		t.Fatalf("remotes after add = %#v", remotes)
	}
	if remotes[0].URL == "" {
		t.Fatalf("remotes after add missing URL: %#v", remotes)
	}

	if err := syncStore.SyncRemoveRemote(ctx, "origin"); err != nil {
		t.Fatalf("SyncRemoveRemote() error = %v", err)
	}

	remotes, err = syncStore.SyncListRemotes(ctx)
	if err != nil {
		t.Fatalf("SyncListRemotes() after remove error = %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("remotes after remove = %#v, want empty", remotes)
	}
}

func TestSyncRemoteValidation(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	testCases := []struct {
		name    string
		run     func() error
		wantErr string
	}{
		{
			name:    "add remote requires name",
			run:     func() error { return syncStore.SyncAddRemote(ctx, "   ", "https://example.com/repo.git") },
			wantErr: "remote name is required",
		},
		{
			name:    "add remote requires url",
			run:     func() error { return syncStore.SyncAddRemote(ctx, "origin", "   ") },
			wantErr: "remote url is required",
		},
		{
			name:    "remove remote requires name",
			run:     func() error { return syncStore.SyncRemoveRemote(ctx, "   ") },
			wantErr: "remote name is required",
		},
		{
			name:    "fetch requires remote",
			run:     func() error { return syncStore.SyncFetch(ctx, "   ", false) },
			wantErr: "remote is required",
		},
		{
			name: "pull requires remote",
			run: func() error {
				_, err := syncStore.SyncPull(ctx, "   ", "master")
				return err
			},
			wantErr: "remote is required",
		},
		{
			name: "push requires remote",
			run: func() error {
				_, err := syncStore.SyncPush(ctx, "   ", "master", false, false)
				return err
			},
			wantErr: "remote is required",
		},
	}

	for _, tc := range testCases {
		if err := tc.run(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s error = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestSyncCompactRunsCleanlyAndPreservesData(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "gc target", Topic: "gc-test", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	if err := syncStore.SyncCompact(ctx); err != nil {
		t.Fatalf("SyncCompact() error = %v", err)
	}
	if err := syncStore.SyncCompact(ctx); err != nil {
		t.Fatalf("second SyncCompact() error = %v", err)
	}

	got, err := syncStore.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() after compact error = %v", err)
	}
	if got.Title != "gc target" {
		t.Fatalf("GetIssue() after compact title = %q, want %q", got.Title, "gc target")
	}
}

func TestValidateEmbeddedSyncSupportAcceptsRequiredVersions(t *testing.T) {
	err := validateEmbeddedSyncSupport(map[string]string{
		"github.com/dolthub/dolt/go": minEmbeddedDoltVersion,
		"github.com/dolthub/driver":  minEmbeddedDriverVersion,
	})
	if err != nil {
		t.Fatalf("validateEmbeddedSyncSupport() error = %v", err)
	}
}

func TestValidateEmbeddedSyncSupportRejectsOlderVersions(t *testing.T) {
	err := validateEmbeddedSyncSupport(map[string]string{
		"github.com/dolthub/dolt/go": "v0.40.5-0.20240702155756-bcf4dd5f5cc1",
		"github.com/dolthub/driver":  "v0.2.0",
	})
	if err == nil {
		t.Fatal("validateEmbeddedSyncSupport() error = nil, want version failure")
	}
	if !strings.Contains(err.Error(), "embedded sync requires") {
		t.Fatalf("validateEmbeddedSyncSupport() error = %v, want embedded sync guidance", err)
	}
}

func TestSyncFreshnessStateClassification(t *testing.T) {
	cases := []struct {
		name string
		in   SyncFreshness
		want SyncFreshnessState
	}{
		{"never synced ignores counts", SyncFreshness{Synced: false, Ahead: 0, Behind: 0}, SyncNeverSynced},
		{"up to date", SyncFreshness{Synced: true, Ahead: 0, Behind: 0}, SyncUpToDate},
		{"ahead only", SyncFreshness{Synced: true, Ahead: 2, Behind: 0}, SyncAhead},
		{"behind only", SyncFreshness{Synced: true, Ahead: 0, Behind: 3}, SyncBehind},
		{"diverged", SyncFreshness{Synced: true, Ahead: 2, Behind: 3}, SyncDiverged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.State(); got != tc.want {
				t.Fatalf("State() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSyncFreshnessRequiresRemoteAndBranch(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	if _, err := st.SyncFreshness(ctx, "  ", "master"); err == nil || !strings.Contains(err.Error(), "remote is required") {
		t.Fatalf("SyncFreshness with blank remote error = %v, want remote required", err)
	}
	if _, err := st.SyncFreshness(ctx, "origin", "  "); err == nil || !strings.Contains(err.Error(), "branch is required") {
		t.Fatalf("SyncFreshness with blank branch error = %v, want branch required", err)
	}
}

// TestSyncFreshnessTracksAheadBehindAgainstRemote drives a real file-backed
// remote through every freshness state so the dolt_log range counting and the
// tracking-ref guard are proven against the embedded engine, not asserted in a
// vacuum.
func TestSyncFreshnessTracksAheadBehindAgainstRemote(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := filepath.Join(base, "dolt")
	remoteURL := "file://" + filepath.Join(base, "remote")

	commit := func(title string) {
		st, err := Open(ctx, doltRoot, "ws")
		if err != nil {
			t.Fatalf("Open(%q) error = %v", title, err)
		}
		if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0}); err != nil {
			t.Fatalf("CreateIssue(%q) error = %v", title, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%q) error = %v", title, err)
		}
	}

	assertFreshness := func(label string, sync *Store, wantState SyncFreshnessState, wantAhead, wantBehind int64) {
		t.Helper()
		got, err := sync.SyncFreshness(ctx, "origin", "master")
		if err != nil {
			t.Fatalf("%s: SyncFreshness() error = %v", label, err)
		}
		if got.State() != wantState || got.Ahead != wantAhead || got.Behind != wantBehind {
			t.Fatalf("%s: freshness = %+v state=%q, want state=%q ahead=%d behind=%d", label, got, got.State(), wantState, wantAhead, wantBehind)
		}
	}

	commit("c1")

	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := sync.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	// Remote configured but never pushed/fetched: tracking ref absent.
	assertFreshness("never synced", sync, SyncNeverSynced, 0, 0)

	if _, err := sync.SyncPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncPush(c1) error = %v", err)
	}
	var c1Hash string
	if err := sync.db.QueryRowContext(ctx, `SELECT hash FROM dolt_branches WHERE name = 'master'`).Scan(&c1Hash); err != nil {
		t.Fatalf("read c1 hash error = %v", err)
	}
	assertFreshness("after first push", sync, SyncUpToDate, 0, 0)
	if err := sync.Close(); err != nil {
		t.Fatalf("Close() after push error = %v", err)
	}

	// Local commit not pushed: ahead by 1.
	commit("c2")
	sync, err = OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() after c2 error = %v", err)
	}
	assertFreshness("after local commit", sync, SyncAhead, 1, 0)

	// Publish c2 so the remote-tracking ref advances to c2, then rewind the
	// local branch to c1: the tracking ref is now ahead of local → behind by 1.
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(c2) error = %v", err)
	}
	assertFreshness("after publishing c2", sync, SyncUpToDate, 0, 0)
	if _, err := sync.db.ExecContext(ctx, `CALL DOLT_RESET('--hard', ?)`, c1Hash); err != nil {
		t.Fatalf("DOLT_RESET to c1 error = %v", err)
	}
	assertFreshness("after rewind to c1", sync, SyncBehind, 0, 1)

	// New local commit on top of the rewound branch: c3 is not on the remote and
	// c2 is not local → diverged.
	if err := sync.Close(); err != nil {
		t.Fatalf("Close() before c3 error = %v", err)
	}
	commit("c3")
	sync, err = OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() after c3 error = %v", err)
	}
	defer sync.Close()
	assertFreshness("after divergent commit", sync, SyncDiverged, 1, 1)
}

func TestGitBackedRemoteURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with .git", "https://github.com/org/repo.git", "git+https://github.com/org/repo.git"},
		{"https without .git", "https://github.com/org/repo", "git+https://github.com/org/repo"},
		{"http without .git", "http://example.com/org/repo", "git+http://example.com/org/repo"},
		{"scp-like with .git", "git@github.com:org/repo.git", "git+ssh://git@github.com/./org/repo.git"},
		{"scp-like without .git", "git@github.com:org/repo", "git+ssh://git@github.com/./org/repo"},
		{"ssh url with .git", "ssh://git@github.com/org/repo.git", "git+ssh://git@github.com/org/repo.git"},
		{"file url", "file:///srv/git/repo.git", "git+file:///srv/git/repo.git"},
		{"local absolute path with .git", "/srv/git/repo.git", "git+file:///srv/git/repo.git"},
		{"local absolute path without .git", "/srv/git/repo", "git+file:///srv/git/repo"},
		{"schemeless host without .git", "github.com/org/repo", "git+https://github.com/org/repo"},
		{"already git+ prefixed", "git+https://github.com/org/repo", "git+https://github.com/org/repo"},
		{"empty", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GitBackedRemoteURL(tc.in); got != tc.want {
				t.Fatalf("GitBackedRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGitBackedRemoteURLIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"https://github.com/org/repo",
		"git@github.com:org/repo.git",
		"/srv/git/repo.git",
	} {
		once := GitBackedRemoteURL(in)
		twice := GitBackedRemoteURL(once)
		if once != twice {
			t.Fatalf("GitBackedRemoteURL not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// TestGitBackedRemoteURLRoundTripsThroughDolt guards the no-churn invariant in the
// sync reconciliation loop: the URL lit computes for a git remote must equal the URL
// Dolt stores when that same value is added, or every reconcile would remove+re-add
// the remote forever. Covers the suffix-less and local-path forms that a pure unit
// test of the string output cannot prove against the real store.
func TestGitBackedRemoteURLRoundTripsThroughDolt(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"https://github.com/org/repo.git",
		"https://github.com/org/repo",
		"git@github.com:org/repo.git",
		"git@github.com:org/repo",
		"/srv/git/repo.git",
		"/srv/git/repo",
	} {
		t.Run(raw, func(t *testing.T) {
			doltRoot := filepath.Join(t.TempDir(), "dolt")
			st, err := Open(ctx, doltRoot, "test-workspace-id")
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if err := st.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
			if err != nil {
				t.Fatalf("OpenSync() error = %v", err)
			}
			defer syncStore.Close()

			desired := GitBackedRemoteURL(raw)
			if err := syncStore.SyncAddRemote(ctx, "origin", desired); err != nil {
				t.Fatalf("SyncAddRemote(%q) error = %v", desired, err)
			}
			remotes, err := syncStore.SyncListRemotes(ctx)
			if err != nil {
				t.Fatalf("SyncListRemotes() error = %v", err)
			}
			if len(remotes) != 1 {
				t.Fatalf("remotes = %#v, want 1", remotes)
			}
			if remotes[0].URL != desired {
				t.Fatalf("stored URL = %q, want %q (would cause reconcile churn)", remotes[0].URL, desired)
			}
		})
	}
}
