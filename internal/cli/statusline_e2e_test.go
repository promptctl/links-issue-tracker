package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/statusline"
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
