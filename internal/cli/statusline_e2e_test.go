package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/statusline"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// readStatusline reads the checkout's statusline file the way a statusline
// script would: straight off disk, with no lit process.
func readStatusline(t *testing.T, checkout string) statusline.File {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(checkout, ".lit", "statusline.json"))
	if err != nil {
		t.Fatalf("read statusline file in %s: %v", checkout, err)
	}
	var f statusline.File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("statusline file in %s is not JSON: %v\n%s", checkout, err, raw)
	}
	return f
}

func workingOnID(f statusline.File) string {
	if f.WorkingOn == nil {
		return ""
	}
	return f.WorkingOn.Ticket.ID
}

// nextID is the pick's id, "" for a recorded pick of nothing, and "<none>"
// when no pick is recorded at all.
func nextID(f statusline.File) string {
	if f.Next == nil {
		return "<none>"
	}
	if f.Next.Ticket == nil {
		return ""
	}
	return f.Next.Ticket.ID
}

func gitStatus(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all", "--ignored=no")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status in %s: %v\n%s", dir, err, out)
	}
	return string(out)
}

// Each checkout's file follows that checkout's own commands — start, next,
// done — and never shows in git status, in the main checkout or a worktree,
// while a committed file beside it under .lit/ still does.
func TestStatuslineFileFollowsEachCheckoutsOwnWork(t *testing.T) {
	// --by is the identity source only when no agent session id is inherited.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	main := t.TempDir()
	runGit(t, main, "init")
	runGit(t, main, "config", "user.email", "m@m.co")
	runGit(t, main, "config", "user.name", "main")
	if err := os.MkdirAll(filepath.Join(main, ".lit"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(main, ".lit", "config.toml")
	if err := os.WriteFile(configPath, []byte("# committed project config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, main, "add", "-A")
	runGit(t, main, "commit", "-m", "seed")
	runCLIInDir(t, main, "init", "--skip-hooks", "--skip-agents")
	tree := filepath.Join(t.TempDir(), "tree")
	runGit(t, main, "worktree", "add", "-b", "tree", tree)

	epic := newEpicInDir(t, main, "statusline epic")
	first := extractTicketID(t, runCLIInDir(t, main, "new", "--title", "First", "--topic", "demo", "--type", "task", "--parent", epic))

	runCLIInDir(t, main, "next")
	if got := readStatusline(t, main); nextID(got) != first || workingOnID(got) != "" {
		t.Fatalf("after next: file = %+v; want next %s and nothing in flight", got, first)
	}

	runCLIInDir(t, main, "start", first, "--by", "main-agent")
	got := readStatusline(t, main)
	if workingOnID(got) != first || got.WorkingOn.Ticket.Title != "First" {
		t.Fatalf("after start: working_on = %+v; want %s \"First\"", got.WorkingOn, first)
	}
	if _, err := os.Stat(filepath.Join(tree, ".lit", "statusline.json")); !os.IsNotExist(err) {
		t.Fatalf("a start in the main checkout wrote the worktree's file (stat err = %v)", err)
	}

	runCLIInDir(t, main, "update", first, "--title", "First, renamed")
	if got := readStatusline(t, main); workingOnID(got) != first || got.WorkingOn.Ticket.Title != "First, renamed" || got.Next.Ticket.Title != "First, renamed" {
		t.Fatalf("after a retitle: file = %+v / %+v; want both records of %s titled \"First, renamed\"", got.WorkingOn, got.Next, first)
	}

	// A narrowed next answers a different question than "what's next", so it
	// leaves the pick alone; the worktree's next writes the worktree's file.
	// Both find nothing, which `lit next` reports as an error exit.
	runCLIInDirAllowError(t, main, "next", "--type", "bug")
	runCLIInDirAllowError(t, tree, "next")
	if got := readStatusline(t, main); nextID(got) != first || workingOnID(got) != first {
		t.Fatalf("after a narrowed next and the worktree's next: main file = %+v; want next and working_on still %s", got, first)
	}
	if got := readStatusline(t, tree); workingOnID(got) != "" || nextID(got) == "<none>" {
		t.Fatalf("worktree file = %+v; want a recorded pick and nothing in flight", got)
	}

	runCLIInDir(t, main, "done", first)
	if got := readStatusline(t, main); got.WorkingOn != nil || got.Next != nil {
		t.Fatalf("after done: file = %+v; want both records of %s cleared", got, first)
	}

	runCLIInDirAllowError(t, main, "next")
	if got := readStatusline(t, main); nextID(got) != "" {
		t.Fatalf("after next on an empty backlog: next = %q; want a recorded pick of nothing", nextID(got))
	}

	for _, checkout := range []string{main, tree} {
		if status := gitStatus(t, checkout); strings.Contains(status, "statusline") {
			t.Fatalf("git status in %s shows the statusline file:\n%s", checkout, status)
		}
	}
	if err := os.WriteFile(configPath, []byte("# edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status := gitStatus(t, main); !strings.Contains(status, ".lit/config.toml") {
		t.Fatalf("git status no longer shows an edit to the committed .lit/config.toml:\n%s", status)
	}
}

// statuslineEvent is what the last invocation's event says its statusline
// write did: the statusline.* facts and the byte count.
func statuslineEvent(t *testing.T) (map[string]any, int) {
	t.Helper()
	ev := lastEvent(t)
	facts := map[string]any{}
	for key, value := range writtenFacts(t, ev) {
		if strings.HasPrefix(key, "statusline.") {
			facts[key] = value
		}
	}
	bytes, ok := ev.Counts["statusline.bytes"]
	if !ok {
		t.Fatalf("%s event has no statusline.bytes count: %v", ev.Event, ev.Counts)
	}
	return facts, bytes
}

// Every command that writes the statusline file puts what the write did on its
// own event — the slots it changed, the bytes it wrote, whether it created the
// file and its exclude entry, and whether it replaced an unreadable file — and
// a command that wrote nothing says so with zeros.
func TestEachStatuslineWriteRecordsItsFactsOnTheEvent(t *testing.T) {
	isolateEvents(t)
	dir, first, second := statuslineCheckout(t)
	file := filepath.Join(dir, ".lit", "statusline.json")

	type step struct {
		args                   []string
		changed, rollup, phase string
		created, excludeAdded  bool
	}
	check := func(s step) {
		t.Helper()
		runCLIInDir(t, dir, s.args...)
		facts, bytes := statuslineEvent(t)
		want := decodeJSON(t, fmt.Sprintf(`{"statusline.changed": %s, "statusline.rollup_progress": %s, "statusline.phase": %s, "statusline.created": %t, "statusline.exclude_added": %t, "statusline.replaced": null, "statusline.error": null}`, s.changed, s.rollup, s.phase, s.created, s.excludeAdded))
		if !reflect.DeepEqual(facts, want) {
			t.Fatalf("lit %v event facts = %v\nwant %v", s.args, facts, want)
		}
		// The file's size on disk, or zero before any command has written it.
		wantBytes := 0
		if info, err := os.Stat(file); err == nil {
			wantBytes = int(info.Size())
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes != wantBytes {
			t.Fatalf("lit %v event statusline.bytes = %d, want %d (the file's size)", s.args, bytes, wantBytes)
		}
	}
	for _, s := range []step{
		{[]string{"show", first}, `[]`, `null`, `null`, false, false},
		{[]string{"next"}, `["next"]`, `null`, `null`, true, true},
		{[]string{"start", first, "--by", "agent"}, `["working_on"]`, `{"rollup_completed_count": 0, "rollup_total_count": 2}`, `"plan"`, false, false},
		{[]string{"update", first, "--title", "First, renamed"}, `["working_on", "next"]`, `{"rollup_completed_count": 0, "rollup_total_count": 2}`, `"plan"`, false, false},
		{[]string{"phase", first, "--to", "impl"}, `["working_on"]`, `{"rollup_completed_count": 0, "rollup_total_count": 2}`, `"impl"`, false, false},
	} {
		check(s)
	}
	for _, s := range []step{
		{[]string{"done", first}, `["working_on", "next"]`, `null`, `null`, false, false},
		{[]string{"start", second, "--by", "agent"}, `["working_on"]`, `{"rollup_completed_count": 1, "rollup_total_count": 2}`, `"plan"`, false, false},
		{[]string{"purge", second, "--yes"}, `["working_on"]`, `null`, `null`, false, false},
	} {
		check(s)
	}

	// Recreating the file finds the exclude entry already there. A retitle of
	// a ticket the new file does not name changes no slot of it.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	check(step{[]string{"update", first, "--title", "First, again"}, `[]`, `null`, `null`, true, false})

	// A file this lit cannot read is replaced, and the event says why.
	if err := os.WriteFile(file, []byte("{working_on"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLIInDirAllowError(t, dir, "next")
	facts, _ := statuslineEvent(t)
	if replaced, ok := facts["statusline.replaced"].(string); !ok || !strings.Contains(replaced, file) {
		t.Fatalf("next over an unreadable file: statusline.replaced = %v; want the reason, naming %s", facts["statusline.replaced"], file)
	}
	if got := facts["statusline.changed"]; !reflect.DeepEqual(got, []any{"working_on", "next"}) {
		t.Fatalf("next over an unreadable file: statusline.changed = %v; want [working_on next]", got)
	}
}

// A retitle reaches the file by whichever command makes it, lit import as
// surely as lit update — and a retitle an import makes before its batch fails
// stays made, so the file shows it too.
func TestAnImportRetitleReachesTheStatuslineFile(t *testing.T) {
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "next")
	runCLIInDir(t, dir, "start", first, "--by", "agent")
	importFile := func(body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "batch.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	titles := func(want string) {
		t.Helper()
		got := readStatusline(t, dir)
		if workingOnID(got) != first || got.WorkingOn.Ticket.Title != want || nextID(got) != first || got.Next.Ticket.Title != want {
			t.Fatalf("file = %+v / %+v; want both records of %s titled %q", got.WorkingOn, got.Next, first, want)
		}
	}

	runCLIInDir(t, dir, "import", "--path", importFile(fmt.Sprintf("id: %s\ntitle: Renamed\n", first)))
	titles("Renamed")
	if facts, _ := statuslineEvent(t); !reflect.DeepEqual(facts["statusline.changed"], []any{"working_on", "next"}) {
		t.Fatalf("import retitle: statusline.changed = %v; want [working_on next]", facts["statusline.changed"])
	}

	if _, err := runCLIInDirAllowError(t, dir, "import", "--path", importFile(fmt.Sprintf("id: %s\ntitle: Renamed again\n---\nid: %s-nope\ntitle: Never\n", first, first))); err == nil {
		t.Fatalf("an import naming %s-nope succeeded; the test needs its batch to fail", first)
	}
	if title := strings.TrimSpace(runCLIInDir(t, dir, "show", first, "--field", "title")); title != "Renamed again" {
		t.Fatalf("the failed batch left %s titled %q; the test needs its first update to land", first, title)
	}
	titles("Renamed again")
}

// A backup restore replaces the store the file's records describe, so it
// clears both of them and keeps the schema version.
func TestABackupRestoreClearsTheStatuslineFile(t *testing.T) {
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "next")
	runCLIInDir(t, dir, "start", first, "--by", "agent")
	runCLIInDir(t, dir, "backup", "create")
	if got := readStatusline(t, dir); workingOnID(got) != first || nextID(got) != first {
		t.Fatalf("before the restore the file = %+v / %+v; the test needs both records of %s", got.WorkingOn, got.Next, first)
	}

	runCLIInDir(t, dir, "backup", "restore", "--latest")
	if got := readStatusline(t, dir); got.WorkingOn != nil || got.Next != nil || got.SchemaVersion != statusline.SchemaVersion {
		t.Fatalf("after a restore the file = %+v; want schema_version %d and both records null", got, statusline.SchemaVersion)
	}
	if facts, _ := statuslineEvent(t); !reflect.DeepEqual(facts["statusline.changed"], []any{"working_on", "next"}) {
		t.Fatalf("restore: statusline.changed = %v; want [working_on next]", facts["statusline.changed"])
	}
}

// After each start, and after a lit next hands the work back, the file's
// rollup progress is the "N/M done" the lit backlog claim line prints for that lane at
// that moment — including a start that takes a closed ticket out of the done
// count.
func TestTheStatuslineRollupProgressMatchesTheClaimLine(t *testing.T) {
	isolateEvents(t)
	dir, first, second := statuslineCheckout(t)
	claimCount := regexp.MustCompile(`(?m)^\s*claimed here: .* (\d+)/(\d+) done$`)
	matches := func(after string, want statusline.RollupProgress) {
		t.Helper()
		got := readStatusline(t, dir)
		if got.WorkingOn == nil {
			t.Fatalf("after %s: working_on is null; want the ticket started", after)
		}
		backlog := runCLIInDir(t, dir, "backlog")
		line := claimCount.FindAllStringSubmatch(backlog, -1)
		if len(line) != 1 {
			t.Fatalf("after %s: lit backlog printed %d claim lines with a count; want 1:\n%s", after, len(line), backlog)
		}
		printed := statusline.RollupProgress{CompletedCount: mustAtoi(t, line[0][1]), TotalCount: mustAtoi(t, line[0][2])}
		if got.WorkingOn.RollupProgress != printed || printed != want {
			t.Fatalf("after %s: file rollup_progress = %+v, claim line %+v; want both %+v", after, got.WorkingOn.RollupProgress, printed, want)
		}
	}

	runCLIInDir(t, dir, "start", first, "--by", "agent")
	matches("starting the first ticket", statusline.RollupProgress{CompletedCount: 0, TotalCount: 2})
	runCLIInDir(t, dir, "start", second, "--by", "agent")
	runCLIInDir(t, dir, "done", second)
	runCLIInDir(t, dir, "next")
	matches("lit next handing the first ticket back", statusline.RollupProgress{CompletedCount: 1, TotalCount: 2})
	runCLIInDir(t, dir, "start", second, "--by", "agent")
	matches("starting the closed ticket", statusline.RollupProgress{CompletedCount: 0, TotalCount: 2})
}

// After each command that records the work, the file's phase is the phase lit
// show prints for the ticket it names — through a start, a phase move, a move
// of a ticket the file does not name, lit next handing work back, and a
// reclaim that keeps the phase the work had reached.
func TestTheStatuslinePhaseMatchesTheTicketsPhase(t *testing.T) {
	isolateEvents(t)
	dir, first, second := statuslineCheckout(t)
	matches := func(after, id, want string) {
		t.Helper()
		got := readStatusline(t, dir)
		if workingOnID(got) != id || got.WorkingOn.Phase == nil {
			t.Fatalf("after %s: working_on = %+v; want %s with a phase", after, got.WorkingOn, id)
		}
		shown := strings.TrimSpace(runCLIInDir(t, dir, "show", id, "--field", "phase"))
		if *got.WorkingOn.Phase != shown || shown != want {
			t.Fatalf("after %s: file phase = %q, lit show %q; want both %q", after, *got.WorkingOn.Phase, shown, want)
		}
	}

	runCLIInDir(t, dir, "start", first, "--by", "agent")
	matches("starting the first ticket", first, "plan")
	runCLIInDir(t, dir, "phase", first, "--to", "impl")
	matches("moving it to impl", first, "impl")
	runCLIInDir(t, dir, "start", second, "--by", "agent")
	matches("starting the second ticket", second, "plan")
	runCLIInDir(t, dir, "phase", first, "--to", "review")
	matches("moving the first ticket, which the file does not name", second, "plan")
	runCLIInDir(t, dir, "done", second)
	runCLIInDir(t, dir, "next")
	matches("lit next handing the first ticket back", first, "review")
	runCLIInDir(t, dir, "start", first, "--by", "agent")
	matches("reclaiming the first ticket", first, "review")
}

// A checkout that takes over a ticket another checkout moved to review records
// review, the phase lit show prints there: the takeover keeps the phase the
// work had reached rather than starting it over in plan.
func TestATakeoverRecordsThePhaseTheWorkHadReached(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	alpha := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, alpha, "config", "user.email", "a@a.co")
	runGit(t, alpha, "config", "user.name", "alpha")
	runGit(t, alpha, "commit", "--allow-empty", "-m", "seed")
	runGit(t, alpha, "push", "origin", "HEAD")
	runCLIInDir(t, alpha, "init", "--skip-hooks", "--skip-agents")
	epic := newEpicInDir(t, alpha, "statusline epic")
	ticket := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "First", "--topic", "demo", "--type", "task", "--parent", epic))
	runCLIInDir(t, alpha, "start", ticket, "--by", "alpha-agent")
	runCLIInDir(t, alpha, "phase", ticket, "--to", "review")
	runCLIInDir(t, alpha, "sync", "push", "--set-upstream")

	bravo := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, bravo, "config", "user.email", "b@b.co")
	runGit(t, bravo, "config", "user.name", "bravo")
	runCLIInDir(t, bravo, "init", "--skip-hooks", "--skip-agents")
	if out := runCLIInDir(t, bravo, "start", ticket, "--by", "bravo-agent", "--take"); !strings.Contains(out, "claim transferred") {
		t.Fatalf("start %s --take = %q, want the transfer announced", ticket, out)
	}

	got := readStatusline(t, bravo)
	shown := strings.TrimSpace(runCLIInDir(t, bravo, "show", ticket, "--field", "phase"))
	if workingOnID(got) != ticket || got.WorkingOn.Phase == nil || *got.WorkingOn.Phase != "review" || shown != "review" {
		t.Fatalf("after the takeover: working_on = %+v, lit show phase %q; want %s in review in both", got.WorkingOn, shown, ticket)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A statusline write that fails is on the event with its error, beside what
// the write had done before it failed; the command itself does not fail.
func TestAFailedStatuslineWriteIsRecordedOnTheEvent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root from writing")
	}
	root := t.TempDir()
	litDir := filepath.Join(root, ".lit")
	if err := os.MkdirAll(litDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(litDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(litDir, 0o755) })
	rec := telemetry.Begin(telemetry.Event{Event: "lit start"})
	ctx := telemetry.With(context.Background(), rec)
	ws := workspace.Info{RootDir: root, Location: workspace.Location{GitCommonDir: filepath.Join(root, "git")}}
	recordStatusline(ctx, ws, statusline.Facts{statusline.NewStarted(statusline.Ticket{ID: "t-1", Title: "One"}, statusline.RollupProgress{CompletedCount: 0, TotalCount: 1})})
	ev := rec.Finish(telemetry.End{At: time.Now(), Outcome: telemetry.OutcomeOK})
	facts := writtenFacts(t, ev)
	if text, ok := facts["statusline.error"].(string); !ok || !strings.Contains(text, "statusline") {
		t.Fatalf("statusline.error = %v; want the write's error", facts["statusline.error"])
	}
	if facts["statusline.created"] != true || facts["statusline.exclude_added"] != true || ev.Counts["statusline.bytes"] != 0 {
		t.Fatalf("failed write recorded created=%v exclude_added=%v bytes=%d; want true, true, 0", facts["statusline.created"], facts["statusline.exclude_added"], ev.Counts["statusline.bytes"])
	}
	if got := facts["statusline.changed"]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("failed write recorded statusline.changed = %v; want []", got)
	}
}

// statuslineCheckout is a fresh git checkout with lit initialised and two
// tasks, First and Second, under one epic, in a HOME and XDG homes of its own
// so neither lit nor the tools a statusline runs read this machine's config.
func statuslineCheckout(t *testing.T) (dir, first, second string) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir = t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "m@m.co")
	runGit(t, dir, "config", "user.name", "main")
	runGit(t, dir, "commit", "--allow-empty", "-m", "seed")
	runCLIInDir(t, dir, "init", "--skip-hooks", "--skip-agents")
	epic := newEpicInDir(t, dir, "statusline epic")
	first = extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "First", "--topic", "demo", "--type", "task", "--parent", epic))
	second = extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "Second", "--topic", "demo", "--type", "task", "--parent", epic))
	return dir, first, second
}

// minimalStatuslineScript is the shell script docs/statusline.md gives under
// "A minimal statusline". The test runs the doc's own text, so the example a
// reader copies is the one proven here. [LAW:one-source-of-truth]
func minimalStatuslineScript(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "statusline.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "\n## A minimal statusline\n")
	if !ok {
		t.Fatal(`docs/statusline.md has no "## A minimal statusline" section`)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	_, block, ok := strings.Cut(section, "```sh\n")
	if !ok {
		t.Fatal(`docs/statusline.md's "A minimal statusline" section has no sh code block`)
	}
	script, _, ok := strings.Cut(block, "```")
	if !ok {
		t.Fatal(`docs/statusline.md's "A minimal statusline" sh block is not closed`)
	}
	return script
}

// The documented statusline renders what this checkout's commands recorded —
// the ticket being worked on, the next pick, nothing ready, and nothing at all
// when the file is missing, records nothing, or has a schema version it does
// not know — with no lit process: a lit first on its PATH fails any render
// that runs it, so no lit lock can hold the statusline up.
func TestDocumentedStatuslineRendersTheFileWithNoLitProcess(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("the documented statusline is a jq program, and jq is not on PATH: %v", err)
	}
	script := filepath.Join(t.TempDir(), "statusline.sh")
	if err := os.WriteFile(script, []byte(minimalStatuslineScript(t)), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim leaves a mark however the script redirects or ignores it.
	shim := t.TempDir()
	ranLit := filepath.Join(shim, "ran")
	if err := os.WriteFile(filepath.Join(shim, "lit"), []byte("#!/bin/sh\ntouch \""+ranLit+"\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, first, second := statuslineCheckout(t)
	file := filepath.Join(dir, ".lit", "statusline.json")

	render := func() string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		start := time.Now()
		if err := cmd.Run(); err != nil || stderr.Len() > 0 {
			t.Fatalf("documented statusline: err=%v stderr=%q", err, stderr.String())
		}
		if _, err := os.Stat(ranLit); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the documented statusline ran lit (%v)", err)
		}
		t.Logf("rendered %q in %v", stdout.String(), time.Since(start))
		return stdout.String()
	}

	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("statusline file before any command wrote it: %v; want it absent", err)
	}
	for _, step := range []struct {
		name  string
		setup func()
		want  string
	}{
		{"no file yet", func() {}, ""},
		{"after start", func() { runCLIInDir(t, dir, "start", first, "--by", "agent") }, "working: " + first + " First (0/2 done) [plan]\n"},
		{"after a phase move", func() { runCLIInDir(t, dir, "phase", first, "--to", "impl") }, "working: " + first + " First (0/2 done) [impl]\n"},
		{"after done", func() { runCLIInDir(t, dir, "done", first) }, ""},
		{"after next", func() { runCLIInDir(t, dir, "next") }, "next: " + second + "\n"},
		{"after start of the pick", func() { runCLIInDir(t, dir, "start", second, "--by", "agent") }, "working: " + second + " Second (1/2 done) [plan]  |  next: " + second + "\n"},
		{"after done of the pick", func() { runCLIInDir(t, dir, "done", second) }, ""},
		{"after next on an empty backlog", func() { runCLIInDirAllowError(t, dir, "next") }, "next: nothing ready\n"},
		{"a record with no rollup progress or phase, as an older lit writes", func() {
			if err := os.WriteFile(file, []byte(`{"schema_version": 1, "working_on": {"ticket": {"id": "x-1", "title": "X"}, "recorded_at": "2026-10-15T09:30:00Z"}, "next": null}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "working: x-1 X\n"},
		{"an unknown schema_version", func() {
			if err := os.WriteFile(file, []byte(`{"schema_version": 2, "working_on": {"ticket": {"id": "x-1", "title": "X"}}, "next": null}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}, ""},
	} {
		step.setup()
		if got := render(); got != step.want {
			t.Fatalf("%s: the documented statusline printed %q, want %q", step.name, got, step.want)
		}
	}
}
