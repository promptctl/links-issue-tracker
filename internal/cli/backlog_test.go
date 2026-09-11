package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// backlogTestHarness mirrors readyTestHarness so the two surfaces can be
// exercised by the same builder helpers. Keeping a separate type clarifies
// which command is under test in each call site.
type backlogTestHarness struct {
	t   *testing.T
	ctx context.Context
	ap  *app.App
}

func newBacklogTestHarness(t *testing.T) backlogTestHarness {
	t.Helper()
	return backlogTestHarness{
		t:   t,
		ctx: context.Background(),
		ap:  newTestCLIApp(t),
	}
}

func (h backlogTestHarness) createIssue(input storage.CreateIssueInput) (id string) {
	h.t.Helper()
	if input.Prefix == "" {
		input.Prefix = h.ap.Workspace.IssuePrefix.Value()
	}
	// Fixtures author top-to-bottom in listing order, so append at the bottom
	// to make creation order equal rank order. Stated rather than inherited —
	// this fixture's order is its own premise, not a reading of the product
	// default it happens to agree with.
	input.Placement = storage.RankBottom
	issue, err := h.ap.Store.CreateIssue(h.ctx, input)
	if err != nil {
		h.t.Fatalf("CreateIssue(%q) error = %v", input.Title, err)
	}
	return issue.ID
}

func (h backlogTestHarness) addDependency(dependentID, dependencyID string) {
	h.t.Helper()
	if _, err := h.ap.Store.AddRelation(h.ctx, storage.AddRelationInput{
		SrcID: dependentID, DstID: dependencyID, Type: "blocks", CreatedBy: "agent",
	}); err != nil {
		h.t.Fatalf("AddRelation(blocks) error = %v", err)
	}
}

// runBacklogIDs renders the backlog and extracts the issue ID leading each row,
// in render order — the structured probe over the command's logic (which items,
// in what order) now read from the one canonical text surface.
func (h backlogTestHarness) runBacklogIDs(args ...string) []string {
	h.t.Helper()
	return issueIDsFromText(h.runBacklogText(args...))
}

func (h backlogTestHarness) runBacklogText(args ...string) string {
	h.t.Helper()
	var stdout bytes.Buffer
	if err := runWorkable(h.ctx, &stdout, h.ap, args, backlogView); err != nil {
		h.t.Fatalf("runBacklog(%v) error = %v", args, err)
	}
	return stdout.String()
}

// unblocksLineNames reports whether any "unblocks:" line names the id. Scoped to
// that line rather than to the whole text, because an id also appears in
// dependency lines and proves nothing there about the leverage line under test.
func unblocksLineNames(text, id string) bool {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "unblocks:") && strings.Contains(trimmed, id) {
			return true
		}
	}
	return false
}

// rankInversionWarning returns the inversion warning line, or "" when the view
// printed none.
func rankInversionWarning(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "rank inversion(s)") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// A row's "unblocks" line and the rank-inversion count are facts about the whole
// workable set, not about whichever slice of it is on screen. Both were read off
// the printed rows, so either narrowing deleted them silently — and the one that
// gets deleted belongs to the row that SURVIVED: exclude the dependent and its
// prerequisite keeps its place in the list with its leverage line quietly
// shortened, under a preamble still promising "what closing it would unblock".
//
// Two narrowings reach it. The focus scope is this change's own (a dependent off
// the focused path), and --limit is links-listing-85sd, which predates it; one
// population fixes both, which is why they are pinned together here.
func TestBacklogUnblocksLinesSurviveTheFocusScope(t *testing.T) {
	h := newBacklogTestHarness(t)
	// Creation order is rank order here, and it is load-bearing: offPath is
	// created FIRST so it outranks the prerequisite it depends on, which is what
	// makes the inversion it carries an OFF-path one. Built the other way round
	// every inversion sits on an on-path row, both views count it, and the
	// warning compares equal no matter which population it was counted over —
	// the assertion passes against the defect it was written to catch.
	offPath := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Off-path dependent", Topic: "noise", IssueType: "task"})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Goal", Topic: "goal", IssueType: "task", Labels: []string{FocusLabel}})
	onPath := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Path work", Topic: "goal", IssueType: "task"})
	h.addDependency(goal, onPath)
	// Off the focused path, and unblocked by an on-path row all the same.
	h.addDependency(offPath, onPath)

	text := h.runBacklogText()
	if !unblocksLineNames(text, offPath) {
		t.Fatalf("focused backlog names no unblocks line for %s — it sits off the focus path, but closing %s still unblocks it, and %s's row is on screen making the claim; got:\n%s", offPath, onPath, onPath, text)
	}

	// The count describes the stored ranks, so narrowing the view must not move
	// it: same warning, whether or not the scope is in force.
	if got, want := rankInversionWarning(text), rankInversionWarning(h.runBacklogText("--all")); got != want {
		t.Fatalf("rank inversion warning differs by view:\n  focused: %q\n  --all:   %q\nthe count is a property of the backlog, not of the rows on screen", got, want)
	}
}

func TestBacklogUnblocksLinesSurviveLimit(t *testing.T) {
	h := newBacklogTestHarness(t)
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocker", Topic: "lim", IssueType: "task"})
	second := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Dependent", Topic: "lim", IssueType: "task"})
	h.addDependency(second, first)

	// --limit 1 cuts the dependent's own row, not the fact that it is a dependent.
	text := h.runBacklogText("--limit", "1")
	if !unblocksLineNames(text, second) {
		t.Fatalf("`--limit 1` names no unblocks line for %s on %s's surviving row — the preamble promises what closing it would unblock, and --limit removed only the dependent's own row; got:\n%s", second, first, text)
	}
}

// Backlog must keep blocked items at their ranked position rather than push
// them to the bottom — that's the whole reason it exists alongside `lit ready`.
func TestBacklogKeepsBlockedItemsInRankOrder(t *testing.T) {
	h := newBacklogTestHarness(t)
	a := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A urgent", Topic: "blk", IssueType: "task", Priority: 1})
	b := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B urgent", Topic: "blk", IssueType: "task", Priority: 1})
	c := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C normal", Topic: "blk", IssueType: "task", Priority: 0})
	h.addDependency(b, a) // b is blocked

	got := h.runBacklogIDs()
	wantOrder := []string{a, b, c}
	if len(got) != len(wantOrder) {
		t.Fatalf("len(got) = %d, want %d; got=%v", len(got), len(wantOrder), got)
	}
	for i, want := range wantOrder {
		if got[i] != want {
			t.Fatalf("backlog[%d].ID = %q, want %q; full order=%v", i, got[i], want, got)
		}
	}
}

func TestBacklogTextShowsBlockedReasonsInline(t *testing.T) {
	h := newBacklogTestHarness(t)
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocker", Topic: "blk", IssueType: "task", Priority: 1})
	blocked := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocked", Topic: "blk", IssueType: "task", Priority: 1})
	flagged := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Needs design", Topic: "blk", IssueType: "task", Priority: 0, Labels: []string{NeedsDesignLabel}})
	h.addDependency(blocked, blocker)

	text := h.runBacklogText()
	if !strings.Contains(text, "depends on: "+blocker) {
		t.Fatalf("expected 'depends on: %s' for blocked item; got:\n%s", blocker, text)
	}
	if !strings.Contains(text, "unblocks: "+blocked) {
		t.Fatalf("expected 'unblocks: %s' on blocker; got:\n%s", blocked, text)
	}
	if !strings.Contains(text, "blocked: needs-design") {
		t.Fatalf("expected 'blocked: needs-design' line for %s; got:\n%s", flagged, text)
	}
}

func TestBacklogTextShowsPreamble(t *testing.T) {
	h := newBacklogTestHarness(t)
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Anything", Topic: "any", IssueType: "task", Priority: 1})

	text := h.runBacklogText()
	if !strings.Contains(text, "backlog in priority/rank order") {
		t.Fatalf("missing preamble; got:\n%s", text)
	}
	// The completeness claim moved out of the preamble and into the notice, so
	// that it can say the other thing when the view is scoped instead of
	// asserting "full" over a narrowed list (links-listing-ju7i).
	if !strings.Contains(text, "Nothing is hidden: every workable item is listed.") {
		t.Fatalf("missing scope notice; got:\n%s", text)
	}
	if !strings.Contains(text, "─") {
		t.Fatalf("missing separator; got:\n%s", text)
	}
}

func TestBacklogEmptyDataShowsMarker(t *testing.T) {
	h := newBacklogTestHarness(t)
	text := h.runBacklogText()
	if !strings.Contains(text, "(backlog empty)") {
		t.Fatalf("expected '(backlog empty)'; got:\n%s", text)
	}
}

func TestBacklogRespectsLimit(t *testing.T) {
	h := newBacklogTestHarness(t)
	for i := 0; i < 5; i++ {
		h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "T", Topic: "lim", IssueType: "task", Priority: 1})
	}

	got := h.runBacklogIDs("--limit", "2")
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (limit)", len(got))
	}
}

func TestBacklogIncludesInProgressInline(t *testing.T) {
	h := newBacklogTestHarness(t)
	a := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A", Topic: "inp", IssueType: "task", Priority: 1})
	b := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B", Topic: "inp", IssueType: "task", Priority: 1})
	c := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C", Topic: "inp", IssueType: "task", Priority: 1})
	if _, err := h.ap.Store.Apply(h.ctx, b, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("start(%s) error = %v", b, err)
	}

	got := h.runBacklogIDs()
	wantOrder := []string{a, b, c}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3; got=%v", len(got), got)
	}
	for i, want := range wantOrder {
		if got[i] != want {
			t.Fatalf("backlog[%d].ID = %q, want %q; full order=%v", i, got[i], want, got)
		}
	}

	text := h.runBacklogText()
	if !strings.Contains(text, "in_progress:") {
		t.Fatalf("expected 'in_progress:' suffix for started item; got:\n%s", text)
	}
}

// issueIDsFromText extracts the issue ID leading each row of a list command's
// text output, in render order. Issue IDs are the first <prefix>-<token>
// identifier on a row; rows without one (preamble, separators, context lines)
// contribute nothing. This is the text-surface equivalent of reading the
// ordered ID list the old --json probe produced.
func issueIDsFromText(text string) []string {
	var ids []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// A primary list row leads with the issue ID, or with an "N." position
		// number followed by it. Indented context lines (epic:/depends on:/...)
		// lead with a label token and are intentionally skipped.
		if isIssueIDToken(fields[0]) {
			ids = append(ids, fields[0])
			continue
		}
		if isPositionToken(fields[0]) && len(fields) > 1 && isIssueIDToken(fields[1]) {
			ids = append(ids, fields[1])
		}
	}
	return ids
}

func isPositionToken(s string) bool {
	if len(s) < 2 || s[len(s)-1] != '.' {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isIssueIDToken(s string) bool {
	// An issue ID is <prefix>-<token>, with child IDs appending ".N" segments
	// (e.g. test-ab12, test-ab12.1.3). The leading dash is what separates a real
	// ID from a position number ("1.") or a label ("epic:"); a dot only appears
	// inside child IDs, always after that dash.
	dash := strings.IndexByte(s, '-')
	if dash <= 0 || dash == len(s)-1 {
		return false
	}
	dot := strings.IndexByte(s, '.')
	if dot >= 0 && dot < dash {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isDigit && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// The sibling gate is a blocking annotation like any other, and the backlog
// owes it a line. It had none: nonDependencyBlockingReasons had no phrasing
// for EarlierSiblingPending, so a row held back by an earlier same-lane
// sibling rendered with nothing under it — top of the queue, no visible reason
// — while routing skipped it as unready. That gap is the whole of
// links-claims-gxxw: the backlog is the surface that tells an agent what `lit
// next` will serve, so this pins both halves at once, the rendered reason and
// the pick it explains.
func TestBacklogNamesTheSiblingGateAndNextAgreesWithIt(t *testing.T) {
	h := newBacklogTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "gate", IssueType: "epic", Priority: 1})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "gate", IssueType: "task", Priority: 1, ParentID: epic})
	second := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "gate", IssueType: "task", Priority: 1, ParentID: epic})

	text := h.runBacklogText()
	if want := "blocked: earlier sibling " + first + " still open"; !strings.Contains(text, want) {
		t.Fatalf("backlog does not say why %s is held back: want %q; got:\n%s", second, want, text)
	}

	rows, details, _, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	outcome := routeNext(rows, details, claims.Standings{}, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane", outcome, outcome)
	}
	if served.Row.ID != first {
		t.Fatalf("served = %q, want %q — the row the backlog ranks first and marks unblocked", served.Row.ID, first)
	}
}

// The gate that keeps the omission from coming back, driven off the annotation
// registry rather than a list maintained beside it: every kind the registry
// classifies as blocking must reach the reader. OpenDependency is the one
// deliberate silence here — printBacklogContext gives it its own "depends on:"
// line — so it is named as the exception rather than left to a length check
// that would pass for a kind nobody phrased.
// [LAW:one-source-of-truth] [LAW:verifiable-goals]
func TestBacklogPhrasesEveryBlockingKind(t *testing.T) {
	for _, kind := range annotation.Kinds() {
		if kind.ReadinessRole() != annotation.RoleBlocking {
			continue
		}
		readiness := ClassifyReadiness([]annotation.Annotation{{Kind: kind, Message: "test-detail"}})
		reasons := nonDependencyBlockingReasons(readiness)
		if kind == annotation.OpenDependency {
			if len(reasons) != 0 {
				t.Errorf("kind %s rendered %v in the \"blocked:\" line; it belongs to \"depends on:\" alone", kind, reasons)
			}
			continue
		}
		if len(reasons) != 1 || reasons[0] == "" {
			t.Errorf("kind %s rendered %v, want exactly one non-empty phrase — a blocking kind the backlog cannot phrase is a blocker the reader never sees", kind, reasons)
		}
	}
}
