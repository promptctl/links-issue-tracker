package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunNestedInvalidPathsReturnUsageOutsideRepo(t *testing.T) {
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	nonRepo := t.TempDir()
	if err := os.Chdir(nonRepo); err != nil {
		t.Fatalf("Chdir(nonRepo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	cases := []struct {
		args    []string
		wantErr string
	}{
		{args: []string{"comment"}, wantErr: "usage: lnks comment add <id> --body <text>"},
		{args: []string{"label", "--help"}, wantErr: "usage: lnks label <add|rm> ..."},
		{args: []string{"dep", "unknown"}, wantErr: "usage: lnks dep <add|rm|ls> ..."},
		{args: []string{"sync", "unknown"}, wantErr: "usage: lnks sync <status|remote|fetch|pull|push> ..."},
		{args: []string{"hooks"}, wantErr: "usage: lnks hooks install [--json]"},
	}

	for _, tc := range cases {
		var stdout bytes.Buffer
		err := Run(context.Background(), &stdout, &stdout, tc.args)
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
	repo := t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var initOut bytes.Buffer
	if err := Run(context.Background(), &initOut, &initOut, []string{"init", "--json"}); err != nil {
		t.Fatalf("Run(init --json) error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		var stdout bytes.Buffer
		done <- Run(context.Background(), &stdout, &stdout, []string{"new", "--title", "deadlock guard", "--json"})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run(new --json) error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run(new --json) timed out; likely mutation lock context regression")
	}
}

func TestRunNestedHelpAfterValidSubcommandPassesThrough(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var initOut bytes.Buffer
	if err := Run(context.Background(), &initOut, &initOut, []string{"init", "--json"}); err != nil {
		t.Fatalf("Run(init --json) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"dep", "add", "--help"}); err != nil {
		t.Fatalf("Run(dep add --help) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Usage of dep add:") {
		t.Fatalf("help output = %q, want dep add help text", output)
	}
	if strings.Contains(output, "usage: lnks dep <add|rm|ls> ...") {
		t.Fatalf("help output unexpectedly returned top-level dep usage: %q", output)
	}
}
