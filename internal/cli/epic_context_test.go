package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// epicFixture builds an epic and returns the app plus a helper to add children
// in rank order (creation order = bottom rank, so children stack in call order).
type epicFixture struct {
	t      *testing.T
	ctx    context.Context
	ap     *app.App
	epicID string
}

func newEpicFixture(t *testing.T, epicTitle, epicDesc string) epicFixture {
	t.Helper()
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: epicTitle, Description: epicDesc, Topic: "epic-view", IssueType: "epic", Priority: 1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	return epicFixture{t: t, ctx: ctx, ap: ap, epicID: epic.ID}
}

func (f epicFixture) addChild(title string) string {
	f.t.Helper()
	child, err := f.ap.Store.CreateIssue(f.ctx, store.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "epic-view", IssueType: "task", Priority: 0, ParentID: f.epicID,
		// Author children top-to-bottom in call order: append at the bottom so
		// creation order equals rank order (production default is top).
		Placement: store.RankBottom,
	})
	if err != nil {
		f.t.Fatalf("CreateIssue(child %q) error = %v", title, err)
	}
	return child.ID
}

func (f epicFixture) transition(id, action string) {
	f.t.Helper()
	if _, err := f.ap.Store.TransitionIssue(f.ctx, store.TransitionIssueInput{IssueID: id, Action: action, CreatedBy: "test"}); err != nil {
		f.t.Fatalf("TransitionIssue(%s, %s) error = %v", id, action, err)
	}
}

// block makes blocked depend on blocker (blocks convention: src=dependent).
func (f epicFixture) block(blocked, blocker string) {
	f.t.Helper()
	if _, err := f.ap.Store.AddRelation(f.ctx, store.AddRelationInput{SrcID: blocked, DstID: blocker, Type: "blocks", CreatedBy: "test"}); err != nil {
		f.t.Fatalf("AddRelation(blocks %s<-%s) error = %v", blocked, blocker, err)
	}
}

func (f epicFixture) render(focused string) string {
	f.t.Helper()
	ec, err := buildEpicContext(f.ctx, f.ap.Store, f.epicID, focused)
	if err != nil {
		f.t.Fatalf("buildEpicContext error = %v", err)
	}
	return renderEpicContext(ec)
}

func TestRenderEpicContextEmptyEpic(t *testing.T) {
	f := newEpicFixture(t, "Empty epic", "# Why this exists\nbecause reasons")
	out := f.render("")

	if !strings.Contains(out, "Epic: "+f.epicID+" — Empty epic") {
		t.Errorf("missing epic header line in:\n%s", out)
	}
	if !strings.Contains(out, "Why: Why this exists") {
		t.Errorf("why line should strip markdown heading, got:\n%s", out)
	}
	if !strings.Contains(out, "Children:\n  (none)") {
		t.Errorf("empty epic should render (none), got:\n%s", out)
	}
}

func TestRenderEpicContextAllClosed(t *testing.T) {
	f := newEpicFixture(t, "Closed epic", "all done")
	c1 := f.addChild("First")
	c2 := f.addChild("Second")
	f.transition(c1, "close")
	f.transition(c2, "close")

	out := f.render("")
	for _, id := range []string{c1, c2} {
		want := "    [closed]      " + id + "  "
		if !strings.Contains(out, want) {
			t.Errorf("missing closed line %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "▶") {
		t.Errorf("no focus requested, should not render a you-are-here marker:\n%s", out)
	}
}

func TestRenderEpicContextMixedStatesWithFocus(t *testing.T) {
	f := newEpicFixture(t, "Mixed epic", "# Mixed\nplan context")
	closed := f.addChild("Closed one")
	inProgress := f.addChild("Working one")
	ready := f.addChild("Ready one")
	blocked := f.addChild("Blocked one")

	f.transition(closed, "close")
	f.transition(inProgress, "start")
	f.block(blocked, ready) // blocked depends on the still-open ready child

	out := f.render(ready)

	wantLines := []string{
		"    [closed]      " + closed + "  Closed one",
		"    [in_progress] " + inProgress + "  Working one",
		"  ▶ [ready]       " + ready + "  Ready one   (you are here)",
		"    [blocked-by " + ready + "] " + blocked + "  Blocked one",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing line %q in:\n%s", want, out)
		}
	}

	// Rank order: children appear in creation order.
	if idx(out, closed) > idx(out, inProgress) || idx(out, inProgress) > idx(out, ready) || idx(out, ready) > idx(out, blocked) {
		t.Errorf("children out of rank order:\n%s", out)
	}
}

// addChildLane adds a child in a specific lane so the epic plan can show which
// sub-sequence each child belongs to.
func (f epicFixture) addChildLane(title, lane string) string {
	f.t.Helper()
	child, err := f.ap.Store.CreateIssue(f.ctx, store.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "epic-view", IssueType: "task", Priority: 0, ParentID: f.epicID, Lane: lane,
	})
	if err != nil {
		f.t.Fatalf("CreateIssue(child %q) error = %v", title, err)
	}
	return child.ID
}

func TestRenderEpicContextShowsLaneGrouping(t *testing.T) {
	f := newEpicFixture(t, "Lane epic", "parallel work")
	laneless := f.addChild("Sequential one")
	build := f.addChildLane("Build it", "build")
	docs := f.addChildLane("Document it", "docs")

	out := f.render("")

	// A child with a lane shows its lane tag; the default (empty) lane child
	// renders exactly as before — no tag, so lane-free epics are unchanged.
	if !strings.Contains(out, build+"  Build it  [lane: build]") {
		t.Errorf("missing lane tag for build child in:\n%s", out)
	}
	if !strings.Contains(out, docs+"  Document it  [lane: docs]") {
		t.Errorf("missing lane tag for docs child in:\n%s", out)
	}
	if strings.Contains(out, laneless+"  Sequential one  [lane:") {
		t.Errorf("default-lane child should carry no lane tag, got:\n%s", out)
	}
}

func TestRenderEpicContextChildBlockedBySibling(t *testing.T) {
	f := newEpicFixture(t, "Sibling block", "deps")
	blocker := f.addChild("Blocker sibling")
	blocked := f.addChild("Blocked sibling")
	f.block(blocked, blocker)

	out := f.render("")
	want := "[blocked-by " + blocker + "] " + blocked + "  Blocked sibling"
	if !strings.Contains(out, want) {
		t.Errorf("sibling blocker should be named, want %q in:\n%s", want, out)
	}
}

func TestRenderEpicContextChildBlockedByNonChild(t *testing.T) {
	f := newEpicFixture(t, "External block", "deps")
	blocked := f.addChild("Blocked by outsider")
	// An issue outside this epic (no ParentID).
	outsider, err := f.ap.Store.CreateIssue(f.ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Outsider", Topic: "other", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(outsider) error = %v", err)
	}
	f.block(blocked, outsider.ID)

	out := f.render("")
	want := "[blocked-by " + outsider.ID + "] " + blocked + "  Blocked by outsider"
	if !strings.Contains(out, want) {
		t.Errorf("external blocker should be named inline, want %q in:\n%s", want, out)
	}
	// The outsider is not a child, so it must not appear as its own row.
	if strings.Contains(out, "  "+outsider.ID+"  Outsider") {
		t.Errorf("non-child blocker should not be listed as a child:\n%s", out)
	}
}

// A closed blocker no longer blocks: the dependent renders ready, not blocked.
func TestRenderEpicContextClosedBlockerUnblocks(t *testing.T) {
	f := newEpicFixture(t, "Closed blocker", "deps")
	blocker := f.addChild("Done blocker")
	blocked := f.addChild("Now ready")
	f.block(blocked, blocker)
	f.transition(blocker, "close")

	out := f.render("")
	if !strings.Contains(out, "[ready]       "+blocked+"  Now ready") {
		t.Errorf("dependent should be ready once blocker closed:\n%s", out)
	}
	if strings.Contains(out, "blocked-by") {
		t.Errorf("no open blockers remain, should not render blocked-by:\n%s", out)
	}
}

// outsider creates an issue outside the fixture's epic (no parent) and returns
// its id — the external endpoint for cross-epic edge tests.
func (f epicFixture) outsider(title string) string {
	f.t.Helper()
	out, err := f.ap.Store.CreateIssue(f.ctx, store.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "other", IssueType: "task", Priority: 0,
	})
	if err != nil {
		f.t.Fatalf("CreateIssue(outsider %q) error = %v", title, err)
	}
	return out.ID
}

func TestRenderEpicContextNoCrossEpicEdges(t *testing.T) {
	f := newEpicFixture(t, "No cross edges", "deps")
	a := f.addChild("Child A")
	b := f.addChild("Child B")
	f.block(b, a) // same-epic edge: conveyed by rank, never the cross-epic section

	out := f.render("")
	if strings.Contains(out, "Cross-epic dependencies") {
		t.Errorf("same-epic edge must not surface a cross-epic section:\n%s", out)
	}
}

func TestRenderEpicContextCrossEpicOneDirection(t *testing.T) {
	f := newEpicFixture(t, "One direction", "deps")
	child := f.addChild("Inside")
	ext := f.outsider("Outside")
	f.block(child, ext) // inside depends on outside => "Blocked externally"

	out := f.render("")
	if !strings.Contains(out, "Cross-epic dependencies:") {
		t.Fatalf("expected cross-epic section, got:\n%s", out)
	}
	if !strings.Contains(out, "Blocked externally:\n    "+child+" blocked by "+ext) {
		t.Errorf("expected inbound edge %q blocked by %q in:\n%s", child, ext, out)
	}
	if strings.Contains(out, "Blocks externally:") {
		t.Errorf("no outbound edge exists, that subsection must be omitted:\n%s", out)
	}
}

func TestRenderEpicContextCrossEpicBothDirections(t *testing.T) {
	f := newEpicFixture(t, "Both directions", "deps")
	child := f.addChild("Inside")
	upstream := f.outsider("Upstream")   // inside depends on it
	downstream := f.outsider("Downstream") // it depends on inside
	f.block(child, upstream)
	f.block(downstream, child)

	out := f.render("")
	wantLines := []string{
		"Blocks externally:\n    " + downstream + " blocked by " + child,
		"Blocked externally:\n    " + child + " blocked by " + upstream,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing cross-epic line %q in:\n%s", want, out)
		}
	}
	// Both-directions ordering: "Blocks externally" precedes "Blocked externally".
	if idx(out, "Blocks externally") > idx(out, "Blocked externally") {
		t.Errorf("subsection order wrong:\n%s", out)
	}
}

func TestRenderEpicContextCrossEpicClosedSideFiltered(t *testing.T) {
	f := newEpicFixture(t, "Closed sides", "deps")
	openChild := f.addChild("Open inside")
	closedChild := f.addChild("Closed inside")
	closedExt := f.outsider("Closed outside")
	openExt := f.outsider("Open outside")

	f.block(openChild, closedExt)  // external side closed => filtered
	f.block(closedChild, openExt)  // internal side closed => filtered
	f.transition(closedChild, "close")
	f.transition(closedExt, "close")

	out := f.render("")
	if strings.Contains(out, "Cross-epic dependencies") {
		t.Errorf("every cross-epic edge has a closed endpoint; section must be absent:\n%s", out)
	}
}

func TestRenderEpicContextEdgeToOwnEpicNotCrossEpic(t *testing.T) {
	f := newEpicFixture(t, "Epic self edge", "deps")
	child := f.addChild("Inside")
	f.block(child, f.epicID) // child depends on its own epic: inside the boundary

	out := f.render("")
	if strings.Contains(out, "Cross-epic dependencies") {
		t.Errorf("an edge between a child and its own epic is intra-epic, not cross-epic:\n%s", out)
	}
}

func TestRenderEpicContextEpicNodeCrossEpicEdges(t *testing.T) {
	f := newEpicFixture(t, "Epic-level deps", "deps")
	f.addChild("Some child")
	upstream := f.outsider("Upstream")     // epic depends on it
	downstream := f.outsider("Downstream") // it depends on the epic
	f.block(f.epicID, upstream)
	f.block(downstream, f.epicID)

	out := f.render("")
	wantLines := []string{
		"Blocks externally:\n    " + downstream + " blocked by " + f.epicID,
		"Blocked externally:\n    " + f.epicID + " blocked by " + upstream,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("epic node's own external edges should surface, missing %q in:\n%s", want, out)
		}
	}
}

func TestFirstLineStripsHeadingAndBlanks(t *testing.T) {
	cases := map[string]string{
		"# Heading\nbody":  "Heading",
		"\n\n## Why\nmore": "Why",
		"plain first":      "plain first",
		"   ###  spaced  ": "spaced",
		"":                 "",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func idx(haystack, needle string) int {
	return strings.Index(haystack, needle)
}
