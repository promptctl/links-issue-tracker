package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/backup"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// resolveRestorePath is the single authority `backup restore` resolves its
// source through; these assert the documented cap of two sources (explicit
// path, latest backup) and that the degenerate combinations fail loudly rather
// than picking a silent precedence.
func TestResolveRestorePathCap(t *testing.T) {
	t.Parallel()
	t.Run("no source is a usage error", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		_, err := resolveRestorePath(ap, "  ", false)
		if _, ok := err.(UsageError); !ok {
			t.Fatalf("resolveRestorePath(no source) error = %v, want UsageError", err)
		}
	})

	t.Run("both sources is a mutual-exclusion error, not silent precedence", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		_, err := resolveRestorePath(ap, "/some/export.json", true)
		ue, ok := err.(UsageError)
		if !ok || !strings.Contains(ue.Message, "mutually exclusive") {
			t.Fatalf("resolveRestorePath(both) error = %v, want mutually-exclusive UsageError", err)
		}
	})

	t.Run("explicit path passes through trimmed", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		got, err := resolveRestorePath(ap, "  /some/export.json  ", false)
		if err != nil || got != "/some/export.json" {
			t.Fatalf("resolveRestorePath(path) = %q, %v; want trimmed path", got, err)
		}
	})

	t.Run("latest with no backups fails loudly", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		_, err := resolveRestorePath(ap, "", true)
		if err == nil || !strings.Contains(err.Error(), "no backups available") {
			t.Fatalf("resolveRestorePath(latest, empty) error = %v, want no backups available", err)
		}
	})

	t.Run("latest resolves to the newest snapshot path", func(t *testing.T) {
		dir := t.TempDir()
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: dir}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		export := model.Export{Version: 1, WorkspaceID: "ws", ExportedAt: time.Now().UTC()}
		snapshot, err := backup.Create(dir, export)
		if err != nil {
			t.Fatalf("backup.Create() error = %v", err)
		}
		got, err := resolveRestorePath(ap, "", true)
		if err != nil || got != snapshot.Path {
			t.Fatalf("resolveRestorePath(latest) = %q, %v; want %q", got, err, snapshot.Path)
		}
	})
}

// guardRestore lets a restore through when it is forced, when no restore has
// recorded a digest, or when the workspace holds what the last restore left,
// and refuses only a workspace that moved since. A forced restore never reads
// the record, so an unreadable one cannot block it.
func TestGuardRestoreVerdicts(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	issue, err := model.HydrateStatus(model.Issue{ID: "ws-1", Title: "Home", IssueType: "task", CreatedAt: at, UpdatedAt: at}, model.StatusView{Value: model.StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
	}
	left := model.Export{Version: model.ExportVersion, WorkspaceID: "ws", ExportedAt: at, Issues: []model.Issue{issue}}
	recorded, err := syncfile.ContentHash(left)
	if err != nil {
		t.Fatalf("ContentHash() error = %v", err)
	}
	later := left
	later.ExportedAt = at.Add(time.Hour)
	moved := later
	moved.Issues = nil
	recordedIn := func(t *testing.T) workspace.Info {
		ws := declineWorkspace(t)
		if err := writeMarkerAtomic(restoreDigestPath(ws), []byte(recorded+"\n")); err != nil {
			t.Fatalf("record digest: %v", err)
		}
		return ws
	}
	unreadable := func(t *testing.T) workspace.Info {
		ws := declineWorkspace(t)
		if err := os.MkdirAll(filepath.Join(restoreDigestPath(ws), "blocked"), 0o755); err != nil {
			t.Fatalf("block digest: %v", err)
		}
		return ws
	}
	unrecorded := declineWorkspace
	for _, tc := range []struct {
		name  string
		ws    func(*testing.T) workspace.Info
		local model.Export
		force bool
		want  restoreGuard
	}{
		{"forced over a moved workspace", recordedIn, moved, true, guardForced},
		{"forced over an unreadable record", unreadable, moved, true, guardForced},
		{"no recorded digest", unrecorded, moved, false, guardUnrecorded},
		{"same content exported later", recordedIn, later, false, guardUnchanged},
		{"workspace moved since", recordedIn, moved, false, guardChanged},
	} {
		got, err := guardRestore(tc.ws(t), tc.local, tc.force)
		if err != nil || got != tc.want {
			t.Errorf("%s: guardRestore() = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if got, err := guardRestore(unreadable(t), later, false); err == nil {
		t.Errorf("unforced over an unreadable record: guardRestore() = %q, nil; want the read error", got)
	}
}

// TestBackupRestoreAfterARestoreProceedsUntilTheWorkspaceMoves is the
// acceptance: a second restore over a workspace that holds what the first one
// left goes ahead without --force, and one over a workspace with a ticket made
// since is refused. The workspace holds an epic, whose decoded form cannot be
// re-marshalled, so the guard must compare digests of store exports; and
// children and a comment, so its relations and comments are exported again
// in the order the digest was taken in.
func TestBackupRestoreAfterARestoreProceedsUntilTheWorkspaceMoves(t *testing.T) {
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	for _, title := range []string{"First", "Second"} {
		child := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--title", title, "--topic", "demo", "--parent", epic))
		runCLIInDir(t, repo, "comment", "add", child, "--body", "noted on "+title)
	}
	created := strings.Fields(runCLIInDir(t, repo, "backup", "create"))
	if len(created) != 2 {
		t.Fatalf("backup create printed %v, want a name and a path", created)
	}
	runCLIInDir(t, repo, "backup", "restore", "--path", created[1])
	if check := restoreTraceRecord(t, repo).Metadata["restore_guard"]; check != string(guardUnrecorded) {
		t.Errorf("first restore restore_guard = %q, want %q", check, guardUnrecorded)
	}
	for _, nth := range []string{"second", "third"} {
		runCLIInDir(t, repo, "backup", "restore", "--latest")
		if check := restoreTraceRecord(t, repo).Metadata["restore_guard"]; check != string(guardUnchanged) {
			t.Errorf("%s restore over an unchanged workspace restore_guard = %q, want %q", nth, check, guardUnchanged)
		}
	}

	runCLIInDir(t, repo, "new", "--type", "task", "--title", "Made since", "--topic", "demo", "--parent", epic)
	_, err := runCLIInDirAllowError(t, repo, "backup", "restore", "--latest")
	var conflict RestoreOverwriteError
	if !errors.As(err, &conflict) {
		t.Fatalf("restore over a ticket made since the last restore error = %v (%T), want the overwrite refusal", err, err)
	}
	if check := restoreTraceRecord(t, repo).Metadata["restore_guard"]; check != string(guardChanged) {
		t.Errorf("refused restore restore_guard = %q, want %q", check, guardChanged)
	}
}

// A restore's digest is of one clone's restore, so it does not travel with the
// store: a clone that adopted the store from another that restored has run no
// restore of its own, and its first one is unrecorded, never compared against
// what the other clone's restore left. Driven over a real git remote, because
// the claim is about what sync carries.
func TestARestoreDigestStaysInTheCloneThatRestored(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")
	clone := func(name string) string {
		dir := filepath.Join(base, name)
		runGit(t, base, "clone", remote, name)
		runGit(t, dir, "config", "user.email", name+"@example.com")
		runGit(t, dir, "config", "user.name", name)
		return dir
	}

	restored := clone("alpha")
	if err := os.WriteFile(filepath.Join(restored, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, restored, "add", "-A")
	runGit(t, restored, "commit", "-m", "seed")
	runGit(t, restored, "push", "origin", "HEAD")
	runCLIInDir(t, restored, "init", "--skip-hooks")
	newEpicInDir(t, restored, "Home")
	created := strings.Fields(runCLIInDir(t, restored, "backup", "create"))
	if len(created) != 2 {
		t.Fatalf("backup create printed %v, want a name and a path", created)
	}
	runCLIInDir(t, restored, "backup", "restore", "--path", created[1])
	runCLIInDir(t, restored, "sync", "push", "--set-upstream")

	adopted := clone("bravo")
	runCLIInDir(t, adopted, "init", "--skip-hooks")
	runCLIInDir(t, adopted, "backup", "create")
	runCLIInDir(t, adopted, "backup", "restore", "--latest")
	if check := restoreTraceRecord(t, adopted).Metadata["restore_guard"]; check != string(guardUnrecorded) {
		t.Errorf("first restore in a clone that adopted a restored store restore_guard = %q, want %q", check, guardUnrecorded)
	}
}
