package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// frameFixture builds the canonical cross-frame scenario: epic E with three
// ranked children, plus a standalone task ranked below everything.
type frameFixture struct {
	epic       model.Issue
	children   []model.Issue
	standalone model.Issue
}

func newFrameFixture(t *testing.T, ctx context.Context, st *Store) frameFixture {
	t.Helper()
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic E", Topic: "frame", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	children := make([]model.Issue, 0, 3)
	for _, title := range []string{"C1", "C2", "C3"} {
		child, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "frame", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		children = append(children, child)
	}
	standalone, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone X", Topic: "frame", IssueType: "task", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(standalone) error = %v", err)
	}
	return frameFixture{epic: epic, children: children, standalone: standalone}
}

func currentRanks(t *testing.T, ctx context.Context, st *Store, issues []model.Issue) map[string]string {
	t.Helper()
	out := make(map[string]string, len(issues))
	for _, issue := range issues {
		got, err := st.GetIssue(ctx, issue.ID)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", issue.ID, err)
		}
		out[issue.ID] = got.Rank
	}
	return out
}

func TestRankAboveStandaloneAgainstEpicChildAnchorsToEpic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, append(fx.children, fx.epic))

	move, err := st.RankAbove(ctx, fx.standalone.ID, fx.children[1].ID)
	if err != nil {
		t.Fatalf("RankAbove(standalone, child) error = %v", err)
	}
	if move.MovedID != fx.standalone.ID || move.AnchorID != fx.epic.ID {
		t.Fatalf("move = %+v, want moved=%s anchor=%s", move, fx.standalone.ID, fx.epic.ID)
	}

	after := currentRanks(t, ctx, st, append([]model.Issue{fx.standalone, fx.epic}, fx.children...))
	for _, issue := range append(fx.children, fx.epic) {
		if after[issue.ID] != before[issue.ID] {
			t.Errorf("issue %s rank changed %q -> %q; epic and children must not move", issue.ID, before[issue.ID], after[issue.ID])
		}
	}
	if after[fx.standalone.ID] >= after[fx.epic.ID] {
		t.Errorf("standalone rank %q not above epic rank %q", after[fx.standalone.ID], after[fx.epic.ID])
	}
}

func TestRankAboveChildAgainstStandaloneMovesEpic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, append(fx.children, fx.standalone))

	move, err := st.RankAbove(ctx, fx.children[1].ID, fx.standalone.ID)
	if err != nil {
		t.Fatalf("RankAbove(child, standalone) error = %v", err)
	}
	if move.MovedID != fx.epic.ID || move.AnchorID != fx.standalone.ID {
		t.Fatalf("move = %+v, want moved=%s anchor=%s", move, fx.epic.ID, fx.standalone.ID)
	}

	after := currentRanks(t, ctx, st, append([]model.Issue{fx.standalone, fx.epic}, fx.children...))
	for _, issue := range append(fx.children, fx.standalone) {
		if after[issue.ID] != before[issue.ID] {
			t.Errorf("issue %s rank changed %q -> %q; children and anchor must not move", issue.ID, before[issue.ID], after[issue.ID])
		}
	}
	if after[fx.epic.ID] >= after[fx.standalone.ID] {
		t.Errorf("epic rank %q not above standalone rank %q", after[fx.epic.ID], after[fx.standalone.ID])
	}
}

func TestRankBelowAcrossTwoEpicsMovesBothRepresentatives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx1 := newFrameFixture(t, ctx, st)
	fx2 := newFrameFixture(t, ctx, st)

	move, err := st.RankBelow(ctx, fx1.children[0].ID, fx2.children[2].ID)
	if err != nil {
		t.Fatalf("RankBelow(child1, child2) error = %v", err)
	}
	if move.MovedID != fx1.epic.ID || move.AnchorID != fx2.epic.ID {
		t.Fatalf("move = %+v, want moved=%s anchor=%s", move, fx1.epic.ID, fx2.epic.ID)
	}
	after := currentRanks(t, ctx, st, []model.Issue{fx1.epic, fx2.epic})
	if after[fx1.epic.ID] <= after[fx2.epic.ID] {
		t.Errorf("epic1 rank %q not below epic2 rank %q", after[fx1.epic.ID], after[fx2.epic.ID])
	}
}

func TestRankAboveSameEpicSiblingsRanksTheSiblings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)

	move, err := st.RankAbove(ctx, fx.children[2].ID, fx.children[0].ID)
	if err != nil {
		t.Fatalf("RankAbove(C3, C1) error = %v", err)
	}
	if move.MovedID != fx.children[2].ID || move.AnchorID != fx.children[0].ID {
		t.Fatalf("move = %+v, want moved=%s anchor=%s (siblings rank directly)", move, fx.children[2].ID, fx.children[0].ID)
	}
	after := currentRanks(t, ctx, st, fx.children)
	if after[fx.children[2].ID] >= after[fx.children[0].ID] {
		t.Errorf("C3 rank %q not above C1 rank %q", after[fx.children[2].ID], after[fx.children[0].ID])
	}
}

func TestRankAgainstOwnContainerErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)

	if _, err := st.RankAbove(ctx, fx.children[0].ID, fx.epic.ID); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Errorf("RankAbove(child, own epic) error = %v, want containment rejection", err)
	}
	if _, err := st.RankBelow(ctx, fx.epic.ID, fx.children[0].ID); err == nil || !strings.Contains(err.Error(), "contains") {
		t.Errorf("RankBelow(epic, own child) error = %v, want containment rejection", err)
	}
}

func TestRankSetMixedFrameResolvesChildToEpic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, fx.children)

	result, err := st.RankSet(ctx, []string{fx.standalone.ID, fx.children[1].ID})
	if err != nil {
		t.Fatalf("RankSet(standalone, child) error = %v", err)
	}
	resolutions := result.Resolutions
	// A standalone ranked against an epic's child resolves to the epic, so the
	// stack lands at the top level, not inside the epic.
	if result.Frame != storage.TopLevel {
		t.Errorf("RankSet frame = %q, want the top level", result.Frame)
	}
	want := []storage.RankSetResolution{
		{NamedID: fx.standalone.ID, RankedID: fx.standalone.ID},
		{NamedID: fx.children[1].ID, RankedID: fx.epic.ID},
	}
	if len(resolutions) != 2 || resolutions[0] != want[0] || resolutions[1] != want[1] {
		t.Fatalf("resolutions = %+v, want %+v", resolutions, want)
	}

	after := currentRanks(t, ctx, st, append([]model.Issue{fx.standalone, fx.epic}, fx.children...))
	for _, child := range fx.children {
		if after[child.ID] != before[child.ID] {
			t.Errorf("child %s rank changed %q -> %q; children must not move", child.ID, before[child.ID], after[child.ID])
		}
	}
	if after[fx.standalone.ID] >= after[fx.epic.ID] {
		t.Errorf("standalone rank %q not above epic rank %q", after[fx.standalone.ID], after[fx.epic.ID])
	}
}

func TestRankSetAcrossTwoEpicsRanksRepresentatives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx1 := newFrameFixture(t, ctx, st)
	fx2 := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, append(fx1.children, fx2.children...))

	result, err := st.RankSet(ctx, []string{fx2.children[0].ID, fx1.children[2].ID})
	if err != nil {
		t.Fatalf("RankSet(child2, child1) error = %v", err)
	}
	resolutions := result.Resolutions
	if result.Frame != storage.TopLevel {
		t.Errorf("RankSet frame = %q, want the top level the two epics share", result.Frame)
	}
	if resolutions[0].RankedID != fx2.epic.ID || resolutions[1].RankedID != fx1.epic.ID {
		t.Fatalf("resolutions = %+v, want ranked %s then %s", resolutions, fx2.epic.ID, fx1.epic.ID)
	}

	after := currentRanks(t, ctx, st, append(append(fx1.children, fx2.children...), fx1.epic, fx2.epic))
	for _, child := range append(fx1.children, fx2.children...) {
		if after[child.ID] != before[child.ID] {
			t.Errorf("child %s rank changed %q -> %q; children must not move", child.ID, before[child.ID], after[child.ID])
		}
	}
	if after[fx2.epic.ID] >= after[fx1.epic.ID] {
		t.Errorf("epic2 rank %q not above epic1 rank %q", after[fx2.epic.ID], after[fx1.epic.ID])
	}
}

func TestRankSetSameEpicSiblingsRanksSiblingsDirectly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, []model.Issue{fx.epic, fx.standalone})

	result, err := st.RankSet(ctx, []string{fx.children[2].ID, fx.children[0].ID, fx.children[1].ID})
	if err != nil {
		t.Fatalf("RankSet(siblings) error = %v", err)
	}
	resolutions := result.Resolutions
	// Siblings rank inside their epic, and the frame says so. This is the value
	// the CLI names in its summary: "at the top of <epic>" rather than a bare
	// "at top", which would read as the head of the backlog.
	if result.Frame != storage.Frame(fx.epic.ID) {
		t.Errorf("RankSet frame = %q, want the epic %s the siblings live in", result.Frame, fx.epic.ID)
	}
	for _, r := range resolutions {
		if r.NamedID != r.RankedID {
			t.Errorf("resolution %+v substituted; same-frame siblings rank directly", r)
		}
	}

	after := currentRanks(t, ctx, st, append([]model.Issue{fx.epic, fx.standalone}, fx.children...))
	for _, issue := range []model.Issue{fx.epic, fx.standalone} {
		if after[issue.ID] != before[issue.ID] {
			t.Errorf("issue %s rank changed %q -> %q; only the named siblings move", issue.ID, before[issue.ID], after[issue.ID])
		}
	}
	if !(after[fx.children[2].ID] < after[fx.children[0].ID] && after[fx.children[0].ID] < after[fx.children[1].ID]) {
		t.Errorf("sibling order = C3:%q C1:%q C2:%q, want C3 < C1 < C2", after[fx.children[2].ID], after[fx.children[0].ID], after[fx.children[1].ID])
	}
}

func TestRankSetDuplicateRepresentativesRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, append([]model.Issue{fx.epic, fx.standalone}, fx.children...))

	_, err := st.RankSet(ctx, []string{fx.children[0].ID, fx.standalone.ID, fx.children[1].ID})
	if err == nil || !strings.Contains(err.Error(), "both resolve to") {
		t.Fatalf("RankSet(C1, X, C2) error = %v, want duplicate-representative rejection", err)
	}

	after := currentRanks(t, ctx, st, append([]model.Issue{fx.epic, fx.standalone}, fx.children...))
	for id, rank := range before {
		if after[id] != rank {
			t.Errorf("issue %s rank changed %q -> %q on rejected rank set", id, rank, after[id])
		}
	}
}

func TestRankSetWithOwnContainerRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)

	if _, err := st.RankSet(ctx, []string{fx.epic.ID, fx.children[0].ID}); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Errorf("RankSet(epic, own child) error = %v, want containment rejection", err)
	}
	if _, err := st.RankSet(ctx, []string{fx.children[0].ID, fx.epic.ID}); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Errorf("RankSet(child, own epic) error = %v, want containment rejection", err)
	}
}

// TestRankToEdgeDrawsItsKeyFromItsOwnFrame asserts the rank STRINGS, not the
// rendered order. The order can be right while the keyspace is already
// contaminated — a child holding a key below every top-level row still lists
// among its siblings correctly — and it is the keyspace, not the order, that
// decides what the NEXT rank is computed against. An order-only case would
// have passed throughout the defect this test exists for.
func TestRankToEdgeDrawsItsKeyFromItsOwnFrame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	all := append([]model.Issue{fx.epic, fx.standalone}, fx.children...)
	before := currentRanks(t, ctx, st, all)

	end, err := st.RankToTop(ctx, fx.children[2].ID)
	if err != nil {
		t.Fatalf("RankToTop(C3) error = %v", err)
	}
	if end.Frame != storage.Frame(fx.epic.ID) {
		t.Fatalf("RankToTop(C3) frame = %q, want the epic %q", end.Frame, fx.epic.ID)
	}
	if !end.Moved {
		t.Fatal("RankToTop(C3) reported no move; C3 was last among its siblings")
	}

	after := currentRanks(t, ctx, st, all)
	// C3's key is seeded from C1's — the key that led C3's own frame — and from
	// nothing else.
	if want := rank.Before(before[fx.children[0].ID]); after[fx.children[2].ID] != want {
		t.Errorf("C3 rank = %q, want %q = Before(C1 %q), its frame's leading key", after[fx.children[2].ID], want, before[fx.children[0].ID])
	}
	for _, issue := range []model.Issue{fx.epic, fx.standalone, fx.children[0], fx.children[1]} {
		if after[issue.ID] != before[issue.ID] {
			t.Errorf("issue %s rank changed %q -> %q; only C3 moves", issue.ID, before[issue.ID], after[issue.ID])
		}
	}

	// The precondition that gives the rest of this case its teeth: C3's new key
	// now sorts below every top-level key, so an unscoped "first rank" query
	// WOULD return it. Without this the assertions below pass vacuously — which
	// is the shape of an invariant test that proves nothing.
	if after[fx.children[2].ID] >= after[fx.epic.ID] {
		t.Fatalf("C3 rank %q does not sort below the top-level leader %q; this case cannot tell a scoped query from an unscoped one", after[fx.children[2].ID], after[fx.epic.ID])
	}
	if after[fx.epic.ID] >= after[fx.standalone.ID] {
		t.Fatalf("epic rank %q is not the top-level leader (standalone %q); the fixture no longer sets up this case", after[fx.epic.ID], after[fx.standalone.ID])
	}

	// A top-level --top now: its key must come from the top-level leader, never
	// from the child that happens to hold a smaller string.
	if _, err := st.RankToTop(ctx, fx.standalone.ID); err != nil {
		t.Fatalf("RankToTop(standalone) error = %v", err)
	}
	final := currentRanks(t, ctx, st, all)
	if want := rank.Before(after[fx.epic.ID]); final[fx.standalone.ID] != want {
		t.Errorf("standalone rank = %q, want %q = Before(the top-level leader %q)", final[fx.standalone.ID], want, after[fx.epic.ID])
	}
	if contaminated := rank.Before(after[fx.children[2].ID]); final[fx.standalone.ID] == contaminated {
		t.Errorf("standalone rank = %q was computed against C3 %q, a key in another frame", final[fx.standalone.ID], after[fx.children[2].ID])
	}

	// The bottom edge is scoped the same way: C3 back to the bottom of its
	// frame is seeded from C2, the key that trails its siblings — not from the
	// workspace's last key.
	if _, err := st.RankToBottom(ctx, fx.children[2].ID); err != nil {
		t.Fatalf("RankToBottom(C3) error = %v", err)
	}
	bottom := currentRanks(t, ctx, st, all)
	if want := rank.After(final[fx.children[1].ID]); bottom[fx.children[2].ID] != want {
		t.Errorf("C3 rank = %q, want %q = After(C2 %q), its frame's trailing key", bottom[fx.children[2].ID], want, final[fx.children[1].ID])
	}
}

// TestRankToEdgeReportsAnUnmovableIssue covers the outcome that looks exactly
// like success: the issue already holds the edge, so nothing is written and the
// caller has to be told, or it reads an unchanged order as a promotion.
func TestRankToEdgeReportsAnUnmovableIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	all := append([]model.Issue{fx.epic, fx.standalone}, fx.children...)
	before := currentRanks(t, ctx, st, all)

	// C1 already leads its siblings, and the epic already leads the top level.
	for _, tc := range []struct {
		name      string
		id        string
		wantFrame storage.Frame
	}{
		{"leading child", fx.children[0].ID, storage.Frame(fx.epic.ID)},
		{"leading top-level issue", fx.epic.ID, storage.TopLevel},
	} {
		end, err := st.RankToTop(ctx, tc.id)
		if err != nil {
			t.Fatalf("RankToTop(%s) error = %v", tc.name, err)
		}
		if end.Moved {
			t.Errorf("RankToTop(%s) reported a move; it already held the edge", tc.name)
		}
		if end.Frame != tc.wantFrame {
			t.Errorf("RankToTop(%s) frame = %q, want %q", tc.name, end.Frame, tc.wantFrame)
		}
	}

	// A no-op writes nothing at all — not the same rank back, which would bump
	// updated_at and land a commit for a move that did not happen.
	after := currentRanks(t, ctx, st, all)
	for id, r := range before {
		if after[id] != r {
			t.Errorf("issue %s rank changed %q -> %q on a no-op rank", id, r, after[id])
		}
	}
}

func TestResolveFrameRepresentatives(t *testing.T) {
	t.Parallel()
	// wantFrame is asserted alongside the representatives because the frame is
	// what every neighbor lookup seeded by them gets scoped to: representatives
	// that are right in a frame that is wrong still write a key into the wrong
	// keyspace.
	cases := []struct {
		name      string
		chains    [][]string
		want      []string
		wantFrame storage.Frame
		wantErr   bool
	}{
		{name: "all top-level", chains: [][]string{{"x"}, {"y"}, {"z"}}, want: []string{"x", "y", "z"}, wantFrame: storage.TopLevel},
		{name: "siblings same epic", chains: [][]string{{"c1", "e"}, {"c2", "e"}, {"c3", "e"}}, want: []string{"c1", "c2", "c3"}, wantFrame: "e"},
		{name: "children plus outsider resolve to roots", chains: [][]string{{"c1", "e"}, {"x"}, {"c2", "e"}}, want: []string{"e", "x", "e"}, wantFrame: storage.TopLevel},
		{name: "children of two epics under shared parent with outsider", chains: [][]string{{"c1", "e1", "p"}, {"c2", "e2", "p"}, {"x"}}, want: []string{"p", "p", "x"}, wantFrame: storage.TopLevel},
		{name: "children of two epics under shared parent", chains: [][]string{{"c1", "e1", "p"}, {"c2", "e2", "p"}}, want: []string{"e1", "e2"}, wantFrame: "p"},
		{name: "grandchild vs child of shared epic", chains: [][]string{{"g", "s", "e"}, {"c", "e"}}, want: []string{"s", "c"}, wantFrame: "e"},
		{name: "issue with its own container", chains: [][]string{{"c", "e"}, {"e"}}, wantErr: true},
		{name: "container with deep descendant", chains: [][]string{{"e"}, {"g", "s", "e"}}, wantErr: true},
		{name: "container alongside outsider and descendant", chains: [][]string{{"e"}, {"x"}, {"c", "e"}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reps, frame, err := resolveFrameRepresentatives(tc.chains)
			if tc.wantErr {
				var containment *frameContainmentError
				if !errors.As(err, &containment) {
					t.Fatalf("resolveFrameRepresentatives() error = %v, want frameContainmentError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveFrameRepresentatives() error = %v", err)
			}
			if len(reps) != len(tc.want) {
				t.Fatalf("resolveFrameRepresentatives() = %v, want %v", reps, tc.want)
			}
			for i := range reps {
				if reps[i] != tc.want[i] {
					t.Fatalf("resolveFrameRepresentatives() = %v, want %v", reps, tc.want)
				}
			}
			if frame != tc.wantFrame {
				t.Fatalf("resolveFrameRepresentatives() frame = %q, want %q", frame, tc.wantFrame)
			}
			// Every representative is a member of the frame it came back with,
			// which is the property that makes scoping a query by that frame
			// find exactly the rows these keys are compared against.
			for i, chain := range tc.chains {
				container := storage.TopLevel
				if idx := slices.Index(chain, reps[i]); idx >= 0 && idx+1 < len(chain) {
					container = storage.Frame(chain[idx+1])
				}
				if container != frame {
					t.Fatalf("representative %s sits in frame %q, not the reported %q", reps[i], container, frame)
				}
			}
		})
	}
}

func TestResolveComparableFrame(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                    string
		issueChain, targetChain []string
		wantMoved, wantAnchor   string
		wantFrame               storage.Frame
		wantErr                 bool
	}{
		{name: "both top-level", issueChain: []string{"x"}, targetChain: []string{"y"}, wantMoved: "x", wantAnchor: "y", wantFrame: storage.TopLevel},
		{name: "siblings same epic", issueChain: []string{"c1", "e"}, targetChain: []string{"c2", "e"}, wantMoved: "c1", wantAnchor: "c2", wantFrame: "e"},
		{name: "standalone vs child", issueChain: []string{"x"}, targetChain: []string{"c", "e"}, wantMoved: "x", wantAnchor: "e", wantFrame: storage.TopLevel},
		{name: "child vs standalone", issueChain: []string{"c", "e"}, targetChain: []string{"x"}, wantMoved: "e", wantAnchor: "x", wantFrame: storage.TopLevel},
		{name: "children of two epics", issueChain: []string{"c1", "e1"}, targetChain: []string{"c2", "e2"}, wantMoved: "e1", wantAnchor: "e2", wantFrame: storage.TopLevel},
		{name: "grandchild vs child of shared epic", issueChain: []string{"g", "s", "e"}, targetChain: []string{"c", "e"}, wantMoved: "s", wantAnchor: "c", wantFrame: "e"},
		{name: "issue inside target", issueChain: []string{"c", "e"}, targetChain: []string{"e"}, wantErr: true},
		{name: "target inside issue", issueChain: []string{"e"}, targetChain: []string{"c", "e"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			moved, anchor, frame, err := resolveComparableFrame(tc.issueChain, tc.targetChain)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveComparableFrame() error = nil, want containment error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveComparableFrame() error = %v", err)
			}
			if moved != tc.wantMoved || anchor != tc.wantAnchor {
				t.Fatalf("resolveComparableFrame() = (%s, %s), want (%s, %s)", moved, anchor, tc.wantMoved, tc.wantAnchor)
			}
			if frame != tc.wantFrame {
				t.Fatalf("resolveComparableFrame() frame = %q, want %q", frame, tc.wantFrame)
			}
		})
	}
}

// TestWriteRankRefusesAnIssueDeletedUnderTheLock pins the guarantee that
// mustRankable cannot give on its own. That gate runs before the commit lock, so
// its answer is only as fresh as the instant it was read, and a delete landing
// between it and the write would otherwise leave a key on a row no listing
// shows.
//
// The race has no seam to stage — there is no point between the gate and the
// write where a test can land a concurrent delete — so this asserts the property
// the race depends on, directly against the statement that would have carried it
// out. Deleting the row before the transaction opens produces exactly the state
// such a delete produces: a row the pre-lock gate would have passed, gone by the
// time the key is written.
//
// White-box on purpose. Every public rank verb refuses this issue at the gate and
// so can never reach writeRankTx with a deleted row, which is what made this
// second line of defence untestable from outside the package — and what let it
// be missing from every write site unnoticed.
func TestWriteRankRefusesAnIssueDeletedUnderTheLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Doomed", Topic: "frame", IssueType: "task", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue error = %v", err)
	}
	before, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue(before) error = %v", err)
	}
	if err := st.ExecRawForTest(ctx, `UPDATE issues SET deleted_at = ? WHERE id = ?`, "2026-09-11T00:00:00Z", issue.ID); err != nil {
		t.Fatalf("soft-delete %s: %v", issue.ID, err)
	}

	var writeErr error
	if err := st.withMutation(ctx, "write-rank-under-lock-test", func(ctx context.Context, tx *sql.Tx) error {
		writeErr = writeRankTx(ctx, tx, issue.ID, "zzzz", "2026-09-11T00:00:01Z")
		return nil
	}); err != nil {
		t.Fatalf("withMutation error = %v", err)
	}
	if writeErr == nil {
		t.Fatalf("writeRankTx put a key on the deleted %s; want a refusal", issue.ID)
	}
	if !strings.Contains(writeErr.Error(), "deleted while the move was being applied") {
		t.Errorf("writeRankTx error = %q, want it to name the mid-flight deletion", writeErr)
	}

	// The refusal has to be a refusal, not a complaint after the fact. GetIssue
	// carries no deleted_at filter, so the trashed row is still readable and its
	// key can be compared directly.
	after, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue(after) error = %v", err)
	}
	if after.Rank != before.Rank {
		t.Errorf("a refused write still moved the deleted %s: rank %q -> %q", issue.ID, before.Rank, after.Rank)
	}
}

// TestFrameResolutionRefusesADeletedNamedIssue covers the case writeRankTx
// cannot: an id that frame resolution substitutes away.
//
// writeRankTx re-reads the row it is about to write, which closes the race for
// every id that reaches the write. A named id does not always reach it. Naming
// a child of an epic beside a top-level issue resolves the child to its epic,
// and from there the epic is what every later check sees — so a delete of the
// child landing after the pre-lock gate went unnoticed, and the set proceeded to
// rank the epic on behalf of an issue that no longer existed. The refusal the
// CHANGELOG promises for "every form of the command" was not the refusal the
// substitution path gave.
//
// White-box for the same reason as the write-side case above: the public verb
// refuses this id at the gate and so can never reach resolution with it.
func TestFrameResolutionRefusesADeletedNamedIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	child := fx.children[0]

	if err := st.ExecRawForTest(ctx, `UPDATE issues SET deleted_at = ? WHERE id = ?`, "2026-09-11T00:00:00Z", child.ID); err != nil {
		t.Fatalf("soft-delete %s: %v", child.ID, err)
	}

	// The child resolves to its epic, which is live and untouched — which is
	// exactly why nothing downstream would have caught the deletion.
	var resolveErr error
	if err := st.withMutation(ctx, "frame-resolution-liveness-test", func(ctx context.Context, tx *sql.Tx) error {
		_, _, resolveErr = resolveRankSet(ctx, tx, []string{child.ID, fx.standalone.ID})
		return nil
	}); err != nil {
		t.Fatalf("withMutation error = %v", err)
	}
	if resolveErr == nil {
		t.Fatalf("resolveRankSet accepted the deleted %s by substituting its epic %s; want a refusal", child.ID, fx.epic.ID)
	}
	if !strings.Contains(resolveErr.Error(), "deleted while the move was being applied") {
		t.Errorf("resolveRankSet error = %q, want it to name the mid-flight deletion", resolveErr)
	}

	// The same gap on the relative verbs, which resolve through the same walk.
	var pairErr error
	if err := st.withMutation(ctx, "frame-resolution-liveness-test-pair", func(ctx context.Context, tx *sql.Tx) error {
		_, _, _, pairErr = rankPairTx(ctx, tx, child.ID, fx.standalone.ID)
		return nil
	}); err != nil {
		t.Fatalf("withMutation error = %v", err)
	}
	if pairErr == nil {
		t.Fatalf("rankPairTx accepted the deleted %s by substituting its epic %s; want a refusal", child.ID, fx.epic.ID)
	}
}
