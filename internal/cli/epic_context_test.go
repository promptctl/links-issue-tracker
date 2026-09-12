package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// epicFixture builds an epic and returns the app plus a helper to add children
// in rank order (creation order = bottom rank, so children stack in call order).
type epicFixture struct {
	t   *testing.T
	ctx context.Context
	ap  *app.App
	// requiredFields is the repo ready-policy the plan slice is built under.
	// Nil — the default — is the no-policy case; a test that sets it is asking
	// the missing-field gate to fire.
	requiredFields []string
	epicID         string
}

func newEpicFixture(t *testing.T, epicTitle, epicDesc string) epicFixture {
	t.Helper()
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: epicTitle, Description: epicDesc, Topic: "epic-view", IssueType: "epic", Priority: 1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	return epicFixture{t: t, ctx: ctx, ap: ap, epicID: epic.ID}
}

func (f epicFixture) addChild(title string) string {
	f.t.Helper()
	child, err := f.ap.Store.CreateIssue(f.ctx, storage.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "epic-view", IssueType: "task", Priority: 0, ParentID: f.epicID,
		// Author children top-to-bottom in call order: append at the bottom so
		// creation order equals rank order. Stated rather than inherited — this
		// fixture's order is its own premise, not a reading of the product
		// default it happens to agree with.
		Placement: storage.RankBottom,
	})
	if err != nil {
		f.t.Fatalf("CreateIssue(child %q) error = %v", title, err)
	}
	return child.ID
}

// transition applies any lifecycle action — the status machine and the retention
// machine are two axes of one lifecycle, and the store applies both through one
// Apply. Taking model.Action rather than one sealed subset keeps the fixture from
// growing a near-identical twin per axis. [LAW:one-type-per-behavior]
func (f epicFixture) transition(id string, action model.Action) {
	f.t.Helper()
	if _, err := f.ap.Store.Apply(f.ctx, id, storage.Change{Action: action, Actor: "test"}); err != nil {
		f.t.Fatalf("transition(%s, %s) error = %v", id, action.Name(), err)
	}
}

// block makes blocked depend on blocker (blocks convention: src=dependent).
func (f epicFixture) block(blocked, blocker string) {
	f.t.Helper()
	if _, err := f.ap.Store.AddRelation(f.ctx, storage.AddRelationInput{SrcID: blocked, DstID: blocker, Type: "blocks", CreatedBy: "test"}); err != nil {
		f.t.Fatalf("AddRelation(blocks %s<-%s) error = %v", blocked, blocker, err)
	}
}

func (f epicFixture) render(focused string) string {
	f.t.Helper()
	ec, err := buildEpicContext(f.ctx, f.ap.Store, f.requiredFields, f.epicID, focused)
	if err != nil {
		f.t.Fatalf("buildEpicContext error = %v", err)
	}
	return renderEpicContext(ec)
}

// A child that has left the flow is labeled with the axis that took it out.
// Rendering a deleted child as "[ready]" invited an agent to start a ticket that
// lit start refuses, and named it as a live blocker of its siblings — the plan
// then disagreed with the readiness gate about the same edge
// (links-readiness-9no1).
func TestRenderEpicContextLabelsFrozenChildrenByRetention(t *testing.T) {
	f := newEpicFixture(t, "Retention epic", "children on both axes")
	deleted := f.addChild("Dropped one")
	archived := f.addChild("Shelved one")
	live := f.addChild("Live one")

	f.transition(deleted, model.Delete{})
	f.transition(archived, model.Archive{})

	out := f.render("")

	wantLines := []string{
		"    [deleted]     " + deleted + "  Dropped one",
		"    [archived]    " + archived + "  Shelved one",
		"    [ready]       " + live + "  Live one",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing line %q in:\n%s", want, out)
		}
	}
}

// The blocked marker and the readiness gate read one predicate, so a blocker
// that has left the flow stops blocking on both surfaces at once. Before the
// fix the dependent rendered "[blocked-by <deleted id>]" naming a ticket absent
// from every listing, with no command able to clear the edge.
func TestRenderEpicContextFrozenBlockerStopsBlocking(t *testing.T) {
	f := newEpicFixture(t, "Blocker epic", "one dead blocker")
	blocker := f.addChild("The blocker")
	dependent := f.addChild("The dependent")
	f.block(dependent, blocker)

	f.transition(blocker, model.Delete{})

	out := f.render("")

	want := "    [ready]       " + dependent + "  The dependent"
	if !strings.Contains(out, want) {
		t.Errorf("missing line %q in:\n%s", want, out)
	}
	if strings.Contains(out, blocker+" still open") || strings.Contains(out, "depends on "+blocker) {
		t.Errorf("deleted blocker %s should not block, got:\n%s", blocker, out)
	}
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
	f.transition(c1, model.Done{})
	f.transition(c2, model.Done{})

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

// Every marker the sum can render, in one epic. Each child sits in its own lane
// so the only thing holding the blocked one back is its declared dependency —
// in the default (empty) lane these four would be one sequential chain and the
// later ones would be held back by the earlier ones, which is its own test
// below.
func TestRenderEpicContextMixedStatesWithFocus(t *testing.T) {
	f := newEpicFixture(t, "Mixed epic", "# Mixed\nplan context")
	closed := f.addChildLane("Closed one", "a")
	inProgress := f.addChildLane("Working one", "b")
	ready := f.addChildLane("Ready one", "c")
	blocked := f.addChildLane("Blocked one", "d")

	f.transition(closed, model.Done{})
	f.transition(inProgress, model.Start{Assignee: "test"})
	f.block(blocked, ready) // blocked depends on the still-open ready child

	out := f.render(ready)

	wantLines := []string{
		"    [closed]      " + closed + "  Closed one  [lane: a]",
		"    [in_progress] " + inProgress + "  Working one  [lane: b]",
		"  ▶ [ready]       " + ready + "  Ready one  [lane: c]   (you are here)",
		"    [blocked: depends on " + ready + "] " + blocked + "  Blocked one  [lane: d]",
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
	child, err := f.ap.Store.CreateIssue(f.ctx, storage.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "epic-view", IssueType: "task", Priority: 0, ParentID: f.epicID, Lane: lane,
		// Same premise as addChild: creation order is rank order, stated here
		// rather than inherited from the product default.
		Placement: storage.RankBottom,
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

// The repro for links-epic-context-oezb: two children in one lane, the first
// still open. The lane gate holds the second back — `lit next` refuses to serve
// it and `lit backlog` prints the reason — so the plan slice calling it [ready]
// was the one surface of the three answering differently.
func TestRenderEpicContextEarlierLaneMateHoldsSiblingBack(t *testing.T) {
	f := newEpicFixture(t, "Sequential epic", "one lane, two children")
	first := f.addChild("First")
	second := f.addChild("Second")

	out := f.render("")

	want := "[blocked: earlier sibling " + first + " still open] " + second + "  Second"
	if !strings.Contains(out, want) {
		t.Errorf("second child is held back by %s; want %q in:\n%s", first, want, out)
	}

	// The other half of the claim: routing, over the same store, serves the
	// first child and not the second. The plan slice and the pick are pinned
	// together here, so a future change that moves one has to move both.
	rows, details, focus, err := gatherWorkableAnnotated(f.ctx, f.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	outcome := routeNext(rows, details, claims.Standings{}, selfAttribution, focus)
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane", outcome, outcome)
	}
	if served.Row.ID != first {
		t.Fatalf("served = %q, want %q — the child the plan slice marks ready", served.Row.ID, first)
	}
}

// Acceptance 2: the ready-policy gate. A child with an empty description under
// a repo that requires one is unservable, and the marker says which field —
// this kind has no id to name, which is why the marker stopped being
// "[blocked-by <id>]"-shaped.
func TestRenderEpicContextMissingRequiredFieldIsNotReady(t *testing.T) {
	f := newEpicFixture(t, "Policy epic", "required fields")
	f.requiredFields = []string{"description"}
	child := f.addChild("No description")

	out := f.render("")

	want := "[blocked: missing description] " + child + "  No description"
	if !strings.Contains(out, want) {
		t.Errorf("child with no description is held back by the ready policy; want %q in:\n%s", want, out)
	}
}

// An epic nested under an epic is a child like any other. It matters because
// the workable pipeline excludes containers by construction (a container owns
// no status of its own), so routing the plan slice through that pipeline's
// annotators put a container in front of them for the first time: the field
// annotator marshals it, the orphan annotator reads its derived state, the lane
// gate asks for its lane. This pins that the whole set tolerates one, rather
// than the plan slice failing on an epic shape it used to render.
func TestRenderEpicContextNestedEpicChildIsClassified(t *testing.T) {
	f := newEpicFixture(t, "Outer epic", "an epic under an epic")
	f.requiredFields = []string{"description"}
	inner, err := f.ap.Store.CreateIssue(f.ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Inner epic", Topic: "epic-view", IssueType: "epic", Priority: 0,
		ParentID: f.epicID, Placement: storage.RankBottom,
	})
	if err != nil {
		t.Fatalf("CreateIssue(inner epic) error = %v", err)
	}

	out := f.render("")

	if !strings.Contains(out, inner.ID+"  Inner epic") {
		t.Errorf("a nested epic should render as a child row, got:\n%s", out)
	}
	// It has no description, and the fixture requires one, so the ready policy
	// reaches a container exactly as it reaches a leaf.
	if !strings.Contains(out, "[blocked: missing description] "+inner.ID) {
		t.Errorf("nested epic should carry the ready-policy reason, got:\n%s", out)
	}
}

// Acceptance 3, and the gate that keeps this bug from returning in a fifth
// kind's clothing: EVERY kind the registry classifies as blocking must move a
// child off [ready] and reach the reader as words. Driven off the registry
// rather than a list maintained beside it — the counterpart of
// TestBacklogPhrasesEveryBlockingKind, which pins the same property for the
// backlog's "blocked:" line.
//
// This asserts the classifier is total over the verdict; that the verdict
// reaching it is the FULL one is what the two tests above cover, each through
// a kind the old edge-walking derivation could not see.
// [LAW:one-source-of-truth] [LAW:verifiable-goals]
func TestEpicContextMarkerReflectsEveryBlockingKind(t *testing.T) {
	t.Parallel()
	child, err := model.HydrateStatus(model.Issue{ID: "test-1", IssueType: model.TypeTask}, model.StatusView{Value: model.StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus error = %v", err)
	}
	readyMarker := statusReady{}.marker()
	for _, kind := range annotation.Kinds() {
		if kind.ReadinessRole() != annotation.RoleBlocking {
			continue
		}
		t.Run(kind.String(), func(t *testing.T) {
			readiness := ClassifyReadiness([]annotation.Annotation{{Kind: kind, Message: "test-detail"}})
			marker := classifyChildStatus(child, readiness).marker()
			if marker == readyMarker {
				t.Fatalf("kind %s left the child marked %s — a child the gate holds back must never read as startable", kind, marker)
			}
			if !strings.HasPrefix(marker, "[blocked: ") || marker == "[blocked: ]" {
				t.Errorf("kind %s rendered %q, want a blocked marker carrying a reason", kind, marker)
			}
		})
	}
}

// A declared edge onto an earlier lane-mate is two prerequisites at once, and
// the marker names both: closing the sibling discharges them together, but a
// marker that mentioned only the edge would leave a reader who re-laned the
// child expecting it to come free.
func TestRenderEpicContextChildBlockedBySibling(t *testing.T) {
	f := newEpicFixture(t, "Sibling block", "deps")
	blocker := f.addChild("Blocker sibling")
	blocked := f.addChild("Blocked sibling")
	f.block(blocked, blocker)

	out := f.render("")
	want := "[blocked: depends on " + blocker + "; earlier sibling " + blocker + " still open] " + blocked + "  Blocked sibling"
	if !strings.Contains(out, want) {
		t.Errorf("sibling blocker should be named, want %q in:\n%s", want, out)
	}
}

func TestRenderEpicContextChildBlockedByNonChild(t *testing.T) {
	f := newEpicFixture(t, "External block", "deps")
	blocked := f.addChild("Blocked by outsider")
	// An issue outside this epic (no ParentID).
	outsider, err := f.ap.Store.CreateIssue(f.ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Outsider", Topic: "other", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(outsider) error = %v", err)
	}
	f.block(blocked, outsider.ID)

	out := f.render("")
	want := "[blocked: depends on " + outsider.ID + "] " + blocked + "  Blocked by outsider"
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
	f.transition(blocker, model.Done{})

	out := f.render("")
	if !strings.Contains(out, "[ready]       "+blocked+"  Now ready") {
		t.Errorf("dependent should be ready once blocker closed:\n%s", out)
	}
	if strings.Contains(out, "[blocked:") {
		t.Errorf("no open blockers remain, should not render a blocked marker:\n%s", out)
	}
}

// outsider creates an issue outside the fixture's epic (no parent) and returns
// its id — the external endpoint for cross-epic edge tests.
func (f epicFixture) outsider(title string) string {
	f.t.Helper()
	out, err := f.ap.Store.CreateIssue(f.ctx, storage.CreateIssueInput{
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
	upstream := f.outsider("Upstream")     // inside depends on it
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

	f.block(openChild, closedExt) // external side closed => filtered
	f.block(closedChild, openExt) // internal side closed => filtered
	f.transition(closedChild, model.Done{})
	f.transition(closedExt, model.Done{})

	out := f.render("")
	if strings.Contains(out, "Cross-epic dependencies") {
		t.Errorf("every cross-epic edge has a closed endpoint; section must be absent:\n%s", out)
	}
}

// A frozen endpoint drops a cross-epic edge exactly as a closed one does. This
// is the seam the closed-side test above cannot reach: `collect`'s member test
// and `inPlayExcluding`'s counterpart filter both moved from "not closed" to
// InPlay in links-readiness-9no1, and closed is the arm that already passed
// before that change — so only a frozen endpoint can catch a regression.
// Deleted sits on the internal side and archived on the external side, which
// puts both predicates under one assertion.
func TestRenderEpicContextCrossEpicFrozenSideFiltered(t *testing.T) {
	f := newEpicFixture(t, "Frozen sides", "deps")
	openChild := f.addChild("Open inside")
	deletedChild := f.addChild("Deleted inside")
	archivedExt := f.outsider("Archived outside")
	openExt := f.outsider("Open outside")

	f.block(openChild, archivedExt) // external side archived => filtered
	f.block(deletedChild, openExt)  // internal side deleted => filtered
	f.transition(deletedChild, model.Delete{})
	f.transition(archivedExt, model.Archive{})

	out := f.render("")
	if strings.Contains(out, "Cross-epic dependencies") {
		t.Errorf("every cross-epic edge has a frozen endpoint; section must be absent:\n%s", out)
	}
}

// The mirror of the test above: a cross-epic edge between two live endpoints
// still renders, so the frozen filter is proved to drop edges for the stated
// reason rather than the section being absent for some unrelated one.
func TestRenderEpicContextCrossEpicLiveSidesRender(t *testing.T) {
	f := newEpicFixture(t, "Live sides", "deps")
	child := f.addChild("Open inside")
	ext := f.outsider("Open outside")
	f.block(child, ext)

	out := f.render("")
	if !strings.Contains(out, "Cross-epic dependencies") {
		t.Errorf("both endpoints live; the section must render:\n%s", out)
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
	t.Parallel()
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
