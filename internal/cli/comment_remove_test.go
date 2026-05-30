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

func TestCommentRemove(t *testing.T) {
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

	t.Setenv("HOME", repo)
	t.Setenv("CODEX_HOME", filepath.Join(repo, ".codex-home"))

	run := func(args ...string) (bytes.Buffer, error) {
		var stdout bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		errCh := make(chan error, 1)
		go func() { errCh <- Run(ctx, &stdout, &stdout, args) }()
		select {
		case runErr := <-errCh:
			return stdout, runErr
		case <-ctx.Done():
			t.Fatalf("Run(%v) timed out: %v", args, ctx.Err())
			return bytes.Buffer{}, nil
		}
	}

	if _, err := run("init", "--skip-hooks", "--skip-agents", "--json"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	newOut, err := run("new", "--title", "comment removal", "--topic", "comments", "--type", "task", "--priority", "1", "--json")
	if err != nil {
		t.Fatalf("new error = %v", err)
	}
	var issue map[string]any
	if err := json.Unmarshal(newOut.Bytes(), &issue); err != nil {
		t.Fatalf("new output should be json: %v (out=%s)", err, newOut.String())
	}
	issueID, _ := issue["id"].(string)
	if issueID == "" {
		t.Fatalf("new returned no id: %s", newOut.String())
	}

	const marker = "delete-me-marker-xyz"
	addOut, err := run("comment", "add", issueID, "--body", marker, "--json")
	if err != nil {
		t.Fatalf("comment add error = %v", err)
	}
	var added map[string]any
	if err := json.Unmarshal(addOut.Bytes(), &added); err != nil {
		t.Fatalf("comment add output should be json: %v (out=%s)", err, addOut.String())
	}
	commentID, _ := added["id"].(string)
	if commentID == "" {
		t.Fatalf("comment add returned no id: %s", addOut.String())
	}

	before, err := run("show", issueID)
	if err != nil {
		t.Fatalf("show (before) error = %v", err)
	}
	if !bytes.Contains(before.Bytes(), []byte(marker)) {
		t.Fatalf("comment not visible in show before delete: %s", before.String())
	}

	rmOut, err := run("comment", "rm", commentID, "--json")
	if err != nil {
		t.Fatalf("comment rm error = %v", err)
	}
	var removed map[string]any
	if err := json.Unmarshal(rmOut.Bytes(), &removed); err != nil {
		t.Fatalf("comment rm output should be json: %v (out=%s)", err, rmOut.String())
	}
	if removed["id"] != commentID {
		t.Fatalf("comment rm --json id = %v, want %s", removed["id"], commentID)
	}
	if removed["body"] != marker {
		t.Fatalf("comment rm --json body = %v, want %q", removed["body"], marker)
	}

	after, err := run("show", issueID)
	if err != nil {
		t.Fatalf("show (after) error = %v", err)
	}
	if bytes.Contains(after.Bytes(), []byte(marker)) {
		t.Fatalf("comment still visible in show after delete: %s", after.String())
	}

	if _, err := run("comment", "rm", commentID, "--json"); err == nil {
		t.Fatalf("expected error deleting already-removed comment, got nil")
	}
}
