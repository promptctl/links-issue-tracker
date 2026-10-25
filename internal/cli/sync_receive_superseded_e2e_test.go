package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAReceiveTheLiveStoreSupersededLandsNothingAndTheNextLands drives the
// automatic receive with a write on the live store between its take and its
// landing: bravo's receive asks the remote, takes its clone and fetches alpha's
// second ticket, and then bravo pulls alpha's third itself. The landing finds
// the live tracking ref moved since the take, lands nothing of its fetch and
// says so on the event; bravo stays level with the remote rather than wound
// back onto the second ticket's head, and received-refs.last stays as it
// stood, since the store does not hold what the receive observed. The next
// receive asks, finds the remote unlike the record, and lands.
// [LAW:behavior-not-structure] asserted through the event, the record a
// receive reads, and the freshness a command reports.
func TestAReceiveTheLiveStoreSupersededLandsNothingAndTheNextLands(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	producer := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, producer, "config", "user.email", "a@a.co")
	runGit(t, producer, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	runCLIInDir(t, producer, "init", "--skip-hooks")
	epicID := newEpicInDir(t, producer, "shared-epic")
	runCLIInDir(t, producer, "new", "--title", "first-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks")
	runCLIInDir(t, producer, "new", "--title", "second-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push")

	ws, err := workspace.Resolve(testEnv(), consumer)
	if err != nil {
		t.Fatalf("resolve consumer workspace: %v", err)
	}
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	gitRemotes, err := workspace.NewRemotes(testEnv(), ws.RootDir).List()
	if err != nil {
		t.Fatalf("read the consumer's git remotes: %v", err)
	}
	observed, err := askRemote(ctx, testEnv(), ws, gitRemotes)
	if err != nil || observed.record() == nil {
		t.Fatalf("askRemote() = %+v, %v; want the remote's advertisement", observed, err)
	}
	clone, err := takeReceiveClone(ctx, ws)
	if err != nil {
		t.Fatalf("takeReceiveClone() error = %v", err)
	}
	defer removeReceiveClone(clone)
	fetch, err := fetchIntoClone(ctx, testEnv(), ws, clone)
	if err != nil || fetch.fetchErr != nil {
		t.Fatalf("fetchIntoClone() = %+v, %v; want a fetch that worked", fetch, err)
	}

	runCLIInDir(t, producer, "new", "--title", "third-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push")
	runCLIInDir(t, consumer, "sync", "pull")
	previous := []byte("origin elsewhere\nold\trefs/dolt/data\n")
	if err := store.WriteReceivedRefs(ws.DatabasePath, previous); err != nil {
		t.Fatalf("seed received-refs.last: %v", err)
	}

	outcome := landAndRecord(ctx, testEnv(), ws, clone, fetch, observed)
	if outcome.err != nil || outcome.landed != store.FetchedHeadSuperseded {
		t.Fatalf("landAndRecord() landed %s, err %v; want superseded by bravo's pull", outcome.landed, outcome.err)
	}
	ev := writtenEvent(t, recorder.Finish(invocationEnd(time.Now(), nil, nil)))
	outcomes := ev.Occurrences["receive.outcomes"]
	if len(outcomes) != 1 {
		t.Fatalf("receive.outcomes = %v; want the one receive's outcome", outcomes)
	}
	if value, _ := outcomes[0].(map[string]any); value["receive.landed"] != "superseded" {
		t.Fatalf("receive outcome = %v; want receive.landed superseded", value)
	}
	if got := readReceivedRefs(ws); string(got) != string(previous) {
		t.Fatalf("received-refs.last = %q after a superseded landing; want it as it stood, %q", got, previous)
	}
	assertLevelWithTheRemote(t, consumer, fetch.target)

	runCLIInDir(t, consumer, "sync", backgroundReceiveSubcommand, "--parent-pid", "0")
	next := lastEvent(t).Occurrences["receive.outcomes"]
	if len(next) != 1 {
		t.Fatalf("the next receive's receive.outcomes = %v; want its one outcome", next)
	}
	if value, _ := next[0].(map[string]any); value["receive.landed"] != "unchanged" || value["receive.decision"] != "up_to_date" {
		t.Fatalf("the next receive's outcome = %v; want it to fetch and land the ref bravo's pull left, unchanged and up to date", value)
	}
	if got := readReceivedRefs(ws); string(got) == string(previous) {
		t.Fatalf("received-refs.last still %q after the next receive settled; want the remote's advertisement", got)
	}
	assertLevelWithTheRemote(t, consumer, fetch.target)
}

// assertLevelWithTheRemote fails unless dir's branch and its tracking ref of
// target name the same commit, as the last fetch or push left them.
func assertLevelWithTheRemote(t *testing.T, dir string, target syncTarget) {
	t.Helper()
	ws, err := workspace.Resolve(testEnv(), dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	session, closeStore, err := openSyncSession(underInvocation(context.Background()), testEnv(), ws)
	if err != nil {
		t.Fatalf("open sync session at %s: %v", dir, err)
	}
	defer func() { _ = closeStore() }()
	freshness, err := session.syncer.SyncFreshness(context.Background(), target.remote, target.branch)
	if err != nil {
		t.Fatalf("SyncFreshness(%s): %v", dir, err)
	}
	if freshness.Ahead != 0 || freshness.Behind != 0 {
		t.Fatalf("%s is ahead %d, behind %d of its tracking ref; want level with the remote", dir, freshness.Ahead, freshness.Behind)
	}
}
