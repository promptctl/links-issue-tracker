package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/doltcli"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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
		want app.AccessMode
	}{
		{name: "no flags defaults to read", args: nil, want: app.AccessRead},
		{name: "json only is read", args: []string{"--json"}, want: app.AccessRead},
		{name: "fix all implies write", args: []string{"--fix"}, want: app.AccessWrite},
		{name: "fix named implies write", args: []string{"--fix", "rank"}, want: app.AccessWrite},
		{name: "fix with json implies write", args: []string{"--fix", "--json"}, want: app.AccessWrite},
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

func TestCommandFamilyResolve(t *testing.T) {
	// [LAW:behavior-not-structure] Pins the contract: which subcommands open
	// the app read-only vs writable, and that illegal paths fail with usage
	// before any app opens.
	cases := []struct {
		name    string
		family  commandFamily[appSubcommand]
		args    []string
		want    app.AccessMode
		wantErr bool
	}{
		{name: "dep ls is read", family: depFamily, args: []string{"ls"}, want: app.AccessRead},
		{name: "dep add is write", family: depFamily, args: []string{"add", "a", "b"}, want: app.AccessWrite},
		{name: "dep rm is write", family: depFamily, args: []string{"rm", "a", "b"}, want: app.AccessWrite},
		{name: "dep unknown rejected", family: depFamily, args: []string{"bogus"}, wantErr: true},
		{name: "dep empty rejected", family: depFamily, args: nil, wantErr: true},
		{name: "dep help flag rejected", family: depFamily, args: []string{"--help"}, wantErr: true},
		{name: "backup create is read", family: backupFamily, args: []string{"create"}, want: app.AccessRead},
		{name: "backup list is read", family: backupFamily, args: []string{"list"}, want: app.AccessRead},
		{name: "backup restore is write", family: backupFamily, args: []string{"restore", "--latest"}, want: app.AccessWrite},
		{name: "backup unknown rejected", family: backupFamily, args: []string{"prune"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.family.resolve(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolve(%v) error = nil, want usage error", tc.args)
				}
				if err.Error() != tc.family.usage {
					t.Fatalf("resolve(%v) error = %q, want family usage %q", tc.args, err, tc.family.usage)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve(%v) error = %v", tc.args, err)
			}
			if got.access != tc.want {
				t.Fatalf("resolve(%v) access = %v, want %v", tc.args, got.access, tc.want)
			}
			if got.run == nil {
				t.Fatalf("resolve(%v) returned a row with no handler", tc.args)
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
