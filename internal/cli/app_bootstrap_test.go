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

func TestResolveDoctorAccessMode(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want appAccessMode
	}{
		{name: "no flags defaults to read", args: nil, want: appAccessRead},
		{name: "json only is read", args: []string{"--json"}, want: appAccessRead},
		{name: "fix all implies write", args: []string{"--fix"}, want: appAccessWrite},
		{name: "fix named implies write", args: []string{"--fix", "rank"}, want: appAccessWrite},
		{name: "fix with json implies write", args: []string{"--fix", "--json"}, want: appAccessWrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDoctorAccessMode(tc.args)
			if got != tc.want {
				t.Fatalf("resolveDoctorAccessMode(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
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
