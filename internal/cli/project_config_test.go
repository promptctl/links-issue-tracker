package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

const requirePromptConfig = "[ready]\nrequired_fields = [\"prompt\"]\n"

// configCheckout is a git checkout with one commit, ready for `lit init`.
func configCheckout(t *testing.T) string {
	t.Helper()
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "")
	setRunEnv(t, "HOME", t.TempDir())
	setRunEnv(t, "XDG_CONFIG_HOME", t.TempDir())
	// The checkout's root as git names it, which the file's path is named under.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "m@m.co")
	runGit(t, dir, "config", "user.name", "main")
	runGit(t, dir, "commit", "--allow-empty", "-m", "seed")
	return dir
}

// commitConfigFile commits the checkout's project config file.
func commitConfigFile(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "add", config.CheckoutProjectPath(runEnv(t), pathspec.New(dir)).String())
	runGit(t, dir, "-c", "user.email=lit@test.co", "-c", "user.name=lit", "commit", "-m", "config")
}

// applyProjectConfig puts content in force on the workspace at dir the way its
// owner does: written to the checkout's file, committed, and applied.
func applyProjectConfig(t *testing.T, dir, content string) {
	t.Helper()
	writeCheckoutProject(t, dir, content)
	commitConfigFile(t, dir)
	runCLIInDir(t, dir, "config", "apply")
}

// backlogBlocked is the blocked cell `lit backlog` prints for id, and what the
// run wrote to stderr.
func backlogBlocked(t *testing.T, dir, id string) (cell, stderr string) {
	t.Helper()
	out, errOut, err := runCLIInDirSplit(t, dir, "backlog", "--columns", "id,blocked")
	if err != nil {
		t.Fatalf("lit backlog: %v\n%s%s", err, out, errOut)
	}
	cells := backlogCells(t, out, model.ID(id))
	if len(cells) != 2 {
		t.Fatalf("backlog row for %s = %v, want id and blocked\n%s", id, cells, out)
	}
	return cells[1], errOut
}

// The checkout's file is what `lit config apply` puts in force, and nothing
// else: an edit has no effect while it is uncommitted or unapplied, `lit
// backlog` says so, and once applied the store runs on it.
func TestAProjectConfigTakesEffectOnlyOnceApplied(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir := configCheckout(t)
	if out := runCLIInDir(t, dir, "init", "--skip-hooks"); !strings.Contains(out, "  config: lit's defaults (no project config has been applied)\n") {
		t.Fatalf("lit init with no project config does not say it runs on the defaults:\n%s", out)
	}
	epic := newEpicInDir(t, dir, "config epic")
	task := extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "No prompt", "--topic", "demo", "--type", "task", "--parent", epic))

	path := writeCheckoutProject(t, dir, requirePromptConfig)
	if _, err := runCLIInDirAllowError(t, dir, "config", "apply"); err == nil || !strings.Contains(err.Error(), path+" is not committed") {
		t.Fatalf("lit config apply of an uncommitted file = %v, want it refused as not committed", err)
	}
	if out := runCLIInDir(t, dir, "config", "status"); !strings.Contains(out, path+": has uncommitted changes") {
		t.Errorf("lit config status of an uncommitted file:\n%s", out)
	}
	if cell, stderr := backlogBlocked(t, dir, task); cell != "-" || !strings.Contains(stderr, "lit: "+path+" is not the config in force: has uncommitted changes") {
		t.Errorf("before apply: blocked = %q, stderr:\n%s\nwant unblocked and the file named as not in force", cell, stderr)
	}

	commitConfigFile(t, dir)
	if out := runCLIInDir(t, dir, "config", "status"); !strings.Contains(out, "in force: lit's defaults") || !strings.Contains(out, path+": committed but not in force") {
		t.Errorf("lit config status of a committed, unapplied file:\n%s", out)
	}
	if cell, stderr := backlogBlocked(t, dir, task); cell != "-" || !strings.Contains(stderr, "committed but not in force") {
		t.Errorf("committed, unapplied: blocked = %q, stderr:\n%s\nwant unblocked and the file named as not in force", cell, stderr)
	}

	short := model.DigestConfig(requirePromptConfig).Short()
	if out := runCLIInDir(t, dir, "config", "apply", "--by", "ada"); !strings.HasPrefix(out, "applied "+path+" ("+short+")") {
		t.Fatalf("lit config apply printed:\n%s", out)
	}
	if cell, stderr := backlogBlocked(t, dir, task); cell != "blocked" || strings.Contains(stderr, "not the config in force") {
		t.Errorf("after apply: blocked = %q, stderr:\n%s\nwant the missing prompt to block and no notice", cell, stderr)
	}
	if out := runCLIInDir(t, dir, "config", "status"); !strings.Contains(out, "in force: "+short+", applied by ada at ") || !strings.Contains(out, path+": matches the config in force") {
		t.Errorf("lit config status after apply:\n%s", out)
	}
	if out := runCLIInDir(t, dir, "config", "apply"); !strings.Contains(out, "is already the config in force ("+short+"); nothing applied") {
		t.Errorf("a second apply printed:\n%s\nwant nothing applied", out)
	}

	// The edit after the apply has no effect: the store stays on the applied
	// config until the edit is committed and applied.
	writeCheckoutProject(t, dir, "")
	if cell, _ := backlogBlocked(t, dir, task); cell != "blocked" {
		t.Errorf("an uncommitted edit changed the config in force: blocked = %q", cell)
	}
}

// Apply refuses a file for every fault it holds at once, so a fix is not
// found one refusal at a time.
func TestConfigApplyNamesEveryFault(t *testing.T) {
	t.Parallel()
	dir := configCheckout(t)
	runCLIInDir(t, dir, "init", "--skip-hooks")
	writeCheckoutProject(t, dir, "[sync]\ncadence = \"hourly\"\n[snapshot]\nretention_budget = 0\n")
	out, err := runCLIInDirAllowError(t, dir, "config", "apply")
	if err == nil {
		t.Fatalf("lit config apply of an invalid, uncommitted file succeeded:\n%s", out)
	}
	for _, fault := range []string{"is not committed", "sync.cadence", "snapshot.retention_budget"} {
		if !strings.Contains(err.Error(), fault) {
			t.Errorf("refusal %q does not name %q", err, fault)
		}
	}
	if status := runCLIInDir(t, dir, "config", "status"); !strings.Contains(status, "in force: lit's defaults") {
		t.Errorf("a refused apply changed the config in force:\n%s", status)
	}
}

// `lit init` on a store nothing has been applied to puts the committed file in
// force, so a fresh workspace runs on the config the repository carries.
func TestInitAppliesTheCommittedProjectConfig(t *testing.T) {
	t.Parallel()
	dir := configCheckout(t)
	path := writeCheckoutProject(t, dir, requirePromptConfig)
	commitConfigFile(t, dir)
	short := model.DigestConfig(requirePromptConfig).Short()
	if out := runCLIInDir(t, dir, "init", "--skip-hooks"); !strings.Contains(out, "  config: applied "+path+" ("+short+")\n") {
		t.Fatalf("lit init with a committed project config does not apply it:\n%s", out)
	}
	if out := runCLIInDir(t, dir, "config", "status"); !strings.Contains(out, "in force: "+short) {
		t.Errorf("lit config status after init:\n%s", out)
	}
	// A second init keeps the config in force rather than applying again.
	if out := runCLIInDir(t, dir, "init", "--skip-hooks"); !strings.Contains(out, "  config: "+short+", applied by ") {
		t.Errorf("a second lit init:\n%s", out)
	}
}

// plantProjectConfig puts content in force on the store of the workspace at
// dir without `lit config apply`, so it may be a config apply refuses, as a
// newer lit's can be.
func plantProjectConfig(t *testing.T, dir, content string) {
	t.Helper()
	ws, err := workspace.Resolve(runEnv(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	applyToStore(t, ws.DatabasePath, ws.WorkspaceID, content)
}

// applyToStore puts content in force on the store at databasePath.
func applyToStore(t *testing.T, databasePath, workspaceID, content string) {
	t.Helper()
	ctx := context.Background()
	st, err := engine.Open(ctx, runEnv(t), testStderr(t), engine.ReadWrite, databasePath, workspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	if _, _, err := st.ApplyConfig(ctx, content, "ada"); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// A store read by path runs on the config applied to it, as it does when its
// own checkout reads it: the required field gates its tickets.
func TestLsAtRunsOnTheConfigAppliedToTheStore(t *testing.T) {
	t.Parallel()
	storeDir, issueID := foreignStore(t, "ws-foreign", "proj")
	applyToStore(t, workspace.LocationFromStorageDir(storeDir).DatabasePath, "ws-foreign", requirePromptConfig)

	var out bytes.Buffer
	if err := runList(underInvocation(context.Background()), dirInvocation(t, t.TempDir(), &out, &out, nil), lsSurface, []string{"--at", storeDir, "--columns", "id,blocked"}); err != nil {
		t.Fatalf("ls --at error = %v", err)
	}
	if cells := fieldsOf(lineForID(t, out.String(), model.ID(issueID))); len(cells) != 2 || cells[1] != "blocked" {
		t.Errorf("ls --at row = %v, want the missing prompt to block it\n%s", cells, out.String())
	}
}

// `lit stores --counts` classifies each store on the config applied to it.
func TestStoresCountsRunOnTheConfigAppliedToEachStore(t *testing.T) {
	t.Parallel()
	setRunEnv(t, "XDG_CONFIG_HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repoReal")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, repo)
	storageDir, _ := seedDiscoverableStore(t, repo, "real", "real-workspace-id")
	applyToStore(t, filepath.Join(storageDir, "dolt"), "real-workspace-id", requirePromptConfig)

	rows, err := gatherCrossProjectRollup(underInvocation(context.Background()), runEnv(t), testStderr(t), []string{root})
	if err != nil || len(rows) != 1 || rows[0].Err != nil {
		t.Fatalf("gatherCrossProjectRollup() = (%+v, %v), want one clean row", rows, err)
	}
	if row := rows[0]; row.Ready != 0 || row.InFlight != 1 || row.Blocked != 3 {
		t.Errorf("row counts = ready %d / in-flight %d / blocked %d; want 0 / 1 / 3, the two ready tickets lacking a prompt", row.Ready, row.InFlight, row.Blocked)
	}
}

// `lit quickstart` holds no store of its own, and still runs on the config
// applied to the workspace's store: the committed soil_mode takes effect once
// applied, not before.
func TestQuickstartRunsOnTheAppliedConfig(t *testing.T) {
	t.Parallel()
	dir := configCheckout(t)
	runCLIInDir(t, dir, "init", "--skip-hooks")
	writeCheckoutProject(t, dir, "[quickstart]\nsoil_mode = true\n")
	commitConfigFile(t, dir)
	if out := runCLIInDir(t, dir, "quickstart"); strings.Contains(out, "## Soil") {
		t.Fatalf("quickstart ran on a config not yet applied:\n%s", out)
	}
	runCLIInDir(t, dir, "config", "apply")
	if out := runCLIInDir(t, dir, "quickstart"); !strings.Contains(out, "## Soil") {
		t.Errorf("quickstart does not run on the applied soil_mode:\n%s", out)
	}
}
