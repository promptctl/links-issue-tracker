package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/engine"
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

	before := doltHead(t, ws)

	var stdout bytes.Buffer
	if err := Run(context.Background(), cwdInvocation(&stdout, &stdout, []string{"ls"})); err != nil {
		t.Fatalf("Run(ls) error = %v", err)
	}

	// A freshly initialized workspace has no issues; the text listing emits no
	// rows. (The point of this test is that the read did not create a commit.)
	if got := strings.TrimSpace(stdout.String()); got != "" {
		t.Fatalf("ls on an empty workspace = %q, want no rows", got)
	}

	if after := doltHead(t, ws); after != before {
		t.Fatalf("ls moved HEAD from %s to %s: it created a commit", before, after)
	}
}

// doltHead is the HEAD commit of ws's database, read in process below the
// migration gate.
func doltHead(t *testing.T, ws workspace.Info) string {
	t.Helper()
	dump, err := store.DumpRaw(context.Background(), testEnv(), ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("read HEAD of %s: %v", ws.DatabasePath, err)
	}
	return dump.DoltHead
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
	if err := Run(context.Background(), cwdInvocation(&stdout, &stdout, []string{"init", "--skip-hooks"})); err != nil {
		t.Fatalf("Run(init --skip-hooks) error = %v", err)
	}

	ws, err := workspace.Resolve(testEnv(), repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	st, err := engine.Open(context.Background(), testEnv(), engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}
	return repo, ws
}

func TestResolveDoctorAccessMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want app.AccessMode
	}{
		{name: "no flags defaults to read", args: nil, want: app.AccessRead},
		{name: "fix all implies write", args: []string{"--fix"}, want: app.AccessWrite},
		{name: "fix named implies write", args: []string{"--fix", "rank"}, want: app.AccessWrite},
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
	t.Parallel()
	// [LAW:behavior-not-structure] Pins the contract: which subcommands open
	// the app read-only vs writable, that a bad path is a typed usage refusal
	// before any app opens, and that a help-shaped argument is classified as a
	// help request — for the family itself, and for a nested group before its
	// dispatch pipeline could acquire the app. The assertions are typed
	// (errors.As), not string equality: HelpRequestedError.Error() coincides
	// with the usage text, so a message check cannot tell an answered help
	// from a rejected path.
	cases := []struct {
		name          string
		family        commandFamily[appSubcommand]
		args          []string
		want          app.AccessMode
		wantErr       bool
		wantHelpUsage string
	}{
		{name: "dep ls is read", family: depFamily, args: []string{"ls"}, want: app.AccessRead},
		{name: "dep add is write", family: depFamily, args: []string{"add", "a", "b"}, want: app.AccessWrite},
		{name: "dep rm is write", family: depFamily, args: []string{"rm", "a", "b"}, want: app.AccessWrite},
		{name: "dep unknown rejected", family: depFamily, args: []string{"bogus"}, wantErr: true},
		{name: "dep empty rejected", family: depFamily, args: nil, wantErr: true},
		{name: "dep help flag answered as help", family: depFamily, args: []string{"--help"}, wantHelpUsage: depFamily.usage},
		{name: "bulk nested group help answered before dispatch", family: bulkFamily, args: []string{"label", "--help"}, wantHelpUsage: bulkLabelFamily.usage},
		{name: "backup create is read", family: backupFamily, args: []string{"create"}, want: app.AccessRead},
		{name: "backup list is read", family: backupFamily, args: []string{"list"}, want: app.AccessRead},
		{name: "backup restore is write", family: backupFamily, args: []string{"restore", "--latest"}, want: app.AccessWrite},
		{name: "backup unknown rejected", family: backupFamily, args: []string{"prune"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.family.resolve(underInvocation(context.Background()), tc.args)
			if tc.wantHelpUsage != "" {
				var help HelpRequestedError
				if !errors.As(err, &help) {
					t.Fatalf("resolve(%v) error = %v, want HelpRequestedError", tc.args, err)
				}
				if help.Usage != tc.wantHelpUsage {
					t.Fatalf("resolve(%v) help usage = %q, want %q", tc.args, help.Usage, tc.wantHelpUsage)
				}
				return
			}
			if tc.wantErr {
				var usage UsageError
				if !errors.As(err, &usage) {
					t.Fatalf("resolve(%v) error = %v, want UsageError", tc.args, err)
				}
				if usage.Message != tc.family.usage {
					t.Fatalf("resolve(%v) error = %q, want family usage %q", tc.args, usage.Message, tc.family.usage)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve(%v) error = %v", tc.args, err)
			}
			if got.access != tc.want {
				t.Fatalf("resolve(%v) access = %v, want %v", tc.args, got.access, tc.want)
			}
			if got.declare == nil {
				t.Fatalf("resolve(%v) returned a row with no leaf", tc.args)
			}
		})
	}
}
