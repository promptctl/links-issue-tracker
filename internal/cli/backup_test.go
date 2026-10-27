package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/backup"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// resolveRestoreSource is the single authority `backup restore` resolves its
// source through; these assert the documented cap of two sources (explicit
// path, latest backup) and that the degenerate combinations fail loudly rather
// than picking a silent precedence.
func TestResolveRestoreSourceCap(t *testing.T) {
	t.Parallel()
	t.Run("no source is a usage error", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		_, err := resolveRestoreSource(ap, nil, false)
		if _, ok := err.(UsageError); !ok {
			t.Fatalf("resolveRestoreSource(no source) error = %v, want UsageError", err)
		}
	})

	t.Run("both sources is a mutual-exclusion error, not silent precedence", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		_, err := resolveRestoreSource(ap, &inputFile{path: "/some/export.json"}, true)
		ue, ok := err.(UsageError)
		if !ok || !strings.Contains(ue.Message, "mutually exclusive") {
			t.Fatalf("resolveRestoreSource(both) error = %v, want mutually-exclusive UsageError", err)
		}
	})

	t.Run("explicit file passes through as read", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		explicit := inputFile{path: "/some/export.json", data: []byte("{}")}
		got, err := resolveRestoreSource(ap, &explicit, false)
		if err != nil || got.path != explicit.path || string(got.data) != string(explicit.data) {
			t.Fatalf("resolveRestoreSource(path) = %+v, %v; want %+v", got, err, explicit)
		}
	})

	t.Run("latest with no backups fails loudly", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		_, err := resolveRestoreSource(ap, nil, true)
		if err == nil || !strings.Contains(err.Error(), "no backups available") {
			t.Fatalf("resolveRestoreSource(latest, empty) error = %v, want no backups available", err)
		}
	})

	t.Run("latest reads the newest snapshot", func(t *testing.T) {
		dir := t.TempDir()
		ap := &app.App{Workspace: workspace.Info{Location: workspace.Location{StorageDir: dir}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}}
		export := model.Export{Version: 1, WorkspaceID: "ws", ExportedAt: time.Now().UTC()}
		taken, err := backup.Take(context.Background(), dir, backup.Budget, func() (model.Export, error) { return export, nil })
		if err != nil {
			t.Fatalf("backup.Take() error = %v", err)
		}
		snapshot := taken.Written
		want, err := os.ReadFile(snapshot.Path)
		if err != nil {
			t.Fatalf("read snapshot: %v", err)
		}
		got, err := resolveRestoreSource(ap, nil, true)
		if err != nil || got.path != snapshot.Path || string(got.data) != string(want) {
			t.Fatalf("resolveRestoreSource(latest) = %q (%d bytes), %v; want %q (%d bytes)", got.path, len(got.data), err, snapshot.Path, len(want))
		}
	})
}

// An export that cannot be read is refused as usage in the parse, before the
// workspace opens, as an import spec is: outside any workspace the refusal is
// the same, and the invocation's event records the usage exit.
func TestRestoreRefusesAnUnreadablePathBeforeTheWorkspaceOpens(t *testing.T) {
	isolateEvents(t)
	missing := filepath.Join(t.TempDir(), "missing.json")
	_, err := runCLIInDirAllowError(t, t.TempDir(), "backup", "restore", "--path", missing)
	var usage UsageError
	if !errors.As(err, &usage) || !strings.HasPrefix(usage.Message, "read --path: open ") {
		t.Fatalf("backup restore --path <missing> outside any workspace error = %v, want the unreadable-file UsageError", err)
	}
	if ev := lastEvent(t); ev.ExitCode != ExitUsage {
		t.Errorf("event exit_code = %d, want %d", ev.ExitCode, ExitUsage)
	}
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

// Each `restore --latest` restores the previous restore's pre-restore backup,
// so a run of them swings the workspace between two states. The backups hold
// each state once, so the run takes no slot from the distinct backups made
// before it.
func TestRepeatedRestoresKeepEveryDistinctBackup(t *testing.T) {
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	for _, title := range []string{"one", "two", "three"} {
		runCLIInDir(t, repo, "new", "--type", "task", "--title", title, "--topic", "demo", "--parent", epic)
		if title != "three" {
			runCLIInDir(t, repo, "backup", "create")
		}
	}
	for range backup.Budget {
		runCLIInDir(t, repo, "backup", "restore", "--latest")
	}

	var held []string
	for _, line := range strings.Split(strings.TrimSpace(runCLIInDir(t, repo, "backup", "list")), "\n") {
		fields := strings.Fields(line)
		path := fields[len(fields)-1]
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		export, _, err := syncfile.Decode(payload)
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		var titles []string
		for _, issue := range export.Issues {
			titles = append(titles, issue.Title)
		}
		slices.Sort(titles)
		held = append(held, strings.Join(titles, ","))
	}
	slices.Sort(held)
	if want := []string{"Home,one", "Home,one,three,two", "Home,one,two"}; !slices.Equal(held, want) {
		t.Errorf("backups hold %q, want each state once: %q", held, want)
	}
}

// The snapshot a backup took, whether the set already held its content, how
// long it waited for the set's lock, how many killed backups' temp files it
// removed and how many snapshots its prune removed are on the event of
// `backup create` and of a restore alike.
func TestBackupEventNamesTheSnapshotItTook(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	created := strings.Fields(runCLIInDir(t, repo, "backup", "create"))
	name := created[0]
	assertFacts(t, map[string]string{"backup.snapshot": name, "backup.refreshed": "false", "backup.residue_removed": "0", "backup.pruned": "0"})
	if wait, ok := writtenFacts(t, lastEvent(t))["backup.lock_wait_ms"].(float64); !ok || wait < 0 {
		t.Errorf("backup create event backup.lock_wait_ms = %v, want a wait in milliseconds", wait)
	}
	residue := filepath.Join(filepath.Dir(created[1]), "killed.json.tmp-7")
	if err := os.WriteFile(residue, []byte("{"), 0o644); err != nil {
		t.Fatalf("leave a killed backup's temp file: %v", err)
	}
	runCLIInDir(t, repo, "backup", "create")
	assertFacts(t, map[string]string{"backup.snapshot": name, "backup.refreshed": "true", "backup.residue_removed": "1"})
	runCLIInDir(t, repo, "backup", "restore", "--latest")
	assertFacts(t, map[string]string{"backup.snapshot": name, "backup.refreshed": "true"})

	runCLIInDir(t, repo, "new", "--type", "task", "--title", "Made since", "--topic", "demo", "--parent", epic)
	runCLIInDir(t, repo, "backup", "restore", "--latest", "--force")
	facts := writtenFacts(t, lastEvent(t))
	if facts["backup.snapshot"] == name || facts["backup.refreshed"] != false {
		t.Errorf("restore over a moved workspace event facts = %v, want a new snapshot, not refreshed", facts)
	}
	// The set now holds name and the moved state's snapshot; keeping one
	// refreshes name and removes the other.
	runCLIInDir(t, repo, "backup", "create", "--keep", "1")
	assertFacts(t, map[string]string{"backup.snapshot": name, "backup.refreshed": "true", "backup.pruned": "1"})
}
