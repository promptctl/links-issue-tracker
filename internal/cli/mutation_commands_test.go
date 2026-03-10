package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMutationCommandsDoNotDeadlock(t *testing.T) {
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

	// Keep this test self-contained in the temp repo.
	t.Setenv("HOME", repo)
	t.Setenv("CODEX_HOME", filepath.Join(repo, ".codex-home"))

	runWithTimeout := func(args []string, timeout time.Duration) (bytes.Buffer, error) {
		var stdout bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, &stdout, &stdout, args)
		}()
		select {
		case runErr := <-errCh:
			return stdout, runErr
		case <-ctx.Done():
			t.Fatalf("Run(%v) timed out after %s: %v", args, timeout, ctx.Err())
			return bytes.Buffer{}, nil
		}
	}

	if _, err := runWithTimeout([]string{"init", "--skip-hooks", "--skip-agents", "--json"}, 10*time.Second); err != nil {
		t.Fatalf("Run(init --skip-hooks --skip-agents --json) error = %v", err)
	}

	newOut, err := runWithTimeout([]string{"new", "--title", "deadlock regression probe", "--type", "task", "--priority", "4", "--json"}, 10*time.Second)
	if err != nil {
		t.Fatalf("Run(new --json) error = %v", err)
	}
	var issue map[string]any
	if err := json.Unmarshal(newOut.Bytes(), &issue); err != nil {
		t.Fatalf("new output should be json: %v", err)
	}
	issueID, ok := issue["id"].(string)
	if !ok || issueID == "" {
		t.Fatalf("new output missing id: %v", issue)
	}

	if _, err := runWithTimeout([]string{"comment", "add", issueID, "--body", "deadlock regression probe", "--json"}, 10*time.Second); err != nil {
		t.Fatalf("Run(comment add --json) error = %v", err)
	}
	if _, err := runWithTimeout([]string{"close", issueID, "--reason", "deadlock regression probe cleanup", "--json"}, 10*time.Second); err != nil {
		t.Fatalf("Run(close --json) error = %v", err)
	}
}
