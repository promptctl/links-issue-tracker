package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func TestWriteInitSyncLine(t *testing.T) {
	cases := []struct {
		name    string
		outcome initSyncOutcome
		want    string
	}{
		{
			name:    "adopted names remote and branch",
			outcome: initSyncOutcome{State: initSyncAdopted, Remote: "origin", Branch: "master"},
			want:    "  Pulled existing backlog from origin/master\n",
		},
		{
			name:    "failed surfaces the error loudly",
			outcome: initSyncOutcome{State: initSyncFailed, Remote: "origin", Error: "boom"},
			want:    "  Warning: could not pull existing backlog from origin: boom\n",
		},
		{
			name:    "failed without a resolved remote still surfaces",
			outcome: initSyncOutcome{State: initSyncFailed, Error: "boom"},
			want:    "  Warning: could not pull existing backlog: boom\n",
		},
		{
			name:    "has local tickets is silent",
			outcome: initSyncOutcome{State: initSyncHasLocalTickets},
			want:    "",
		},
		{
			name:    "not configured is silent",
			outcome: initSyncOutcome{State: initSyncNotConfigured},
			want:    "",
		},
		{
			name:    "no remote data is silent",
			outcome: initSyncOutcome{State: initSyncNoRemoteData, Remote: "origin", Branch: "master"},
			want:    "",
		},
		{
			name:    "remote empty is silent",
			outcome: initSyncOutcome{State: initSyncRemoteEmpty, Remote: "origin"},
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeInitSyncLine(&out, tc.outcome); err != nil {
				t.Fatalf("writeInitSyncLine() error = %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("writeInitSyncLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAdoptRemoteTicketsOnInitGates exercises the cheap, remote-free branches of
// the adopt decision: a store with local tickets must be preserved, and a
// workspace with no git remote has nothing to adopt.
func TestAdoptRemoteTicketsOnInitGates(t *testing.T) {
	t.Run("preserves a store that already holds local tickets", func(t *testing.T) {
		repo := newInitTestRepo(t)
		runCLIInDir(t, repo, "new", "--title", "local-work", "--topic", "demo", "--type", "task")
		ws, err := workspace.Resolve(repo)
		if err != nil {
			t.Fatalf("workspace.Resolve() error = %v", err)
		}
		outcome := adoptRemoteTicketsOnInit(context.Background(), ws)
		if outcome.State != initSyncHasLocalTickets {
			t.Fatalf("outcome.State = %q, want %q", outcome.State, initSyncHasLocalTickets)
		}
	})

	t.Run("reports not_configured when no git remote exists", func(t *testing.T) {
		repo := newInitTestRepo(t)
		ws, err := workspace.Resolve(repo)
		if err != nil {
			t.Fatalf("workspace.Resolve() error = %v", err)
		}
		outcome := adoptRemoteTicketsOnInit(context.Background(), ws)
		if outcome.State != initSyncNotConfigured {
			t.Fatalf("outcome.State = %q, want %q", outcome.State, initSyncNotConfigured)
		}
	})
}

// TestInitAdoptsExistingRemoteBacklog is the end-to-end proof of the ticket: a
// producer pushes ticket data to a shared remote, and a fresh clone running
// `lit init` transparently picks up that backlog — no extra commands. It drives
// the real CLI for both sides over a real git remote, so "adopted" means the
// consumer's backlog actually lists the producer's ticket.
func TestInitAdoptsExistingRemoteBacklog(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	// Producer: a real clone that publishes a ticket to the remote.
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
	runCLIInDir(t, producer, "new", "--title", "remote-ticket", "--topic", "demo", "--type", "task")
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	// Consumer: a fresh clone whose init must adopt the producer's backlog.
	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	initOut := runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	if !strings.Contains(initOut, "Pulled existing backlog from origin/master") {
		t.Fatalf("consumer init output missing adopt line:\n%s", initOut)
	}
	backlog := runCLIInDir(t, consumer, "backlog")
	if !strings.Contains(backlog, "remote-ticket") {
		t.Fatalf("consumer backlog missing adopted ticket:\n%s", backlog)
	}
}

// newInitTestRepo creates a git repo and runs `lit init` in it, returning the
// repo root. Unlike the producer/consumer integration repos, it configures no
// remote.
func newInitTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runCLIInDir(t, repo, "init", "--skip-hooks", "--skip-agents")
	return repo
}

// runCLIInDir runs the real CLI with cwd set to dir and returns its combined
// output. The chdir is restored on cleanup; these tests must not run in parallel.
func runCLIInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%q) error = %v", dir, err)
	}
	defer func() { _ = os.Chdir(prevWD) }()

	var out bytes.Buffer
	if err := Run(context.Background(), &out, &out, args); err != nil {
		t.Fatalf("Run(%v) error = %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}
