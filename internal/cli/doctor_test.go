package cli

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The identity header is where prefix provenance becomes observable: the one
// run that mints a prefix the user never chose must say so.
func TestPrintWorkspaceIdentityReportsPrefixSource(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	ws := workspace.Info{
		Location:    workspace.Location{StorageDir: "/tmp/store", GitCommonDir: "/tmp/repo/.git"},
		WorkspaceID: "ws-id",
		IssuePrefix: testIssuePrefix(t, "test"),
	}
	if err := printWorkspaceIdentity(&out, ws, idPrefixCensus{}); err != nil {
		t.Fatalf("printWorkspaceIdentity() error = %v", err)
	}
	if !strings.Contains(out.String(), `issue_prefix="test" issue_prefix_source=configured`) {
		t.Fatalf("identity line = %q, want configured provenance", out.String())
	}
}

func TestPrintWorkspaceIdentityReportsDerivedPrefix(t *testing.T) {
	t.Parallel()
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
	if err := printWorkspaceIdentity(&out, ws, idPrefixCensus{}); err != nil {
		t.Fatalf("printWorkspaceIdentity() error = %v", err)
	}
	if !strings.Contains(out.String(), "issue_prefix_source=derived") {
		t.Fatalf("identity line = %q, want derived provenance", out.String())
	}
}

func TestPrintSyncFreshness(t *testing.T) {
	t.Parallel()
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
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{Remote: "origin", Branch: "master", Synced: false}},
			wantSubstrs: []string{"sync:", "never synced with origin/master", "lit sync push"},
		},
		{
			name:        "up to date is honest about staleness",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{Remote: "origin", Branch: "master", Synced: true}},
			wantSubstrs: []string{"sync:", "up to date with origin/master", "as of last fetch"},
		},
		{
			name:        "ahead names push fix",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{Remote: "origin", Branch: "master", Synced: true, Ahead: 2}},
			wantSubstrs: []string{"sync:", "ahead of origin/master by 2", "not pushed", "as of last fetch", "lit sync push", "ahead=2 behind=0"},
		},
		{
			name:        "behind names pull fix and stays honest",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{Remote: "origin", Branch: "master", Synced: true, Behind: 3}},
			wantSubstrs: []string{"sync:", "behind origin/master by 3", "not pulled", "as of last fetch", "lit sync pull", "ahead=0 behind=3"},
		},
		{
			name:        "diverged reports both directions",
			report:      doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{Remote: "origin", Branch: "master", Synced: true, Ahead: 2, Behind: 3}},
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
	if !strings.Contains(got, "build:") {
		t.Fatalf("runDoctor() output missing build status line: %q", got)
	}
}

// TestRunDoctorSurfacesUnresolvedFreshness drives the resolution-failure path
// end to end: the test workspace is not a git repo, so reading git remotes
// fails and doctor must surface a loud "freshness unavailable" line carrying the
// reason — without aborting the integrity health check beside it.
func TestRunDoctorSurfacesUnresolvedFreshness(t *testing.T) {
	ap := newTestCLIApp(t)
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, ap, nil); err != nil {
		t.Fatalf("runDoctor() error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "integrity_check=ok") {
		t.Fatalf("runDoctor() output missing health line: %q", got)
	}
	if !strings.Contains(got, "sync: freshness unavailable") {
		t.Fatalf("runDoctor() output missing unresolved freshness line: %q", got)
	}
}

// A ticket outside an epic has no field on the status line, so the warning line
// is the only place doctor names it. The store planted here holds every shape
// that reaches no write path: a ticket with no parent and one under a task, as a
// recovery rebuild or a lit older than v7 leaves them, and a blocked-by cycle,
// which also leaves one edge inverted under any rank order. Doctor prints a
// warning line for each and no other, and a refused --fix still prints them.
func TestDoctorPrintsEachWarningItsHealthCheckFinds(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Title: "E", Topic: "hier", IssueType: "epic"})
	loose := h.createIssue(storage.CreateIssueInput{Title: "loose", Topic: "hier", IssueType: "task", ParentID: epic.ID})
	host := h.createIssue(storage.CreateIssueInput{Title: "host", Topic: "hier", IssueType: "task", ParentID: epic.ID})
	under := h.createIssue(storage.CreateIssueInput{Title: "under", Topic: "hier", IssueType: "task", ParentID: epic.ID})
	cycleEpic := h.createIssue(storage.CreateIssueInput{Title: "C", Topic: "dep", IssueType: "epic"})
	first := h.createIssue(storage.CreateIssueInput{Title: "first", Topic: "dep", IssueType: "task", ParentID: cycleEpic.ID})
	second := h.createIssue(storage.CreateIssueInput{Title: "second", Topic: "dep", IssueType: "task", ParentID: cycleEpic.ID})

	var clean bytes.Buffer
	if err := runDoctor(h.ctx, &clean, h.ap, nil); err != nil {
		t.Fatalf("runDoctor() on a contained hierarchy error = %v", err)
	}
	if strings.Contains(clean.String(), "warning: ") {
		t.Fatalf("doctor on a contained hierarchy should print no warning; got:\n%s", clean.String())
	}

	raw, err := storage.TestSupport.Of(h.ap.Store)
	if err != nil {
		t.Fatalf("TestSupport: %v", err)
	}
	if err := raw.ExecRawForTest(h.ctx, `DELETE FROM relations WHERE type = 'parent-child' AND src_id IN (?, ?)`, loose.ID, under.ID); err != nil {
		t.Fatalf("detach the tickets: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range [][3]string{
		{under.ID, host.ID, "parent-child"},
		{first.ID, second.ID, "blocked-by"},
		{second.ID, first.ID, "blocked-by"},
	} {
		if err := raw.ExecRawForTest(h.ctx,
			`INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, 'import')`,
			row[0], row[1], row[2], now); err != nil {
			t.Fatalf("plant %s %s %s: %v", row[0], row[2], row[1], err)
		}
	}

	containment := []string{
		"warning: tickets outside an epic: " + loose.ID + " (task) (every ticket except an epic lives inside one; move each into the epic it belongs to with 'lit parent set --child <id> --parent <epic-id>')",
		"warning: tickets under a parent that is not an epic: " + under.ID + " under " + host.ID + " (task) (only an epic contains tickets; move each into the epic it belongs to with 'lit parent set --child <id> --parent <epic-id>')",
		"warning: rank inversions: 1 (dependencies ranked below dependents)",
	}
	// The cycle may be named from either member; both rotations are the same cycle.
	cycleLine := func(a, b string) string {
		return "warning: blocked-by dependency cycle: " + a + " -> " + b + " -> " + a + " (no rank order exists; remove one edge with 'lit dep rm' to break it)"
	}
	assertWarnings := func(t *testing.T, got string) {
		t.Helper()
		var printed []string
		for line := range strings.Lines(got) {
			if strings.HasPrefix(line, "warning: ") {
				printed = append(printed, strings.TrimSuffix(line, "\n"))
			}
		}
		for _, cycle := range []string{cycleLine(first.ID, second.ID), cycleLine(second.ID, first.ID)} {
			if want := append(slices.Clone(containment), cycle); slices.Equal(printed, want) {
				return
			}
		}
		t.Fatalf("doctor printed these warning lines:\n%s\nwant, in order:\n%s\nthen the cycle %s <-> %s; full output:\n%s",
			strings.Join(printed, "\n"), strings.Join(containment, "\n"), first.ID, second.ID, got)
	}

	var buf bytes.Buffer
	if err := runDoctor(h.ctx, &buf, h.ap, nil); err != nil {
		t.Fatalf("runDoctor() error = %v, want exit 0: every read still answers over these warnings", err)
	}
	assertWarnings(t, buf.String())

	// The rank repair refuses on the cycle. The refusal is the command's
	// outcome, and the report it would otherwise have cost is still printed.
	var fixed bytes.Buffer
	err = runDoctor(h.ctx, &fixed, h.ap, []string{"--fix=rank"})
	if err == nil || !strings.Contains(err.Error(), "lit dep rm") {
		t.Fatalf("runDoctor(--fix=rank) error = %v, want the rank repair's refusal on the cycle", err)
	}
	assertWarnings(t, fixed.String())

	// A report cut short by its own write failure still carries the repair's.
	err = runDoctor(h.ctx, refusingWriter{}, h.ap, []string{"--fix=rank"})
	if err == nil || !strings.Contains(err.Error(), "stdout is gone") || !strings.Contains(err.Error(), "lit dep rm") {
		t.Fatalf("runDoctor(--fix=rank) to a refusing stdout error = %v, want both the write failure and the rank repair's refusal", err)
	}
}
