package cli

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestMutationCommandsDoNotDeadlock(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")

	// Keep this test self-contained in the temp repo.
	setRunEnv(t, "HOME", repo)

	runWithTimeout := func(args []string, timeout time.Duration) (bytes.Buffer, error) {
		var stdout bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, dirInvocation(t, repo, &stdout, &stdout, args))
		}()
		select {
		case runErr := <-errCh:
			return stdout, runErr
		case <-ctx.Done():
			t.Fatalf("Run(%v) timed out after %s: %v", args, timeout, ctx.Err())
			return bytes.Buffer{}, nil
		}
	}

	if _, err := runWithTimeout([]string{"init", "--skip-hooks"}, 10*time.Second); err != nil {
		t.Fatalf("Run(init --skip-hooks) error = %v", err)
	}

	epicOut, err := runWithTimeout([]string{"new", "--type", "epic", "--title", "deadlock regression epic", "--topic", "deadlock"}, 10*time.Second)
	if err != nil {
		t.Fatalf("Run(new --type epic) error = %v", err)
	}
	epicID := extractTicketID(t, epicOut.String())

	newOut, err := runWithTimeout([]string{"new", "--title", "deadlock regression probe", "--topic", "deadlock", "--type", "task", "--priority", "1", "--parent", epicID}, 10*time.Second)
	if err != nil {
		t.Fatalf("Run(new) error = %v", err)
	}
	issueID := extractTicketID(t, newOut.String())

	if _, err := runWithTimeout([]string{"comment", "add", issueID, "--body", "deadlock regression probe"}, 10*time.Second); err != nil {
		t.Fatalf("Run(comment add) error = %v", err)
	}
	if _, err := runWithTimeout([]string{"start", issueID, "--by", "tester"}, 10*time.Second); err != nil {
		t.Fatalf("Run(start --by tester) error = %v", err)
	}
	if _, err := runWithTimeout([]string{"close", issueID, "--resolution", "wontfix", "--reason", "deadlock regression probe cleanup"}, 10*time.Second); err != nil {
		t.Fatalf("Run(close) error = %v", err)
	}
}
