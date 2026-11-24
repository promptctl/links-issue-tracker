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

	"github.com/promptctl/links-issue-tracker/internal/deadline"
	"github.com/promptctl/links-issue-tracker/internal/suiteenv"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
	"github.com/promptctl/links-issue-tracker/internal/workspace/workspacetest"
)

// workerHandoffStdin is the handoff a command run in dir hands the worker it
// spawns, built by the launcher's own function, from a run in t's environment.
func workerHandoffStdin(t testing.TB, dir string) string {
	t.Helper()
	return workerHandoffStdinUnder(t, dir, runEnv(t).Deadlines())
}

// workerHandoffStdinUnder is workerHandoffStdin from a run under deadlines.
func workerHandoffStdinUnder(t testing.TB, dir string, deadlines deadline.Deadlines) string {
	t.Helper()
	ws := workspacetest.Resolved(t, runEnv(t), dir)
	handoff, err := handoffOf(underInvocation(context.Background()), ws, withFactParts(runEnv(t), func(p *suiteenv.FactParts) { p.Deadlines = deadlines }))
	if err != nil {
		t.Fatalf("handoffOf(%s): %v", dir, err)
	}
	payload, err := json.Marshal(handoff)
	if err != nil {
		t.Fatalf("encode handoff: %v", err)
	}
	return string(payload)
}

// workerArgv is the argv a command run in dir spawns subcommand with, naming
// a spawner that has already exited, so the worker waits for no one.
func workerArgv(t testing.TB, dir, subcommand string) []string {
	t.Helper()
	loc, err := workspace.Locate(runEnv(t), dir)
	if err != nil {
		t.Fatalf("locate the store for %s: %v", dir, err)
	}
	return []string{subcommand, "--store", loc.StorageDir, "--parent-pid", exitedSpawner(t).flag()}
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
	writeCheckoutProject(t, root, "[sync]\nreceive = false\n")
	runCLIInDir(t, root, "init", "--skip-hooks")
	return root
}

// A worker handed a handoff builds the workspace its launcher had, runs under
// the launcher's run facts and says so on its event, and a worker that
// launches a worker hands on exactly what it was handed.
func TestAHandedWorkspaceIsTheLaunchersAndReHandsItself(t *testing.T) {
	t.Parallel()
	root := twoRemoteCheckout(t)
	ws := workspacetest.Resolved(t, runEnv(t), root)
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	// The launcher's run carries facts the worker's own process does not, so
	// the build can only have them from the handoff.
	launcher := withFactParts(runEnv(t), func(p *suiteenv.FactParts) {
		p.Deadlines = launcherDeadlines(t)
		p.ReleasedSchema = version.SchemaCeiling{Max: 7, Source: "the launcher's own", Bound: version.BoundReleased}
		p.RaisePolicy = version.RaisePolicy{Release: false}
	})
	handed, err := handoffOf(ctx, ws, launcher)
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
	built, ok := buildWorkerWorkspace(ctx, testStderr(t), runEnv(t), ws.Location, strings.NewReader(string(payload))).(builtWorker)
	if !ok {
		t.Fatalf("buildWorkerWorkspace did not build the handed workspace")
	}
	if built.ws.WorkspaceID != ws.WorkspaceID || built.ws.Project.Source() != "handed" {
		t.Fatalf("built workspace id %q source %q; want %q handed", built.ws.WorkspaceID, built.ws.Project.Source(), ws.WorkspaceID)
	}
	if built.env.Facts() != launcher.Facts() {
		t.Fatalf("the worker runs under deadlines %+v, ceiling %+v, policy %+v; want the launcher's %+v, %+v, %+v",
			built.env.Deadlines().Figures(), built.env.ReleasedSchema(), built.env.RaisePolicy(), launcher.Deadlines().Figures(), launcher.ReleasedSchema(), launcher.RaisePolicy())
	}
	ev := recorder.Finish(invocationEnd(time.Now(), nil, nil))
	if ev.Facts["run_facts"] != launcher.Facts() || ev.Facts["run_facts.source"] != runFactsHanded {
		t.Fatalf("the worker's event records run facts %+v from %v; want the launcher's, handed", ev.Facts["run_facts"], ev.Facts["run_facts.source"])
	}
	reHanded, err := handoffOf(ctx, built.ws, built.env)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reHanded, handed) {
		t.Fatalf("re-handed %+v; want what was handed, %+v", reHanded, handed)
	}
}

// launcherDeadlines are deadlines no suite run carries, every figure its own,
// so a worker can only run under them by being handed them.
func launcherDeadlines(t testing.TB) deadline.Deadlines {
	t.Helper()
	d, err := deadline.Of(deadline.Figures{MirrorHold: time.Second, MirrorPush: 2 * time.Second, Receive: 3 * time.Second, PushProof: 4 * time.Second, Compaction: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A handoff that does not carry every run fact the worker runs under is no
// handoff: no facts at all, a fact or a field of one missing, or a deadline
// that is not positive fails the build, naming what is missing. The failed
// build runs under its own process's facts, and its event says so.
func TestAHandoffWithoutItsRunFactsIsRefused(t *testing.T) {
	t.Parallel()
	root := twoRemoteCheckout(t)
	ws := workspacetest.Resolved(t, runEnv(t), root)
	handed, err := handoffOf(underInvocation(context.Background()), ws, runEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(handed)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path []string
		set  json.RawMessage // nil deletes the key at path
		want string
	}{
		{"no run facts", []string{"facts"}, nil, "carries no run facts"},
		{"no deadlines", []string{"facts", "deadlines"}, nil, "carry no deadlines"},
		{"a deadline of zero", []string{"facts", "deadlines", "receive"}, json.RawMessage("0"), "the receive deadline must be positive"},
		{"a deadline missing", []string{"facts", "deadlines", "push_proof"}, nil, "the push proof deadline must be positive"},
		{"no released-schema ceiling", []string{"facts", "released_schema"}, nil, "carry no released_schema"},
		{"a ceiling with no maximum", []string{"facts", "released_schema", "max"}, nil, "carry no released_schema.max"},
		{"a ceiling of another bound", []string{"facts", "released_schema", "bound"}, json.RawMessage(`"hold"`), `must be the "released" bound`},
		{"no raise policy", []string{"facts", "raise_policy"}, nil, "carry no raise_policy"},
		{"a raise policy that says nothing", []string{"facts", "raise_policy", "release"}, nil, "carry no raise_policy.release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edited := editJSON(t, payload, tc.path, tc.set)
			recorder := telemetry.Begin(telemetry.Event{})
			ctx := telemetry.With(context.Background(), recorder)
			failed, ok := buildWorkerWorkspace(ctx, testStderr(t), runEnv(t), ws.Location, strings.NewReader(string(edited))).(failedWorker)
			if !ok || !strings.Contains(failed.err.Error(), tc.want) {
				t.Fatalf("buildWorkerWorkspace(%s) = %+v; want a failed build naming %q", edited, failed, tc.want)
			}
			ev := recorder.Finish(invocationEnd(time.Now(), nil, nil))
			if ev.Facts["run_facts"] != runEnv(t).Facts() || ev.Facts["run_facts.source"] != runFactsOwn {
				t.Fatalf("the failed build's event records run facts %+v from %v; want its own process's, own", ev.Facts["run_facts"], ev.Facts["run_facts.source"])
			}
		})
	}
}

// editJSON is doc with the value at path set to set, or the key deleted when
// set is nil.
func editJSON(t *testing.T, doc []byte, path []string, set json.RawMessage) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc, &fields); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	if len(path) == 1 {
		if set == nil {
			delete(fields, path[0])
		} else {
			fields[path[0]] = set
		}
	} else {
		fields[path[0]] = editJSON(t, fields[path[0]], path[1:], set)
	}
	edited, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return edited
}

// What a spawn starts is decided from values alone: the worker runs in its
// store's directory, as the hidden subcommand told its store and its spawner,
// in the environment it was given, reading the handoff's own encoding.
func TestTheWorkerSpecIsTheSpawnsDecision(t *testing.T) {
	t.Parallel()
	handoff := workerHandoff{CheckoutRoot: "/checkout", SyncRemote: "origin", Project: []byte("[sync]\n"), Facts: runEnv(t).Facts()}
	from, err := parseSpawner("4242")
	if err != nil {
		t.Fatal(err)
	}
	env := runEnv(t).With("WORKER_ONLY=1")
	spec, err := workerSpecOf(handoff, "/store", backgroundReceiveSubcommand, env, from)
	if err != nil {
		t.Fatalf("workerSpecOf() error = %v", err)
	}
	if want := []string{backgroundReceiveSubcommand, "--store", "/store", "--parent-pid", "4242"}; spec.dir != "/store" || !reflect.DeepEqual(spec.args, want) {
		t.Fatalf("spec runs %v in %s; want %v in /store", spec.args, spec.dir, want)
	}
	if v, _ := spec.env.Lookup("WORKER_ONLY"); v != "1" {
		t.Fatalf("spec env %v; want the environment it was given", spec.env.List())
	}
	decoded, err := decodeHandoff(strings.NewReader(string(spec.handoff)))
	if err != nil || !reflect.DeepEqual(decoded, handoff) {
		t.Fatalf("spec handoff decodes to %+v, %v; want %+v", decoded, err, handoff)
	}
	if spec.holds != nil || spec.log != nil {
		t.Fatalf("spec carries holds %v and log %v; the spawner takes those", spec.holds, spec.log)
	}
}

// A handed workspace's remotes are the repository's, and its upstream is the
// one it was handed, cycle after cycle: once the checkout that launched the
// worker is gone, neither question touches it.
func TestAHandedWorkspaceAsksNothingOfARemovedCheckout(t *testing.T) {
	t.Parallel()
	root := twoRemoteCheckout(t)
	worktree := filepath.Join(filepath.Dir(root), "wt")
	runGit(t, root, "worktree", "add", "-b", "wt", worktree, "origin/trunk")
	payload := workerHandoffStdin(t, worktree)
	loc, err := workspace.Locate(runEnv(t), worktree)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "worktree", "remove", "--force", worktree)

	ctx := underInvocation(context.Background())
	built := buildWorkerWorkspace(ctx, testStderr(t), runEnv(t), loc, strings.NewReader(payload))
	if failed, ok := built.(failedWorker); ok {
		t.Fatalf("buildWorkerWorkspace after the checkout was removed: %v", failed.err)
	}
	for cycle, remotes := range []*workspace.Remotes{built.info().Remotes, built.info().Remotes.Reread()} {
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
	t.Parallel()
	root := twoRemoteCheckout(t)
	ws := workspacetest.Resolved(t, runEnv(t), root)
	if err := os.WriteFile(mirrorPendingMarkerPath(ws), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLIInDirWithStdin(t, root, "not a handoff", workerArgv(t, root, backgroundMirrorSubcommand)...); err != nil {
		t.Fatalf("mirror: %v\n%s", err, out)
	}
	rec, _, ok := lastPushOutcome(testStderr(t), ws, time.Now())
	if !ok || rec.Decision != pushDecisionError || !strings.Contains(rec.Reason, "read the launcher's handoff") {
		t.Fatalf("push outcome = %+v (recorded %t); want an error naming the handoff", rec, ok)
	}
	if owed, err := mirrorOwed(ws); err != nil || owed {
		t.Fatalf("mirrorOwed = %t, %v; want the claim cleared", owed, err)
	}
	if got := lastEvent(t).Facts["config.project"]; got != "none" {
		t.Fatalf("config.project = %v, want none", got)
	}
}

// A receive whose workspace cannot be built records the failure as its
// decision, on its event, inside the lifetime its log accounts for. Its event
// says it ran with no project layer.
func TestAReceiveWithAnUnreadableHandoffRecordsAReceiveError(t *testing.T) {
	t.Parallel()
	root := twoRemoteCheckout(t)
	out, err := runCLIInDirWithStdin(t, root, "not a handoff", workerArgv(t, root, backgroundReceiveSubcommand)...)
	if err != nil {
		t.Fatalf("receive: %v\n%s", err, out)
	}
	if !strings.Contains(out, " receive start pid=") || !strings.Contains(out, " receive end pid=") {
		t.Fatalf("receive log lines missing from the worker's output:\n%s", out)
	}
	// The failed build was handed no deadline, so the start line names none.
	if !strings.Contains(out, " deadline=none (no handoff was read)") {
		t.Fatalf("the failed build's start line names a deadline it was never handed:\n%s", out)
	}
	event := lastEvent(t)
	outcomes := event.Occurrences["receive.outcomes"]
	if len(outcomes) != 1 || !strings.Contains(fmt.Sprint(outcomes[0]), "read the launcher's handoff") {
		t.Fatalf("receive.outcomes = %v; want one error naming the handoff", outcomes)
	}
	if got := event.Facts["config.project"]; got != "none" {
		t.Fatalf("config.project = %v, want none", got)
	}
}

// Which project layer a command read is on its event: the checkout's file for
// a command run in a checkout, and the handed one for a worker.
func TestTheEventNamesTheProjectLayerItRead(t *testing.T) {
	t.Parallel()
	root := twoRemoteCheckout(t)
	runCLIInDir(t, root, "backlog")
	checkoutFile := filepath.Join(workspacetest.Resolved(t, runEnv(t), root).RootDir, ".lit", "config.toml")
	if got, want := lastEvent(t).Facts["config.project"], "checkout:"+checkoutFile; got != want {
		t.Fatalf("backlog's config.project = %v, want %q", got, want)
	}
	runWorkerInDir(t, root, backgroundReceiveSubcommand)
	if got := lastEvent(t).Facts["config.project"]; got != "handed" {
		t.Fatalf("the receive worker's config.project = %v, want handed", got)
	}
}
