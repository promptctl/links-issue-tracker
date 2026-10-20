package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAReceiveThatFetchedAndKeepsStoppingReachesTheFailureSurface drives the
// automatic receive on a checkout whose fetch succeeds and whose settle then
// fails, from a real cause: the settle's record of itself cannot be written,
// because its path is a directory. The first stop is a run of one: the event
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
	// The settle's record of itself cannot be written: its path is a directory.
	if err := os.MkdirAll(store.SettledLastPath(ws.DatabasePath), 0o755); err != nil {
		t.Fatalf("block the settle's record: %v", err)
	}
	if err := os.Remove(fetchSuccessMarkerPath(ws)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear the fetch-success marker: %v", err)
	}
	receive := func() map[string]any {
		t.Helper()
		runCLIInDir(t, consumer, "sync", backgroundReceiveSubcommand, "--parent-pid", "0")
		outcomes := lastEvent(t).Occurrences["receive.outcomes"]
		if len(outcomes) != 1 {
			t.Fatalf("receive.outcomes = %v; want the one receive's outcome", outcomes)
		}
		outcome, _ := outcomes[0].(map[string]any)
		return outcome
	}

	// The first stop starts a run and surfaces nothing but its record.
	first := receive()
	if first["receive.stopped"] != "settle" || first["receive.decision"] != "error" || !(first["receive.unsettled_since"].(float64) > 0) {
		t.Fatalf("first receive outcome = %v; want a settle stop that starts a run", first)
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
	second := receive()
	if second["receive.stopped"] != "settle" || second["receive.unsettled_since"] != float64(began.Unix()) {
		t.Fatalf("second receive outcome = %v; want a settle stop in the run that began at %d", second, began.Unix())
	}
	block, err := os.ReadFile(receiveBlockPendingPath(ws))
	if err != nil {
		t.Fatalf("a lasting run of post-fetch stops left no block for the next command: %v", err)
	}
	for _, want := range []string{
		"lit's automatic receive fetched the remote but could not bring it into this checkout",
		"stopped at settling onto the fetched head",
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
	if err := os.Remove(store.SettledLastPath(ws.DatabasePath)); err != nil {
		t.Fatalf("unblock the settle's record: %v", err)
	}
	if err := writeMarkerAtomic(receiveBlockPendingPath(ws), block); err != nil {
		t.Fatalf("re-leave the block: %v", err)
	}
	third := receive()
	if third["receive.stopped"] != "" || third["receive.unsettled_since"] != float64(0) || !(third["receive.settled_last"].(float64) > 0) {
		t.Fatalf("third receive outcome = %v; want a receive that ran to the end, settled, with no run current", third)
	}
	if _, err := os.Stat(receiveBlockPendingPath(ws)); !os.IsNotExist(err) {
		t.Fatalf("a receive that settled left the receive_unsettled block pending (stat err = %v)", err)
	}
	if pages, _ := os.ReadFile(sink); string(pages) != "receive_unsettled\n" {
		t.Fatalf("owner hook output after the settle = %q; want still the one page", pages)
	}
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
