package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunNestedInvalidPathsReturnUsageOutsideRepo(t *testing.T) {
	t.Parallel()
	nonRepo := t.TempDir()

	cases := []struct {
		args    []string
		wantErr string
	}{
		{args: []string{"comment"}, wantErr: "usage: lit comment <add|rm> ..."},
		{args: []string{"label", "bogus"}, wantErr: "usage: lit label <add|rm> ..."},
		{args: []string{"parent", "bogus"}, wantErr: "usage: lit parent <set|clear> ..."},
		{args: []string{"dep", "unknown"}, wantErr: "usage: lit dep <add|rm|ls> ..."},
		{args: []string{"sync", "unknown"}, wantErr: "usage: lit sync <status|remote|fetch|pull|push|compact|reconcile> ..."},
		{args: []string{"hooks"}, wantErr: "usage: lit hooks install"},
		{args: []string{"bulk"}, wantErr: bulkFamily.usage},
		{args: []string{"backup", "prune"}, wantErr: "usage: lit backup <create|list|restore> ..."},
		{args: []string{"snapshots", "bogus"}, wantErr: "usage: lit snapshots <new|list|restore> ..."},
		{args: []string{"lifeboat"}, wantErr: "usage: lit lifeboat <dump|recover> ..."},
		{args: []string{"completion", "powershell"}, wantErr: "usage: lit completion <bash|zsh|fish>"},
	}

	for _, tc := range cases {
		var stdout bytes.Buffer
		err := Run(context.Background(), dirInvocation(t, nonRepo, &stdout, &stdout, tc.args))
		if err == nil {
			t.Fatalf("Run(%v) error = nil, want usage error", tc.args)
		}
		if err.Error() != tc.wantErr {
			t.Fatalf("Run(%v) error = %q, want %q", tc.args, err.Error(), tc.wantErr)
		}
		if strings.Contains(err.Error(), "links requires running inside a git repository/worktree") {
			t.Fatalf("Run(%v) unexpectedly resolved workspace before usage validation: %v", tc.args, err)
		}
	}
}

func TestRunNewCompletesWithoutDeadlock(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")

	var initOut bytes.Buffer
	if err := Run(context.Background(), dirInvocation(t, repo, &initOut, &initOut, []string{"init"})); err != nil {
		t.Fatalf("Run(init) error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		var stdout bytes.Buffer
		done <- Run(context.Background(), dirInvocation(t, repo, &stdout, &stdout, []string{"new", "--title", "deadlock guard", "--topic", "deadlock", "--type", "epic"}))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run(new) error = %v", err)
		}
	case <-time.After(suiteStallDeadline):
		t.Fatal("Run(new) timed out; likely mutation lock context regression")
	}
}

func TestRunNestedHelpAfterValidSubcommandPassesThrough(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")

	var initOut bytes.Buffer
	if err := Run(context.Background(), dirInvocation(t, repo, &initOut, &initOut, []string{"init"})); err != nil {
		t.Fatalf("Run(init) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := Run(context.Background(), dirInvocation(t, repo, &stdout, &stdout, []string{"dep", "add", "--help"})); err != nil {
		t.Fatalf("Run(dep add --help) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Usage of dep add:") {
		t.Fatalf("help output = %q, want dep add help text", output)
	}
	if strings.Contains(output, "usage: lit dep <add|rm|ls> ...") {
		t.Fatalf("help output unexpectedly returned top-level dep usage: %q", output)
	}
}
