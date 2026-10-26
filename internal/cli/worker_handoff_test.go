package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// workerHandoffStdin is the handoff a command run in dir hands the worker it
// spawns, built by the launcher's own function.
func workerHandoffStdin(t testing.TB, dir string) string {
	t.Helper()
	ws, err := workspace.Resolve(testEnv(), dir)
	if err != nil {
		t.Fatalf("resolve the workspace at %s: %v", dir, err)
	}
	handoff, err := handoffOf(underInvocation(context.Background()), ws)
	if err != nil {
		t.Fatalf("handoffOf(%s): %v", dir, err)
	}
	payload, err := json.Marshal(handoff)
	if err != nil {
		t.Fatalf("encode handoff: %v", err)
	}
	return string(payload)
}

// workerArgv is the argv a command run in dir spawns subcommand with, its
// parent wait skipped.
func workerArgv(t testing.TB, dir, subcommand string) []string {
	t.Helper()
	loc, err := workspace.Locate(testEnv(), dir)
	if err != nil {
		t.Fatalf("locate the store for %s: %v", dir, err)
	}
	return []string{subcommand, "--store", loc.StorageDir, "--parent-pid", "0"}
}

// runWorkerInDirAllowError runs a detached worker in-process against the
// workspace at dir, handed what a command run in dir hands the worker it
// spawns.
func runWorkerInDirAllowError(t *testing.T, dir, subcommand string) (string, error) {
	t.Helper()
	return runCLIInDirWithStdin(t, dir, workerHandoffStdin(t, dir), workerArgv(t, dir, subcommand)...)
}

func runWorkerInDir(t *testing.T, dir, subcommand string) string {
	t.Helper()
	out, err := runWorkerInDirAllowError(t, dir, subcommand)
	if err != nil {
		t.Fatalf("%s in %s: %v\noutput:\n%s", subcommand, dir, err, out)
	}
	return out
}

// twoRemoteCheckout is a lit workspace whose repository has two remotes and
// whose branch tracks origin, so the sync-remote precedence asks the upstream.
func twoRemoteCheckout(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-b", "trunk")
	runGit(t, root, "-c", "user.email=h@h.co", "-c", "user.name=h", "commit", "--allow-empty", "-m", "seed")
	for _, remote := range []string{"origin", "backup"} {
		runGit(t, base, "init", "--bare", "-b", "trunk", remote+".git")
		runGit(t, root, "remote", "add", remote, filepath.Join(base, remote+".git"))
		runGit(t, root, "push", remote, "HEAD")
	}
	runGit(t, root, "branch", "--set-upstream-to", "origin/trunk")
	if err := os.MkdirAll(filepath.Join(root, ".lit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lit", "config.toml"), []byte("[sync]\nreceive = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLIInDir(t, root, "init", "--skip-hooks")
	return root
}

// A worker handed a handoff builds the workspace its launcher had, and a
// worker that launches a worker hands on exactly what it was handed.
func TestAHandedWorkspaceIsTheLaunchersAndReHandsItself(t *testing.T) {
	root := twoRemoteCheckout(t)
	ws, err := workspace.Resolve(testEnv(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := underInvocation(context.Background())
	handed, err := handoffOf(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if handed.SyncRemote != "origin" || handed.CheckoutRoot != ws.RootDir || string(handed.Project) != "[sync]\nreceive = false\n" {
		t.Fatalf("handoffOf = %+v; want origin, root %s, and the checkout's config", handed, ws.RootDir)
	}
	payload, err := json.Marshal(handed)
	if err != nil {
		t.Fatal(err)
	}
	built := buildWorkerWorkspace(ctx, testEnv(), ws.Location, strings.NewReader(string(payload)))
	if built.failed != nil {
		t.Fatalf("buildWorkerWorkspace failed: %v", built.failed)
	}
	if built.ws.WorkspaceID != ws.WorkspaceID || built.ws.Project.Source() != "handed" {
		t.Fatalf("built workspace id %q source %q; want %q handed", built.ws.WorkspaceID, built.ws.Project.Source(), ws.WorkspaceID)
	}
	reHanded, err := handoffOf(ctx, built.ws)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reHanded, handed) {
		t.Fatalf("re-handed %+v; want what was handed, %+v", reHanded, handed)
	}
}

// A handed workspace's remotes are the repository's, and its upstream is the
// one it was handed, cycle after cycle: once the checkout that launched the
// worker is gone, neither question touches it.
func TestAHandedWorkspaceAsksNothingOfARemovedCheckout(t *testing.T) {
	root := twoRemoteCheckout(t)
	worktree := filepath.Join(filepath.Dir(root), "wt")
	runGit(t, root, "worktree", "add", "-b", "wt", worktree, "origin/trunk")
	payload := workerHandoffStdin(t, worktree)
	loc, err := workspace.Locate(testEnv(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "worktree", "remove", "--force", worktree)

	ctx := underInvocation(context.Background())
	built := buildWorkerWorkspace(ctx, testEnv(), loc, strings.NewReader(payload))
	if built.failed != nil {
		t.Fatalf("buildWorkerWorkspace after the checkout was removed: %v", built.failed)
	}
	for cycle, remotes := range []*workspace.Remotes{built.ws.Remotes, built.ws.Remotes.Reread()} {
		list, err := remotes.List(ctx)
		if err != nil || len(list) != 2 {
			t.Fatalf("cycle %d: List() = %v, %v; want origin and backup", cycle, list, err)
		}
		if upstream, err := remotes.Upstream(); err != nil || upstream != "origin" {
			t.Fatalf("cycle %d: Upstream() = %q, %v; want the handed origin", cycle, upstream, err)
		}
	}
}

// A mirror whose workspace cannot be built ends the way its other pre-attempt
// deaths do: a failed push outcome, which the FAILING banner reads, and the
// mirror-pending claim it was spawned for cleared.
func TestAMirrorWithAnUnreadableHandoffRecordsAFailedPush(t *testing.T) {
	root := twoRemoteCheckout(t)
	ws, err := workspace.Resolve(testEnv(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mirrorPendingMarkerPath(ws), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLIInDirWithStdin(t, root, "not a handoff", workerArgv(t, root, backgroundMirrorSubcommand)...); err != nil {
		t.Fatalf("mirror: %v\n%s", err, out)
	}
	rec, _, ok := lastPushOutcome(ws, time.Now())
	if !ok || rec.Decision != pushDecisionError || !strings.Contains(rec.Reason, "read the launcher's handoff") {
		t.Fatalf("push outcome = %+v (recorded %t); want an error naming the handoff", rec, ok)
	}
	if owed, err := mirrorOwed(ws); err != nil || owed {
		t.Fatalf("mirrorOwed = %t, %v; want the claim cleared", owed, err)
	}
	if got := lastEvent(t).Facts["config.project"]; got != "handed" {
		t.Fatalf("config.project = %v, want handed", got)
	}
}

// A receive whose workspace cannot be built records the failure as its
// decision, on its event, inside the lifetime its log accounts for. Its event
// still names the project layer it ran with: the empty handed one.
func TestAReceiveWithAnUnreadableHandoffRecordsAReceiveError(t *testing.T) {
	root := twoRemoteCheckout(t)
	out, err := runCLIInDirWithStdin(t, root, "not a handoff", workerArgv(t, root, backgroundReceiveSubcommand)...)
	if err != nil {
		t.Fatalf("receive: %v\n%s", err, out)
	}
	if !strings.Contains(out, " receive start pid=") || !strings.Contains(out, " receive end pid=") {
		t.Fatalf("receive log lines missing from the worker's output:\n%s", out)
	}
	event := lastEvent(t)
	outcomes := event.Occurrences["receive.outcomes"]
	if len(outcomes) != 1 || !strings.Contains(fmt.Sprint(outcomes[0]), "read the launcher's handoff") {
		t.Fatalf("receive.outcomes = %v; want one error naming the handoff", outcomes)
	}
	if got := event.Facts["config.project"]; got != "handed" {
		t.Fatalf("config.project = %v, want handed", got)
	}
}

// Which project layer a command read is on its event: the checkout's file for
// a command run in a checkout, and the handed one for a worker.
func TestTheEventNamesTheProjectLayerItRead(t *testing.T) {
	root := twoRemoteCheckout(t)
	runCLIInDir(t, root, "backlog")
	ws, err := workspace.Resolve(testEnv(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := lastEvent(t).Facts["config.project"], "checkout:"+filepath.Join(ws.RootDir, ".lit", "config.toml"); got != want {
		t.Fatalf("backlog's config.project = %v, want %q", got, want)
	}
	runWorkerInDir(t, root, backgroundReceiveSubcommand)
	if got := lastEvent(t).Facts["config.project"]; got != "handed" {
		t.Fatalf("the receive worker's config.project = %v, want handed", got)
	}
}
