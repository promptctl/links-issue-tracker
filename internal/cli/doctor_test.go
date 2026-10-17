package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
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

// runDoctorRecorded is doctor's parse and work under a recorder the test keeps,
// ended the way the dispatcher ends it, so it can read the facts the
// invocation's event carries beside what it printed.
func runDoctorRecorded(t *testing.T, h readyTestHarness, args ...string) (map[string]any, string) {
	t.Helper()
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(h.ctx, recorder)
	l := doctorLeaf()
	var out bytes.Buffer
	positional, err := parseLeaf(ctx, l, args, &out)
	if err != nil {
		t.Fatalf("parseLeaf: %v", err)
	}
	err = l.work(ctx, &out, h.ap, positional)
	if err != nil {
		t.Fatalf("doctor %v error = %v\n%s", args, err, out.String())
	}
	return writtenFacts(t, recorder.Finish(invocationEnd(time.Now(), err, nil))), out.String()
}

// A key two live tickets hold is named on the status line, in a warning line,
// and on the invocation's event; `--fix=rank` gives each holder its own key,
// and the report it leaves says none is shared.
func TestDoctorReportsASharedRankKeyAndFixSeparatesIt(t *testing.T) {
	h := newReadyTestHarness(t)
	first := h.createIssue(storage.CreateIssueInput{Title: "first", Topic: "rank", IssueType: "epic"})
	h.createIssue(storage.CreateIssueInput{Title: "second", Topic: "rank", IssueType: "epic"})
	third := h.createIssue(storage.CreateIssueInput{Title: "third", Topic: "rank", IssueType: "epic"})
	raw, err := storage.TestSupport.Of(h.ap.Store)
	if err != nil {
		t.Fatalf("TestSupport: %v", err)
	}
	if err := raw.ExecRawForTest(h.ctx, `UPDATE issues SET item_rank = ? WHERE id = ?`, first.Rank, third.ID); err != nil {
		t.Fatalf("plant the shared key: %v", err)
	}
	holders := []string{first.ID, third.ID}
	slices.Sort(holders)

	facts, out := runDoctorRecorded(t, h)
	if !strings.Contains(out, " duplicate_ranks=1 ") {
		t.Errorf("status line lacks duplicate_ranks=1:\n%s", out)
	}
	warning := fmt.Sprintf("warning: duplicate rank keys: 1 (%q shared by %s; ", first.Rank, strings.Join(holders, ", "))
	if !strings.Contains(out, warning) {
		t.Errorf("output lacks %q:\n%s", warning, out)
	}
	if facts["doctor.duplicate_ranks"] != "1" {
		t.Errorf("event fact doctor.duplicate_ranks = %v, want \"1\"", facts["doctor.duplicate_ranks"])
	}

	facts, out = runDoctorRecorded(t, h, "--fix=rank")
	if !strings.Contains(out, " duplicate_ranks=0 ") || strings.Contains(out, "warning: ") {
		t.Errorf("after --fix=rank the report still names a shared key:\n%s", out)
	}
	if facts["doctor.duplicate_ranks"] != "0" {
		t.Errorf("event fact doctor.duplicate_ranks after the fix = %v, want \"0\"", facts["doctor.duplicate_ranks"])
	}
}

// A task waiting on a task in an epic listed after its own is a rank
// inversion in the queue and in lit doctor alike, however the two tasks' own
// keys compare; `--fix=rank` clears it from both, and the invocation's event
// says what the check found, what the repair moved, and that no cycle stood in
// its way. The two epics are filed one way and listed the other.
func TestARankInversionAcrossEpicsIsFlaggedRepairedAndRecorded(t *testing.T) {
	h := newReadyTestHarness(t)
	early := h.createIssue(storage.CreateIssueInput{Title: "early", Topic: "rank", IssueType: "epic"})
	late := h.createIssue(storage.CreateIssueInput{Title: "late", Topic: "rank", IssueType: "epic"})
	lateTask := h.createIssue(storage.CreateIssueInput{Title: "late task", Topic: "rank", IssueType: "task", ParentID: late.ID})
	earlyTask := h.createIssue(storage.CreateIssueInput{Title: "early task", Topic: "rank", IssueType: "task", ParentID: early.ID})
	if _, err := h.ap.Store.RankToTop(h.ctx, late.ID); err != nil {
		t.Fatalf("RankToTop: %v", err)
	}
	h.addDependency(lateTask.ID, earlyTask.ID)
	inversions := func() []string {
		row := findRow(h.runWorkableAnnotated(workableFilter{}, 0), lateTask.ID)
		return ClassifyReadiness(row.Annotations).RankInversions()
	}
	if got := inversions(); !slices.Equal(got, []string{earlyTask.ID}) {
		t.Errorf("queue rank inversions of %s = %v, want [%s]", lateTask.ID, got, earlyTask.ID)
	}

	facts, out := runDoctorRecorded(t, h)
	if !strings.Contains(out, " rank_inversions=1 ") {
		t.Errorf("status line lacks rank_inversions=1:\n%s", out)
	}
	if facts["doctor.rank_inversions"] != "1" {
		t.Errorf("event fact doctor.rank_inversions = %v, want \"1\"", facts["doctor.rank_inversions"])
	}

	facts, out = runDoctorRecorded(t, h, "--fix=rank")
	if !strings.Contains(out, " rank_inversions=0 ") {
		t.Errorf("after --fix=rank the status line lacks rank_inversions=0:\n%s", out)
	}
	for fact, want := range map[string]string{"doctor.reranked": "1", "doctor.rank_inversions": "0", "doctor.dependency_cycle": "none"} {
		if got := fmt.Sprint(facts[fact]); got != want {
			t.Errorf("event fact %s after the fix = %v, want %q", fact, got, want)
		}
	}
	if got := inversions(); len(got) != 0 {
		t.Errorf("queue rank inversions of %s after the fix = %v, want none", lateTask.ID, got)
	}
}

// Two epics whose tasks wait on each other's are interleaved: a workable plan
// no order of the two epics lists every dependency first. The invocation's
// event names the group and no cycle, the queue flags no inversion it would
// send an agent to `lit doctor --fix` for, and the fix runs.
func TestDoctorRecordsInterleavedEpicsAndNoCycle(t *testing.T) {
	h := newReadyTestHarness(t)
	first := h.createIssue(storage.CreateIssueInput{Title: "first", Topic: "rank", IssueType: "epic"})
	second := h.createIssue(storage.CreateIssueInput{Title: "second", Topic: "rank", IssueType: "epic"})
	firstTask := h.createIssue(storage.CreateIssueInput{Title: "first task", Topic: "rank", IssueType: "task", ParentID: first.ID})
	firstOther := h.createIssue(storage.CreateIssueInput{Title: "first other", Topic: "rank", IssueType: "task", ParentID: first.ID})
	secondTask := h.createIssue(storage.CreateIssueInput{Title: "second task", Topic: "rank", IssueType: "task", ParentID: second.ID})
	secondOther := h.createIssue(storage.CreateIssueInput{Title: "second other", Topic: "rank", IssueType: "task", ParentID: second.ID})
	h.addDependency(firstTask.ID, secondTask.ID)
	h.addDependency(secondOther.ID, firstOther.ID)

	for _, args := range [][]string{nil, {"--fix=rank"}} {
		facts, out := runDoctorRecorded(t, h, args...)
		for fact, want := range map[string]string{"doctor.interleaved": "1", "doctor.dependency_cycle": "none", "doctor.rank_inversions": "0"} {
			if got := fmt.Sprint(facts[fact]); got != want {
				t.Errorf("doctor %v: event fact %s = %v, want %q\n%s", args, fact, got, want, out)
			}
		}
	}
	if count := deriveQueueFacts(h.runWorkableAnnotated(workableFilter{}, 0)).RankInversions(); count != 0 {
		t.Errorf("queue rank inversions = %d, want 0: no repair could clear them", count)
	}
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}
