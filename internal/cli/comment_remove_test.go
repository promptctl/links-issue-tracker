package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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
		go func() { errCh <- Run(ctx, cwdInvocation(&stdout, &stdout, args)) }()
		select {
		case runErr := <-errCh:
			return stdout, runErr
		case <-ctx.Done():
			t.Fatalf("Run(%v) timed out: %v", args, ctx.Err())
			return bytes.Buffer{}, nil
		}
	}

	if _, err := run("init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	// `lit new` text prints the issue summary, whose first token is the id.
	epicOut, err := run("new", "--title", "comments", "--topic", "comments", "--type", "epic")
	if err != nil {
		t.Fatalf("new epic error = %v", err)
	}
	newOut, err := run("new", "--title", "comment removal", "--topic", "comments", "--type", "task", "--priority", "1", "--parent", firstToken(epicOut.String()))
	if err != nil {
		t.Fatalf("new error = %v", err)
	}
	issueID := firstToken(newOut.String())
	if issueID == "" {
		t.Fatalf("new returned no id: %s", newOut.String())
	}

	const marker = "delete-me-marker-xyz"
	// `lit comment add` text prints "<issueID> <commentID>".
	addOut, err := run("comment", "add", issueID, "--body", marker)
	if err != nil {
		t.Fatalf("comment add error = %v", err)
	}
	commentID := secondToken(addOut.String())
	if commentID == "" {
		t.Fatalf("comment add returned no comment id: %s", addOut.String())
	}

	before, err := run("show", issueID)
	if err != nil {
		t.Fatalf("show (before) error = %v", err)
	}
	// The id rm takes is read off `lit show`, the way an agent finds it: the
	// comment's row leads with it.
	shownID := shownCommentID(before.String(), marker)
	if shownID != commentID {
		t.Fatalf("show lists the comment under id %q, want %s:\n%s", shownID, commentID, before.String())
	}

	// `lit comment rm` text prints "<issueID> <commentID>" for the removed
	// comment; assert it names the comment we just removed.
	rmOut, err := run("comment", "rm", shownID)
	if err != nil {
		t.Fatalf("comment rm error = %v", err)
	}
	if secondToken(rmOut.String()) != commentID {
		t.Fatalf("comment rm output = %q, want comment id %s", rmOut.String(), commentID)
	}

	after, err := run("show", issueID)
	if err != nil {
		t.Fatalf("show (after) error = %v", err)
	}
	if bytes.Contains(after.Bytes(), []byte(marker)) {
		t.Fatalf("comment still visible in show after delete: %s", after.String())
	}

	if _, err := run("comment", "rm", commentID); err == nil {
		t.Fatalf("expected error deleting already-removed comment, got nil")
	}
}

// firstToken returns the first whitespace-delimited token of the first
// non-empty line — the id leading a `lit new` / issue-summary text row.
func firstToken(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

// secondToken returns the second whitespace-delimited token of the first
// non-empty line — the comment id in `lit comment add/rm`'s "<issue> <comment>".
func secondToken(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			return fields[1]
		}
	}
	return ""
}

// shownCommentID returns the id leading the `lit show` comment row whose
// one-line body is body — the "- <id> [author] <body>" shape. Only a row starts
// at column 0, so an indented body line never matches, and an author holding a
// space still does.
func shownCommentID(show, body string) string {
	for _, line := range strings.Split(show, "\n") {
		row, isRow := strings.CutPrefix(line, "- ")
		if isRow && strings.HasSuffix(row, "] "+body) {
			id, _, _ := strings.Cut(row, " ")
			return id
		}
	}
	return ""
}
