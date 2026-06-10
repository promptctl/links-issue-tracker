package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
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
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic E", Topic: "frame", IssueType: "epic", Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	children := make([]model.Issue, 0, 3)
	for _, title := range []string{"C1", "C2", "C3"} {
		child, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: title, Topic: "frame", IssueType: "task", ParentID: epic.ID, Placement: RankBottom})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		children = append(children, child)
	}
	standalone, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Standalone X", Topic: "frame", IssueType: "task", Placement: RankBottom})
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
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, fx.children)

	resolutions, err := st.RankSet(ctx, []string{fx.standalone.ID, fx.children[1].ID})
	if err != nil {
		t.Fatalf("RankSet(standalone, child) error = %v", err)
	}
	want := []RankSetResolution{
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
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx1 := newFrameFixture(t, ctx, st)
	fx2 := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, append(fx1.children, fx2.children...))

	resolutions, err := st.RankSet(ctx, []string{fx2.children[0].ID, fx1.children[2].ID})
	if err != nil {
		t.Fatalf("RankSet(child2, child1) error = %v", err)
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
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	before := currentRanks(t, ctx, st, []model.Issue{fx.epic, fx.standalone})

	resolutions, err := st.RankSet(ctx, []string{fx.children[2].ID, fx.children[0].ID, fx.children[1].ID})
	if err != nil {
		t.Fatalf("RankSet(siblings) error = %v", err)
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

func TestResolveFrameRepresentatives(t *testing.T) {
	cases := []struct {
		name    string
		chains  [][]string
		want    []string
		wantErr bool
	}{
		{name: "all top-level", chains: [][]string{{"x"}, {"y"}, {"z"}}, want: []string{"x", "y", "z"}},
		{name: "siblings same epic", chains: [][]string{{"c1", "e"}, {"c2", "e"}, {"c3", "e"}}, want: []string{"c1", "c2", "c3"}},
		{name: "children plus outsider resolve to roots", chains: [][]string{{"c1", "e"}, {"x"}, {"c2", "e"}}, want: []string{"e", "x", "e"}},
		{name: "children of two epics under shared parent with outsider", chains: [][]string{{"c1", "e1", "p"}, {"c2", "e2", "p"}, {"x"}}, want: []string{"p", "p", "x"}},
		{name: "children of two epics under shared parent", chains: [][]string{{"c1", "e1", "p"}, {"c2", "e2", "p"}}, want: []string{"e1", "e2"}},
		{name: "grandchild vs child of shared epic", chains: [][]string{{"g", "s", "e"}, {"c", "e"}}, want: []string{"s", "c"}},
		{name: "issue with its own container", chains: [][]string{{"c", "e"}, {"e"}}, wantErr: true},
		{name: "container with deep descendant", chains: [][]string{{"e"}, {"g", "s", "e"}}, wantErr: true},
		{name: "container alongside outsider and descendant", chains: [][]string{{"e"}, {"x"}, {"c", "e"}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reps, err := resolveFrameRepresentatives(tc.chains)
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
		})
	}
}

func TestResolveComparableFrame(t *testing.T) {
	cases := []struct {
		name                    string
		issueChain, targetChain []string
		wantMoved, wantAnchor   string
		wantErr                 bool
	}{
		{name: "both top-level", issueChain: []string{"x"}, targetChain: []string{"y"}, wantMoved: "x", wantAnchor: "y"},
		{name: "siblings same epic", issueChain: []string{"c1", "e"}, targetChain: []string{"c2", "e"}, wantMoved: "c1", wantAnchor: "c2"},
		{name: "standalone vs child", issueChain: []string{"x"}, targetChain: []string{"c", "e"}, wantMoved: "x", wantAnchor: "e"},
		{name: "child vs standalone", issueChain: []string{"c", "e"}, targetChain: []string{"x"}, wantMoved: "e", wantAnchor: "x"},
		{name: "children of two epics", issueChain: []string{"c1", "e1"}, targetChain: []string{"c2", "e2"}, wantMoved: "e1", wantAnchor: "e2"},
		{name: "grandchild vs child of shared epic", issueChain: []string{"g", "s", "e"}, targetChain: []string{"c", "e"}, wantMoved: "s", wantAnchor: "c"},
		{name: "issue inside target", issueChain: []string{"c", "e"}, targetChain: []string{"e"}, wantErr: true},
		{name: "target inside issue", issueChain: []string{"e"}, targetChain: []string{"c", "e"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			moved, anchor, err := resolveComparableFrame(tc.issueChain, tc.targetChain)
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
		})
	}
}
