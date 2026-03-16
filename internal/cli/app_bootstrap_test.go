package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/doltcli"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestResolveFsckAccessMode(t *testing.T) {
	testCases := []struct {
		name string
		args []string
		want appAccessMode
	}{
		{
			name: "default is read only",
			args: []string{},
			want: appAccessRead,
		},
		{
			name: "fsck repair is writable",
			args: []string{"--repair"},
			want: appAccessWrite,
		},
		{
			name: "invalid flag falls back to writable",
			args: []string{"--repair=nope"},
			want: appAccessWrite,
		},
	}

	for _, tc := range testCases {
		got := resolveFsckAccessMode(tc.args)
		if got != tc.want {
			t.Fatalf("%s access mode = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadCommandDoesNotCreateStartupCommit(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	repoPath := filepath.Join(ws.DatabasePath, "links")
	beforeLog, err := doltcli.Run(context.Background(), repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log before ls error = %v", err)
	}

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"ls", "--json"}); err != nil {
		t.Fatalf("Run(ls --json) error = %v", err)
	}

	var issues []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		t.Fatalf("json.Unmarshal(ls output) error = %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("ls issues = %#v, want empty", issues)
	}

	afterLog, err := doltcli.Run(context.Background(), repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log after ls error = %v", err)
	}
	if countNonEmptyLines(afterLog) != countNonEmptyLines(beforeLog) {
		t.Fatalf("ls created extra commit:\nbefore:\n%s\nafter:\n%s", beforeLog, afterLog)
	}
}

func initBootstrapTestRepo(t *testing.T) (string, workspace.Info) {
	t.Helper()
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

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"init", "--skip-hooks", "--skip-agents", "--json"}); err != nil {
		t.Fatalf("Run(init --skip-hooks --skip-agents --json) error = %v", err)
	}

	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	st, err := store.Open(context.Background(), ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}
	return repo, ws
}

func countNonEmptyLines(input string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(input), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
