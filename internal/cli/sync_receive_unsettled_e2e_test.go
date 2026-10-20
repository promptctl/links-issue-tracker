package cli

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAReceiveThatFetchedAndKeepsStoppingReachesTheFailureSurface drives the
// automatic receive on a checkout whose fetch succeeds and whose landing then
// fails, from a real cause: a standing holder of the live store's commit lock,
// taken after the clone and its fetch, the one place a held live store does
// not already refuse the receive. The first stop is a run of one: the event
// and the fetch-success marker say what happened, and nobody is paged. Once
// the run has lasted past unsettledLastingAfter with no settle, the next
// receive leaves the receive_unsettled block for the next command and pages
// the owner once, and the staleness banner never blames the fetch. A receive
// that settles afterwards retires the block and ends the run.
// [LAW:behavior-not-structure] asserted through the files a command reads,
// the event, and the owner hook's own output.
func TestAReceiveThatFetchedAndKeepsStoppingReachesTheFailureSurface(t *testing.T) {
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
	runCLIInDir(t, producer, "init", "--skip-hooks", "--skip-agents")
	epicID := newEpicInDir(t, producer, "shared-epic")
	runCLIInDir(t, producer, "new", "--title", "first-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, producer, "new", "--title", "second-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push")

	ws, err := workspace.Resolve(consumer)
	if err != nil {
		t.Fatalf("resolve consumer workspace: %v", err)
	}
	sink := filepath.Join(t.TempDir(), "notifications")
	addOwnerNotifyHook(t, ws, `printf "%s\n" "$LIT_NOTIFY_KIND" >> `+sink)
	if err := os.Remove(fetchSuccessMarkerPath(ws)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear the fetch-success marker: %v", err)
	}
	oneOutcome := func(outcomes []any) map[string]any {
		t.Helper()
		if len(outcomes) != 1 {
			t.Fatalf("receive.outcomes = %v; want the one receive's outcome", outcomes)
		}
		outcome, _ := outcomes[0].(map[string]any)
		return outcome
	}
	// stoppedReceive is the automatic receive's tail (receiveOnce) run after
	// its clone and fetch, with the commit lock held from the fetch on.
	stoppedReceive := func() map[string]any {
		t.Helper()
		recorder := telemetry.Begin(telemetry.Event{})
		ctx := telemetry.With(context.Background(), recorder)
		clone, err := takeReceiveClone(ctx, ws)
		if err != nil {
			t.Fatalf("takeReceiveClone() error = %v", err)
		}
		defer removeReceiveClone(clone)
		fetch, err := fetchIntoClone(ctx, ws, clone)
		if err != nil || fetch.fetchErr != nil {
			t.Fatalf("fetchIntoClone() = %+v, %v; want a fetch that worked", fetch, err)
		}
		release := holdTheCommitLock(t, ws)
		outcome := landAndRecord(ctx, ws, clone, fetch, remoteAdvertisement{})
		release()
		surfaceReceiveOutcome(ctx, ws, outcome, time.Now())
		return oneOutcome(writtenEvent(t, recorder.Finish(invocationEnd(time.Now(), nil, nil))).Occurrences["receive.outcomes"])
	}
	receive := func() map[string]any {
		t.Helper()
		runCLIInDir(t, consumer, "sync", backgroundReceiveSubcommand, "--parent-pid", "0")
		return oneOutcome(lastEvent(t).Occurrences["receive.outcomes"])
	}

	// The first stop starts a run and surfaces nothing but its record.
	first := stoppedReceive()
	if first["receive.stopped"] != "land" || first["receive.decision"] != "error" || !(first["receive.unsettled_since"].(float64) > 0) {
		t.Fatalf("first receive outcome = %v; want a landing stop that starts a run", first)
	}
	if stops := receiveTraceStops(t, ws); len(stops) != 1 || stops[0] != "land" {
		t.Fatalf("receive traces' stopped = %q; want the one receive's landing stop", stops)
	}
	if _, err := os.Stat(fetchSuccessMarkerPath(ws)); err != nil {
		t.Fatalf("a fetch that worked did not move the fetch-success marker: %v", err)
	}
	if _, err := os.Stat(receiveBlockPendingPath(ws)); !os.IsNotExist(err) {
		t.Fatalf("a single post-fetch stop left a block (stat err = %v); one stop is not a lasting failure", err)
	}
	if _, err := os.Stat(sink); !os.IsNotExist(err) {
		t.Fatalf("a single post-fetch stop paged the owner (stat err = %v)", err)
	}

	// The run has lasted past the threshold with no settle anywhere.
	began := time.Now().Add(-unsettledLastingAfter - time.Minute)
	if err := store.WriteUnsettledFirst(ws.DatabasePath, began); err != nil {
		t.Fatalf("age the run: %v", err)
	}
	second := stoppedReceive()
	if second["receive.stopped"] != "land" || second["receive.unsettled_since"] != float64(began.Unix()) {
		t.Fatalf("second receive outcome = %v; want a landing stop in the run that began at %d", second, began.Unix())
	}
	block, err := os.ReadFile(receiveBlockPendingPath(ws))
	if err != nil {
		t.Fatalf("a lasting run of post-fetch stops left no block for the next command: %v", err)
	}
	for _, want := range []string{
		"lit's automatic receive fetched the remote but could not bring it into this checkout",
		"stopped at landing the fetch on the live store",
		"lit sync pull",
		"ESCALATION — BLOCKED",
	} {
		if !strings.Contains(string(block), want) {
			t.Fatalf("receive_unsettled block missing %q:\n%s", want, block)
		}
	}
	// The divergence classes' headline and their age-derived escalation.
	for _, wrong := range []string{"could not resolve a backlog divergence", "where a divergence is routine", "ESCALATION — recent"} {
		if strings.Contains(string(block), wrong) {
			t.Fatalf("receive_unsettled block carries the divergence classes' %q:\n%s", wrong, block)
		}
	}
	if pages, err := os.ReadFile(sink); err != nil || string(pages) != "receive_unsettled\n" {
		t.Fatalf("owner hook output = %q (err = %v); want one receive_unsettled page", pages, err)
	}

	// The next command prints the block and does not blame the fetch.
	if err := markReceiveAttempt(ws); err != nil {
		t.Fatalf("mark receive attempt: %v", err)
	}
	surface := captureStderr(t, func() { runCLIInDir(t, consumer, "backlog") })
	if !strings.Contains(surface, "could not bring it into this checkout") {
		t.Fatalf("the next command did not print the receive_unsettled block:\n%s", surface)
	}
	if strings.Contains(surface, "run 'lit sync fetch'") {
		t.Fatalf("the staleness banner blamed a fetch that worked:\n%s", surface)
	}

	// The cause is gone: the next receive settles and ends the run.
	if err := writeMarkerAtomic(receiveBlockPendingPath(ws), block); err != nil {
		t.Fatalf("re-leave the block: %v", err)
	}
	third := receive()
	if writes := lastEvent(t).Occurrences["settled_last.writes"]; len(writes) != 1 || writes[0].(map[string]any)["settled_last.error"] != "" {
		t.Fatalf("settled_last.writes = %v; want the one settle's record, written", writes)
	}
	if third["receive.stopped"] != "" || third["receive.unsettled_since"] != float64(0) || !(third["receive.settled_last"].(float64) > 0) {
		t.Fatalf("third receive outcome = %v; want a receive that ran to the end, settled, with no run current", third)
	}
	if stops := receiveTraceStops(t, ws); len(stops) != 3 || stops[2] != "" {
		t.Fatalf("receive traces' stopped = %q; want the third receive's empty stop, it ran to the end", stops)
	}
	if _, err := os.Stat(receiveBlockPendingPath(ws)); !os.IsNotExist(err) {
		t.Fatalf("a receive that settled left the receive_unsettled block pending (stat err = %v)", err)
	}
	if pages, _ := os.ReadFile(sink); string(pages) != "receive_unsettled\n" {
		t.Fatalf("owner hook output after the settle = %q; want still the one page", pages)
	}
}

// holdTheCommitLock makes every settle on ws's live store stop from a real
// cause after its fetch has worked: a standing holder of the live store's
// commit lock, which the receive's clone and its fetch never take. The
// returned release ends the hold; the test's cleanup ends it otherwise.
func holdTheCommitLock(t *testing.T, ws workspace.Info) (release func()) {
	t.Helper()
	unlock, err := store.LockCommitPath(context.Background(), ws.DatabasePath)
	if err != nil {
		t.Fatalf("hold the commit lock: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if err := unlock(); err != nil {
				t.Errorf("release the commit lock: %v", err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

// addOwnerNotifyHook turns the owner channel on for an initialized workspace,
// keeping whatever its project config already holds.
func addOwnerNotifyHook(t *testing.T, ws workspace.Info, hook string) {
	t.Helper()
	t.Setenv(DisableAutoSyncEnvVar, "0")
	t.Setenv("LIT_CONFIG_GLOBAL_PATH", filepath.Join(t.TempDir(), "global-config.toml"))
	path := filepath.Join(ws.RootDir, ".lit", "config.toml") // where config.Load reads the project layer
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir project config dir: %v", err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read project config: %v", err)
	}
	line := "owner_notify_cmd = '" + hook + "'\n"
	updated := string(existing) + "\n[sync]\n" + line
	if strings.Contains(string(existing), "[sync]\n") {
		updated = strings.Replace(string(existing), "[sync]\n", "[sync]\n"+line, 1)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
}

// receiveTraceStops is the stopped metadata of every receive trace the
// workspace recorded, oldest first.
func receiveTraceStops(t *testing.T, ws workspace.Info) []string {
	t.Helper()
	type stop struct {
		at      time.Time
		stopped string
	}
	var stops []stop
	for _, record := range readSyncTraces(t, ws) {
		if record.Command != receiveTraceCommand {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, record.RecordedAt)
		if err != nil {
			t.Fatalf("receive trace %s recorded_at: %v", record.ID, err)
		}
		stops = append(stops, stop{at, record.Metadata["stopped"]})
	}
	sort.Slice(stops, func(i, j int) bool { return stops[i].at.Before(stops[j].at) })
	out := make([]string, len(stops))
	for i, s := range stops {
		out[i] = s.stopped
	}
	return out
}

// unsettlingCheckout is a checkout pushed to its remote whose settle fails
// after a fetch that works: the settle's record of itself cannot be written,
// because its path is a directory. It returns an invocation's context, the
// workspace, and a receive's fetch into a fresh clone of its live store, ready
// to land.
func unsettlingCheckout(t *testing.T) (context.Context, workspace.Info, liveClone, receiveFetch) {
	t.Helper()
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	runGit(t, base, "clone", filepath.Join(base, "remote.git"), "alpha")
	dir := filepath.Join(base, "alpha")
	runGit(t, dir, "config", "user.email", "a@a.co")
	runGit(t, dir, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "seed")
	runGit(t, dir, "push", "origin", "HEAD")
	runCLIInDir(t, dir, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, dir, "sync", "push", "--set-upstream")
	ws, err := workspace.Resolve(dir)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	ctx := telemetry.With(context.Background(), telemetry.Begin(telemetry.Event{}))
	clone, err := takeReceiveClone(ctx, ws)
	if err != nil {
		t.Fatalf("takeReceiveClone() error = %v", err)
	}
	t.Cleanup(func() { removeReceiveClone(clone) })
	fetch, err := fetchIntoClone(ctx, ws, clone)
	if err != nil || fetch.fetchErr != nil || fetch.target.skip != syncTargetReady {
		t.Fatalf("fetchIntoClone() = %+v, %v; want a ready target fetched cleanly", fetch, err)
	}
	return ctx, ws, clone, fetch
}

// TestTheMirrorsReceiveStartsNoRun: the mirror's receive of its re-rooted
// push does not hold the receive lock, so a post-fetch stop there leaves
// unsettled.first unwritten, and the next automatic receive counts it.
func TestTheMirrorsReceiveStartsNoRun(t *testing.T) {
	ctx, ws, clone, fetch := unsettlingCheckout(t)
	holdTheCommitLock(t, ws)
	err := receivePushedReRoot(ctx, ws, clone, pushedHead{remote: fetch.target.remote, branch: fetch.target.branch})
	if err == nil {
		t.Fatal("receivePushedReRoot() succeeded under a held commit lock; want the settle's stop")
	}
	if at, ok, err := store.ReadUnsettledFirst(ws.DatabasePath); ok || err != nil {
		t.Fatalf("unsettled.first after the mirror's stop = %s ok %v err %v; want no record", at, ok, err)
	}
}

// TestAReceiveWhoseOpenFailsHasNothingToClose: a receive that stops at
// opening the live store holds no store, and the closer it hands back is one
// its caller can always call.
func TestAReceiveWhoseOpenFailsHasNothingToClose(t *testing.T) {
	ctx, ws, clone, fetch := unsettlingCheckout(t)
	ws.WorkspaceID = "" // the open refuses a store with no workspace identity; the land never reads it
	outcome, closeLive := performSyncReceive(ctx, ws, clone, fetch, leaveUnsettledRun(ws.DatabasePath))
	if outcome.stopped != receiveStepOpen {
		t.Fatalf("receive stopped at %q (err %v); want the open", outcome.stopped, outcome.err)
	}
	if err := closeLive(); err != nil {
		t.Fatalf("closeLive() after a failed open = %v; want nothing to close", err)
	}
}
