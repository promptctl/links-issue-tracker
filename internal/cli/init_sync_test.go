package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestInitFailsLoudlyWhenRemoteHasDataButAdoptCannotComplete is the regression
// guard for the silent-empty data-loss bug: when the remote advertises lit
// ticket data (refs/dolt/*) but the backlog does not adopt, init must NOT leave
// a silent empty store — it must surface the failure loudly so the operator
// knows the store is empty and must not be pushed. The failure is forced
// deterministically by pointing the consumer's sync branch at a name the remote
// does not carry, which is exactly the shape that produced the silent
// no_remote_data outcome before the fix.
func TestInitFailsLoudlyWhenRemoteHasDataButAdoptCannotComplete(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	// Producer publishes a real backlog to the remote (refs/dolt/* populated).
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
	// A successful sync push lands the backlog under refs/dolt/* on the remote.
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")

	// Force the adopt to resolve a branch the remote's Dolt data is not on, so
	// the post-fetch freshness check reports not-synced even though the data is
	// there — the precise condition that used to fall through to silent empty.
	t.Setenv("LINKS_DEBUG_DOLT_SYNC_BRANCH", "branch-the-remote-does-not-have")
	initOut := runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")

	if !strings.Contains(initOut, "could not pull existing backlog") {
		t.Fatalf("init must surface the adopt failure loudly, got:\n%s", initOut)
	}
	if !strings.Contains(initOut, "refs/dolt/") {
		t.Fatalf("loud failure must name the remote data it could not adopt, got:\n%s", initOut)
	}
	// And it must NOT have silently claimed an adopt.
	if strings.Contains(initOut, "Pulled existing backlog") {
		t.Fatalf("init reported an adopt that did not happen:\n%s", initOut)
	}
}

// TestInitAdoptHardStopsOnTimeout is the regression guard for the lockup: dolt's
// fetch ignores context cancellation, so the adopt must be hard-stopped on a
// deadline. The blocking body is stubbed with one that only returns when
// abandoned (mirroring dolt), proving the wrapper returns a loud failure on the
// deadline rather than blocking on it.
func TestInitAdoptHardStopsOnTimeout(t *testing.T) {
	prevFn := adoptRemoteTicketsBlockingFn
	prevTimeout := adoptRemoteTimeout
	defer func() {
		adoptRemoteTicketsBlockingFn = prevFn
		adoptRemoteTimeout = prevTimeout
	}()

	started := make(chan struct{})
	adoptRemoteTicketsBlockingFn = func(ctx context.Context, ws workspace.Info) initSyncOutcome {
		close(started)
		<-ctx.Done() // a fetch that never finishes until it is abandoned
		return initSyncOutcome{State: initSyncAdopted, Remote: "origin", Branch: "master"}
	}
	adoptRemoteTimeout = 20 * time.Millisecond

	outcome := adoptRemoteTicketsOnInit(context.Background(), workspace.Info{})

	select {
	case <-started:
	default:
		t.Fatal("adopt body was never started")
	}
	if outcome.State != initSyncFailed {
		t.Fatalf("outcome.State = %q, want %q (hard-stop must fail loudly)", outcome.State, initSyncFailed)
	}
	if !strings.Contains(outcome.Error, "exceeded") {
		t.Fatalf("timeout failure must name the lockout, got: %q", outcome.Error)
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
