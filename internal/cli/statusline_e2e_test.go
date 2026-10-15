package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	isolateEvents(t)
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "m@m.co")
	runGit(t, dir, "config", "user.name", "main")
	runGit(t, dir, "commit", "--allow-empty", "-m", "seed")
	runCLIInDir(t, dir, "init", "--skip-hooks", "--skip-agents")
	epic := newEpicInDir(t, dir, "statusline epic")
	first := extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "First", "--topic", "demo", "--type", "task", "--parent", epic))
	second := extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "Second", "--topic", "demo", "--type", "task", "--parent", epic))
	file := filepath.Join(dir, ".lit", "statusline.json")

	type step struct {
		args                  []string
		changed               string
		created, excludeAdded bool
	}
	check := func(s step) {
		t.Helper()
		runCLIInDir(t, dir, s.args...)
		facts, bytes := statuslineEvent(t)
		want := decodeJSON(t, fmt.Sprintf(`{"statusline.changed": %s, "statusline.created": %t, "statusline.exclude_added": %t, "statusline.replaced": null, "statusline.error": null}`, s.changed, s.created, s.excludeAdded))
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
		{[]string{"show", first}, `[]`, false, false},
		{[]string{"next"}, `["next"]`, true, true},
		{[]string{"start", first, "--by", "agent"}, `["working_on"]`, false, false},
		{[]string{"update", first, "--title", "First, renamed"}, `["working_on", "next"]`, false, false},
		{[]string{"done", first}, `["working_on", "next"]`, false, false},
		{[]string{"start", second, "--by", "agent"}, `["working_on"]`, false, false},
		{[]string{"purge", second, "--yes"}, `["working_on"]`, false, false},
	} {
		check(s)
	}

	// Recreating the file finds the exclude entry already there. A retitle of
	// a ticket the new file does not name changes no slot of it.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	check(step{[]string{"update", first, "--title", "First, again"}, `[]`, true, false})

	// A file this lit cannot read is replaced, and the event says why.
	if err := os.WriteFile(file, []byte("{working_on"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLIInDirAllowError(t, dir, "next")
	facts, _ := statuslineEvent(t)
	if replaced, ok := facts["statusline.replaced"].(string); !ok || !strings.Contains(replaced, file) {
		t.Fatalf("next over an unreadable file: statusline.replaced = %v; want the reason, naming %s", facts["statusline.replaced"], file)
	}
	if got := facts["statusline.changed"]; !reflect.DeepEqual(got, []any{"next"}) {
		t.Fatalf("next over an unreadable file: statusline.changed = %v; want [next]", got)
	}
}

// A statusline write that fails is on the event with its error, beside what
// the write had done before it failed; the command itself does not fail.
func TestAFailedStatuslineWriteIsRecordedOnTheEvent(t *testing.T) {
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
	recordStatusline(ctx, ws, statusline.Facts{statusline.Started{Ticket: statusline.Ticket{ID: "t-1", Title: "One"}}})
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
