package cli

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The identity header is where prefix provenance becomes observable: the one
// run that mints a prefix the user never chose must say so.
func TestPrintWorkspaceIdentityReportsPrefixSource(t *testing.T) {
	var out bytes.Buffer
	ws := workspace.Info{
		StorageDir:   "/tmp/store",
		WorkspaceID:  "ws-id",
		IssuePrefix:  testIssuePrefix(t, "test"),
		GitCommonDir: "/tmp/repo/.git",
	}
	if err := printWorkspaceIdentity(&out, ws); err != nil {
		t.Fatalf("printWorkspaceIdentity() error = %v", err)
	}
	if !strings.Contains(out.String(), "issue_prefix=test issue_prefix_source=configured") {
		t.Fatalf("identity line = %q, want configured provenance", out.String())
	}
}

func TestPrintWorkspaceIdentityReportsDerivedPrefix(t *testing.T) {
	repo := t.TempDir()
	gitInit := exec.Command("git", "init")
	gitInit.Dir = repo
	if output, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	// A derived PrefixSpec is only mintable by the workspace resolver itself —
	// the first resolve of a fresh repo derives the prefix from the repo name.
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	if !ws.IssuePrefix.Derived() {
		t.Fatalf("first resolve of a fresh repo must derive the prefix")
	}

	var out bytes.Buffer
	if err := printWorkspaceIdentity(&out, ws); err != nil {
		t.Fatalf("printWorkspaceIdentity() error = %v", err)
	}
	if !strings.Contains(out.String(), "issue_prefix_source=derived") {
		t.Fatalf("identity line = %q, want derived provenance", out.String())
	}
}

func TestPrintSyncFreshness(t *testing.T) {
	cases := []struct {
		name        string
		report      doctorSyncReport
		wantSubstrs []string
	}{
		{
			name:        "no remote",
			report:      doctorSyncReport{Kind: doctorSyncNoRemote},
			wantSubstrs: []string{"sync:", "no git remote configured", "lit sync push"},
		},
		{
			name:        "unresolved surfaces the reason",
			report:      doctorSyncReport{Kind: doctorSyncUnresolved, Detail: "read git remotes: boom"},
			wantSubstrs: []string{"sync:", "freshness unavailable", "read git remotes: boom"},
		},
		{
			name:        "never synced",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{Remote: "origin", Branch: "master", Synced: false}},
			wantSubstrs: []string{"sync:", "never synced with origin/master", "lit sync push"},
		},
		{
			name:        "up to date is honest about staleness",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{Remote: "origin", Branch: "master", Synced: true}},
			wantSubstrs: []string{"sync:", "up to date with origin/master", "as of last fetch"},
		},
		{
			name:        "ahead names push fix",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{Remote: "origin", Branch: "master", Synced: true, Ahead: 2}},
			wantSubstrs: []string{"sync:", "ahead of origin/master by 2", "not pushed", "lit sync push", "ahead=2 behind=0"},
		},
		{
			name:        "behind names pull fix and stays honest",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{Remote: "origin", Branch: "master", Synced: true, Behind: 3}},
			wantSubstrs: []string{"sync:", "behind origin/master by 3", "not pulled", "as of last fetch", "lit sync pull", "ahead=0 behind=3"},
		},
		{
			name:        "diverged reports both directions",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{Remote: "origin", Branch: "master", Synced: true, Ahead: 2, Behind: 3}},
			wantSubstrs: []string{"sync:", "diverged from origin/master", "2 local", "3 remote", "as of last fetch", "lit sync pull", "ahead=2 behind=3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := printSyncFreshness(&buf, tc.report); err != nil {
				t.Fatalf("printSyncFreshness() error = %v", err)
			}
			got := buf.String()
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Fatalf("printSyncFreshness() = %q, missing %q", got, want)
				}
			}
		})
	}
}

// TestRunDoctorReportsNoRemoteFreshness exercises the whole doctor command in a
// real git repo with no remote, so the freshness line travels the actual
// resolve→render path and lands in doctor's output beside the health report.
func TestRunDoctorReportsNoRemoteFreshness(t *testing.T) {
	repo, _ := initBootstrapTestRepo(t)
	ctx := context.Background()
	ap, err := app.Open(ctx, repo, app.AccessRead)
	if err != nil {
		t.Fatalf("app.Open() error = %v", err)
	}
	defer ap.Close()

	var buf bytes.Buffer
	if err := runDoctor(ctx, &buf, ap, nil); err != nil {
		t.Fatalf("runDoctor() error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "integrity_check=ok") {
		t.Fatalf("runDoctor() output missing health line: %q", got)
	}
	if !strings.Contains(got, "sync: no git remote configured") {
		t.Fatalf("runDoctor() output missing no-remote freshness line: %q", got)
	}
}
