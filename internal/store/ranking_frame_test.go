package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// frameFixture builds the canonical cross-frame scenario: epic E with three
// ranked children, plus a standalone epic ranked below everything. The
// standalone is an epic because the top level holds only epics: a leaf always
// lives inside one (storage.RejectLeafWithoutParent).
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
	standalone, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone X", Topic: "frame", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(standalone) error = %v", err)
	}
	return frameFixture{epic: epic, children: children, standalone: standalone}
}

// createTopLevelIssue files one issue at the bottom of the top-level frame and
// returns its id. It is an epic because that is the only thing the top level
// holds; the cases that use it are about keys in a frame, not about types.
func createTopLevelIssue(t *testing.T, ctx context.Context, st *Store, title string) string {
	t.Helper()
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "rank", IssueType: "epic", Priority: 0, Placement: storage.RankBottom,
	})
	if err != nil {
		t.Fatalf("CreateIssue(%s) error = %v", title, err)
	}
	return issue.ID
}

// makeTopLevelIssues files n issues at the bottom of the top-level frame, in
// order, and returns their ids. Epics, for the reason createTopLevelIssue is.
func makeTopLevelIssues(t *testing.T, ctx context.Context, st *Store, n int, titlef string) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		ids = append(ids, createTopLevelIssue(t, ctx, st, fmt.Sprintf(titlef, i)))
	}
	return ids
}

// makeChildrenOf files one leaf per title under epicID, each in its own lane,
// and returns them in filing order.
func makeChildrenOf(t *testing.T, ctx context.Context, st *Store, epicID string, titles ...string) []model.Issue {
	t.Helper()
	issues := make([]model.Issue, 0, len(titles))
	for _, title := range titles {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "frame", IssueType: "task", ParentID: epicID, Lane: title})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		issues = append(issues, issue)
	}
	return issues
}

// openingKey is the key an empty workspace's first issue is written: the
// midpoint of the whole keyspace.
func openingKey(t *testing.T) string {
	t.Helper()
	key, err := rank.Midpoint("", "")
	if err != nil {
		t.Fatalf("rank.Midpoint of the whole keyspace error = %v", err)
	}
	return key
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

func mustBefore(t *testing.T, r string) string {
	t.Helper()
	before, err := rank.Midpoint("", r)
	if err != nil {
		t.Fatalf("rank.Midpoint(\"\", %q) error = %v", r, err)
	}
	return before
}

// mustTopOf is the key a placement at a frame's top must take when anchor is
// the key leading that frame: the midpoint between anchor and the nearest key
// the WHOLE workspace holds below it.
//
// Both halves are the rule under test. Which key the placement lands beside is
// a question about the frame, so anchor is what an assertion varies to tell a
// scoped query from an unscoped one. How much room there is beside it is a
// question about the keyspace every frame shares, so the bound is found by
// scanning the ranks the test already holds — the same population the store
// queries, read independently of it.
func mustTopOf(t *testing.T, ranks map[string]string, anchor string) string {
	t.Helper()
	below := ""
	for _, other := range ranks {
		if other < anchor && other > below {
			below = other
		}
	}
	return mustBetween(t, below, anchor)
}

// mustBottomOf is the same statement at the other end: the midpoint between the
// key trailing a frame and the nearest key the workspace holds above it.
func mustBottomOf(t *testing.T, ranks map[string]string, anchor string) string {
	t.Helper()
	above := ""
	for _, other := range ranks {
		if other > anchor && (above == "" || other < above) {
			above = other
		}
	}
	return mustBetween(t, anchor, above)
}

func mustBetween(t *testing.T, lower, upper string) string {
	t.Helper()
	between, err := rank.Midpoint(lower, upper)
	if err != nil {
		t.Fatalf("rank.Midpoint(%q, %q) error = %v", lower, upper, err)
	}
	return between
}

// noRoomFixture names the issues of TestPlacementMakesRoomBetweenKeysThatPadToTheSameValue.
type noRoomFixture struct{ upper, lower, moved string }

// A key placed against stored keys that leave no room lands where it was asked
// to anyway: between a relative move's neighbors, or past a frame's edge. The
// pairs are ones spacing can write — a rank beside itself extended by zeros —
// and an all-zero rank leading its frame, which leaves no room above it for
// every placement that passes the top edge. A key that does not make room would
// land the issue below both neighbors while the command reports success.
func TestPlacementMakesRoomBetweenKeysThatPadToTheSameValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		upper, lower string // the neighbors' planted ranks; an empty upper deletes that neighbor
		// place performs the placement and returns the ids that must now sort
		// strictly ascending.
		place func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string
	}{
		{"above", "10", "100", func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string {
			if _, err := st.RankAbove(ctx, ids.moved, ids.lower); err != nil {
				t.Fatalf("RankAbove error = %v", err)
			}
			return []string{ids.upper, ids.moved, ids.lower}
		}},
		{"below", "10", "100", func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string {
			if _, err := st.RankBelow(ctx, ids.moved, ids.upper); err != nil {
				t.Fatalf("RankBelow error = %v", err)
			}
			return []string{ids.upper, ids.moved, ids.lower}
		}},
		{"above an all-zero frame leader", "", "0", func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string {
			if _, err := st.RankAbove(ctx, ids.moved, ids.lower); err != nil {
				t.Fatalf("RankAbove error = %v", err)
			}
			return []string{ids.moved, ids.lower}
		}},
		{"to the top past an all-zero frame leader", "", "0", func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string {
			if _, err := st.RankToTop(ctx, ids.moved); err != nil {
				t.Fatalf("RankToTop error = %v", err)
			}
			return []string{ids.moved, ids.lower}
		}},
		{"rank set past an all-zero frame leader", "", "0", func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string {
			other := createTopLevelIssue(t, ctx, st, "Other")
			if _, err := st.RankSet(ctx, []string{other, ids.moved}); err != nil {
				t.Fatalf("RankSet error = %v", err)
			}
			return []string{other, ids.moved, ids.lower}
		}},
		{"created at the top past the only ranked row, all zeros", "", "0", func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string {
			// The issue being created is not stored yet, so the respace around
			// "0" finds that one row and nothing else.
			if err := st.ExecRawForTest(ctx, "UPDATE issues SET deleted_at = ? WHERE id = ?", "2026-01-01T00:00:00Z", ids.moved); err != nil {
				t.Fatalf("delete the moved issue: %v", err)
			}
			created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Created", Topic: "rank", IssueType: "epic", Placement: storage.RankTop})
			if err != nil {
				t.Fatalf("CreateIssue(top) error = %v", err)
			}
			return []string{created.ID, ids.lower}
		}},
		{"created at the top past an all-zero leader", "", "0", func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string {
			created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Created", Topic: "rank", IssueType: "epic", Placement: storage.RankTop})
			if err != nil {
				t.Fatalf("CreateIssue(top) error = %v", err)
			}
			return []string{created.ID, ids.lower}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := openIssueStore(t, ctx)
			ids := noRoomFixture{
				upper: createTopLevelIssue(t, ctx, st, "Upper"),
				lower: createTopLevelIssue(t, ctx, st, "Lower"),
				moved: createTopLevelIssue(t, ctx, st, "Moved"),
			}
			planted := map[string]string{ids.lower: tc.lower, ids.moved: "z"}
			if tc.upper != "" {
				planted[ids.upper] = tc.upper
			} else if err := st.ExecRawForTest(ctx, "UPDATE issues SET deleted_at = ? WHERE id = ?", "2026-01-01T00:00:00Z", ids.upper); err != nil {
				t.Fatalf("delete the upper neighbor: %v", err)
			}
			for id, r := range planted {
				if err := st.ExecRawForTest(ctx, "UPDATE issues SET item_rank = ? WHERE id = ?", r, id); err != nil {
					t.Fatalf("plant rank %q for %s: %v", r, id, err)
				}
			}
			if _, err := rank.Midpoint(tc.upper, tc.lower); !errors.Is(err, rank.ErrNoRoom) {
				t.Fatalf("rank.Midpoint(%q, %q) error = %v, want ErrNoRoom; this case no longer plants a pair without room", tc.upper, tc.lower, err)
			}

			order := tc.place(t, ctx, st, ids)

			ranks := make([]string, len(order))
			for i, id := range order {
				issue, err := st.GetIssue(ctx, id)
				if err != nil {
					t.Fatalf("GetIssue(%s) error = %v", id, err)
				}
				ranks[i] = issue.Rank
			}
			for i := 1; i < len(ranks); i++ {
				if ranks[i-1] >= ranks[i] {
					t.Fatalf("ranks %q, want strictly ascending for %v", ranks, order)
				}
			}
		})
	}
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

	// Either direction is the one refusal, naming the epic as the container
	// whichever side of the pair it was given on.
	want := storage.FrameContainmentError{ContainerID: fx.epic.ID, ContainedID: fx.children[0].ID}
	for _, tc := range []struct {
		name string
		rank func() error
	}{
		{"RankAbove(child, own epic)", func() error { _, err := st.RankAbove(ctx, fx.children[0].ID, fx.epic.ID); return err }},
		{"RankBelow(epic, own child)", func() error { _, err := st.RankBelow(ctx, fx.epic.ID, fx.children[0].ID); return err }},
	} {
		var got storage.FrameContainmentError
		if err := tc.rank(); !errors.As(err, &got) || got != want {
			t.Errorf("%s error = %v, want %#v", tc.name, err, want)
		}
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
// decides what the NEXT rank is computed against.
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
	// nothing else, with the room beside C1 measured across the whole keyspace
	// so the key landed in cannot be one another issue is holding.
	if want := mustTopOf(t, before, before[fx.children[0].ID]); after[fx.children[2].ID] != want {
		t.Errorf("C3 rank = %q, want %q = the room above C1 %q, its frame's leading key", after[fx.children[2].ID], want, before[fx.children[0].ID])
	}
	for _, issue := range []model.Issue{fx.epic, fx.standalone, fx.children[0], fx.children[1]} {
		if after[issue.ID] != before[issue.ID] {
			t.Errorf("issue %s rank changed %q -> %q; only C3 moves", issue.ID, before[issue.ID], after[issue.ID])
		}
	}

	// The precondition that gives the rest of this case its teeth: the key a
	// frame-scoped placement takes here and the key an unscoped one would take
	// are different strings. Without that the assertion above passes whichever
	// query ran — the shape of an invariant test that proves nothing.
	if scoped, unscoped := after[fx.children[2].ID], mustTopOf(t, before, before[fx.epic.ID]); scoped == unscoped {
		t.Fatalf("a frame-scoped placement and an unscoped one both yield %q here; this case cannot tell them apart", scoped)
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
	if want := mustTopOf(t, after, after[fx.epic.ID]); final[fx.standalone.ID] != want {
		t.Errorf("standalone rank = %q, want %q = the room above the top-level leader %q", final[fx.standalone.ID], want, after[fx.epic.ID])
	}
	if contaminated := mustTopOf(t, after, after[fx.children[2].ID]); final[fx.standalone.ID] == contaminated {
		t.Errorf("standalone rank = %q was computed against C3 %q, a key in another frame", final[fx.standalone.ID], after[fx.children[2].ID])
	}

	// The bottom edge is scoped the same way: C3 back to the bottom of its
	// frame is seeded from C2, the key that trails its siblings — not from the
	// workspace's last key.
	if _, err := st.RankToBottom(ctx, fx.children[2].ID); err != nil {
		t.Fatalf("RankToBottom(C3) error = %v", err)
	}
	bottom := currentRanks(t, ctx, st, all)
	if want := mustBottomOf(t, final, final[fx.children[1].ID]); bottom[fx.children[2].ID] != want {
		t.Errorf("C3 rank = %q, want %q = the room below C2 %q, its frame's trailing key", bottom[fx.children[2].ID], want, final[fx.children[1].ID])
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
// second line of defence untestable from outside the package.
func TestWriteRankRefusesAnIssueDeletedUnderTheLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Doomed", Topic: "frame", IssueType: "epic", Placement: storage.RankBottom})
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

// TestMutationValueYieldsTheZeroValueWhenTheMutationFails pins the contract
// that lets every rank verb return its result and its error together without
// the two disagreeing.
//
// Asserting it here rather than through a verb is deliberate: forcing a verb to
// fail midway needs an injection seam that exists for no other reason, and the
// guarantee belongs to this helper, which is what every verb returns through.
// [LAW:behavior-not-structure]
func TestMutationValueYieldsTheZeroValueWhenTheMutationFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	want := storage.RankEnd{Frame: "an-epic", Moved: true}

	// The closure failing is the easy half, and on its own it proves nothing: a
	// closure that returns an error never reaches the assignment, so the result
	// is zero whether or not anything zeroes it. The case that needs the guard
	// is the closure SUCCEEDING and the commit after it failing — withMutation
	// runs tx.Commit and the working-set commit once fn has already returned its
	// value. Cancelling the context from inside the closure stages exactly that:
	// the value is computed and assigned, then the commit it was computed for
	// cannot land.
	cancelCtx, cancel := context.WithCancel(ctx)
	got, err := mutationValue(cancelCtx, st, "mutation-value-commit-failure-test", func(ctx context.Context, tx *sql.Tx) (storage.RankEnd, error) {
		cancel()
		return want, nil
	})
	if err == nil {
		t.Fatalf("mutationValue succeeded with a cancelled commit; want a failure")
	}
	if got != (storage.RankEnd{}) {
		t.Errorf("mutationValue returned %+v beside an error; want the zero value, since a populated result reads as a move that happened", got)
	}

	boom := errors.New("the closure itself failed")
	got, err = mutationValue(ctx, st, "mutation-value-failure-test", func(ctx context.Context, tx *sql.Tx) (storage.RankEnd, error) {
		return want, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("mutationValue error = %v, want the failure the closure reported", err)
	}
	if got != (storage.RankEnd{}) {
		t.Errorf("mutationValue returned %+v beside a closure error; want the zero value", got)
	}

	// The other half of the contract: a successful mutation hands its value back
	// unchanged, or the zeroing above would be indistinguishable from losing it.
	got, err = mutationValue(ctx, st, "mutation-value-success-test", func(ctx context.Context, tx *sql.Tx) (storage.RankEnd, error) {
		return want, nil
	})
	if err != nil {
		t.Fatalf("mutationValue error = %v, want success", err)
	}
	if got != want {
		t.Errorf("mutationValue returned %+v, want %+v", got, want)
	}
}

// TestFrameResolutionRefusesADeletedNamedIssue covers the case writeRankTx
// cannot: an id that frame resolution substitutes away.
//
// writeRankTx re-reads the row it is about to write, which closes the race for
// every id that reaches the write. A named id does not always reach it. Naming
// a child of an epic beside a top-level issue resolves the child to its epic,
// and from there the epic is what every later check sees — so a delete of the
// child landing after the pre-lock gate would go unnoticed, and the set would
// rank the epic on behalf of an issue that no longer exists.
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
		_, _, pairErr = rankPairTx(ctx, tx, child.ID, fx.standalone.ID)
		return nil
	}); err != nil {
		t.Fatalf("withMutation error = %v", err)
	}
	if pairErr == nil {
		t.Fatalf("rankPairTx accepted the deleted %s by substituting its epic %s; want a refusal", child.ID, fx.epic.ID)
	}
}

// An unranked anchor has no place in the order to stand beside. Its "" read as
// an open end would land a move above it at the keyspace's midpoint and a move
// below it above every ranked issue, each reported as a success.
func TestRelativeMoveRefusesAnUnrankedAnchor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	moved := createTopLevelIssue(t, ctx, st, "Moved")
	anchor := createTopLevelIssue(t, ctx, st, "Anchor")
	createTopLevelIssue(t, ctx, st, "Other")
	if err := st.ExecRawForTest(ctx, "UPDATE issues SET item_rank = '' WHERE id = ?", anchor); err != nil {
		t.Fatalf("unrank the anchor: %v", err)
	}
	before, err := st.GetIssue(ctx, moved)
	if err != nil {
		t.Fatalf("GetIssue(%s) error = %v", moved, err)
	}
	for name, move := range map[string]func(context.Context, string, string) (storage.RankMove, error){
		"above": st.RankAbove,
		"below": st.RankBelow,
	} {
		if _, err := move(ctx, moved, anchor); err == nil || !strings.Contains(err.Error(), "has no rank") {
			t.Errorf("rank %s an unranked anchor: error = %v, want a refusal naming the missing rank", name, err)
		}
	}
	after, err := st.GetIssue(ctx, moved)
	if err != nil {
		t.Fatalf("GetIssue(%s) error = %v", moved, err)
	}
	if after.Rank != before.Rank {
		t.Errorf("a refused move still moved %s: rank %q -> %q", moved, before.Rank, after.Rank)
	}
}

// TestCreateAtTopDrawsItsKeyFromItsOwnFrame is the creation-side twin of
// TestRankToEdgeDrawsItsKeyFromItsOwnFrame, and it asserts rank STRINGS for
// the same reason: the rendered order cannot see a misdrawn key. A child filed
// at the top of its epic leads its siblings whichever key it holds, so the
// listing looks identical while the key was drawn from a keyspace the child is
// never read against — and it is the keyspace, not the order, that decides
// what the NEXT rank is computed against.
func TestCreateAtTopDrawsItsKeyFromItsOwnFrame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newFrameFixture(t, ctx, st)
	all := append([]model.Issue{fx.epic, fx.standalone}, fx.children...)
	before := currentRanks(t, ctx, st, all)

	// The fixture shape this case rests on: the epic leads the top level, and
	// C1 leads the epic's children. Stated rather than assumed, so a fixture
	// that changes shape fails here instead of asserting something else.
	if before[fx.epic.ID] >= before[fx.standalone.ID] {
		t.Fatalf("epic rank %q is not the top-level leader (standalone %q); the fixture no longer sets up this case", before[fx.epic.ID], before[fx.standalone.ID])
	}
	if before[fx.children[0].ID] >= before[fx.children[1].ID] {
		t.Fatalf("C1 rank %q does not lead C2 %q; the fixture no longer sets up this case", before[fx.children[0].ID], before[fx.children[1].ID])
	}

	lead, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "New lead", Topic: "frame", IssueType: "task", ParentID: fx.epic.ID, Placement: storage.RankTop})
	if err != nil {
		t.Fatalf("CreateIssue(child, --top) error = %v", err)
	}
	// The key is seeded from C1's — the key that leads the new child's own
	// frame — and from nothing else, with the room beside C1 measured across
	// the whole keyspace so the key cannot be one another issue is holding.
	if want := mustTopOf(t, before, before[fx.children[0].ID]); lead.Rank != want {
		t.Errorf("child filed --top has rank %q, want %q = the room above C1 %q, its frame's leading key", lead.Rank, want, before[fx.children[0].ID])
	}
	if contaminated := mustTopOf(t, before, before[fx.epic.ID]); lead.Rank == contaminated {
		t.Errorf("child filed --top has rank %q, computed against the workspace's first key (the epic, %q) rather than against its frame", lead.Rank, before[fx.epic.ID])
	}
	// Filing writes one key. An issue that was already there and was not named
	// has no reason to move.
	for id, rankBefore := range currentRanks(t, ctx, st, all) {
		if rankBefore != before[id] {
			t.Errorf("issue %s rank changed %q -> %q; filing a new issue moves nothing that already exists", id, before[id], rankBefore)
		}
	}

	// The precondition that gives the rest of this case its teeth: the key a
	// frame-scoped placement takes here and the key an unscoped one would take
	// are different strings. Without that the assertions above pass whichever
	// query ran — the shape of an invariant test that proves nothing.
	if unscoped := mustTopOf(t, before, before[fx.epic.ID]); lead.Rank == unscoped {
		t.Fatalf("a frame-scoped placement and an unscoped one both yield %q here; this case cannot tell them apart", lead.Rank)
	}
	// And the key lands where a frame's top belongs: inside its own epic's
	// span, above the epic and below the sibling it now leads.
	if !(lead.Rank > before[fx.epic.ID] && lead.Rank < before[fx.children[0].ID]) {
		t.Errorf("child filed --top has rank %q, want it between its epic %q and C1 %q", lead.Rank, before[fx.epic.ID], before[fx.children[0].ID])
	}

	// A top-level --top create now: its key must come from the top-level
	// leader, never from the child that happens to hold a smaller string.
	topLead, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "New top-level lead", Topic: "frame", IssueType: "epic", Placement: storage.RankTop})
	if err != nil {
		t.Fatalf("CreateIssue(top-level, --top) error = %v", err)
	}
	if want := mustTopOf(t, before, before[fx.epic.ID]); topLead.Rank != want {
		t.Errorf("top-level issue filed --top has rank %q, want %q = the room above the top-level leader %q", topLead.Rank, want, before[fx.epic.ID])
	}
	if contaminated := mustTopOf(t, before, lead.Rank); topLead.Rank == contaminated {
		t.Errorf("top-level issue filed --top has rank %q, computed against %q, a key in the epic's frame", topLead.Rank, lead.Rank)
	}

	// The bottom edge keeps asking the whole workspace, and that is the
	// contract: filing at the bottom must land after everything that already
	// exists, which is what keeps an authored batch in the order its file
	// states. A child filed there is seeded from the workspace's last key — the
	// standalone's — not from C3's.
	trail, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "New trail", Topic: "frame", IssueType: "task", ParentID: fx.epic.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(child, default placement) error = %v", err)
	}
	after, err := rank.Midpoint(before[fx.standalone.ID], "")
	if err != nil {
		t.Fatalf("rank.Midpoint(%q, \"\") error = %v", before[fx.standalone.ID], err)
	}
	if trail.Rank != after {
		t.Errorf("child filed at the bottom has rank %q, want %q = a step past the workspace's last key %q", trail.Rank, after, before[fx.standalone.ID])
	}
	// Nothing sits outside the workspace's last key, so the room beside it is
	// the open end — the same answer, reached by the rule the top edge uses
	// rather than by a second one. [LAW:one-source-of-truth]
	if want := mustBottomOf(t, before, before[fx.standalone.ID]); trail.Rank != want {
		t.Errorf("child filed at the bottom has rank %q, want %q = the room below the workspace's last key", trail.Rank, want)
	}
}

// TestCreateAtTopOfAnEmptyFrameTakesADistinctKey covers the case scoping the
// top edge to the frame creates: the frame's edge reads as absent, both bounds
// come back open, and the midpoint of the whole keyspace is the opening key —
// which the workspace's first issue is already holding. The rendered order
// cannot see that either. Two issues sharing a key still list in some order,
// decided by the id tiebreak, and only the keys say that nothing ranked them.
func TestCreateAtTopOfAnEmptyFrameTakesADistinctKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	first, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(first) error = %v", err)
	}
	if first.Rank != openingKey(t) {
		t.Fatalf("the first issue in a workspace has rank %q, want the opening key %q; this case rests on it holding the key an empty-bounds midpoint yields", first.Rank, openingKey(t))
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Childless epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	// Ranked after the epic so that "beside the container" and "past the
	// workspace's last key" are different answers. With the epic trailing
	// everything, the two coincide and the case cannot tell them apart.
	trailing, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Trailing", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(trailing) error = %v", err)
	}

	only, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Only child", Topic: "frame", IssueType: "task", ParentID: epic.ID, Placement: storage.RankTop})
	if err != nil {
		t.Fatalf("CreateIssue(first child, --top) error = %v", err)
	}
	for _, other := range []model.Issue{first, epic, trailing} {
		if only.Rank == other.Rank {
			t.Errorf("the first child filed --top took rank %q, the key %s already holds; a rank orders one issue", only.Rank, other.ID)
		}
	}
	// There is no sibling to lead, so the frame's own container is the key it
	// sits beside: the child lands just past its epic, not past everything in
	// the workspace. Filing it at the far end would read as LAST in every view
	// that sorts a child by its own key, which is what --top did not ask for.
	if want := mustBottomOf(t, map[string]string{first.ID: first.Rank, epic.ID: epic.Rank, trailing.ID: trailing.Rank}, epic.Rank); only.Rank != want {
		t.Errorf("the first child filed --top took rank %q, want %q = the room just past its epic %q", only.Rank, want, epic.Rank)
	}
	if !(only.Rank > epic.Rank && only.Rank < trailing.Rank) {
		t.Errorf("the first child filed --top took rank %q, want it between its epic %q and the next top-level key %q", only.Rank, epic.Rank, trailing.Rank)
	}

	// Once the frame holds a key, the top edge has an order to lead again and
	// goes back to seeding from it.
	second, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Second child", Topic: "frame", IssueType: "task", ParentID: epic.ID, Placement: storage.RankTop})
	if err != nil {
		t.Fatalf("CreateIssue(second child, --top) error = %v", err)
	}
	ranksNow := map[string]string{first.ID: first.Rank, epic.ID: epic.Rank, only.ID: only.Rank}
	if want := mustTopOf(t, ranksNow, only.Rank); second.Rank != want {
		t.Errorf("the second child filed --top took rank %q, want %q = the room above its frame's leading key %q", second.Rank, want, only.Rank)
	}
	for id, held := range ranksNow {
		if second.Rank == held {
			t.Errorf("the second child filed --top took rank %q, the key %s already holds; a rank orders one issue", second.Rank, id)
		}
	}
}

// TestRelativeMoveDrawsItsRoomFromTheWholeWorkspace is the four-command case.
// The frame picks the anchor — rank pair resolution substitutes the epic for
// the child named — but the room beside that anchor is read from the whole
// workspace. Read with the frame's scope, the bound beside an anchor with
// nothing past it in the top level comes back open, and the midpoint of an open
// span is a key something outside the frame is already holding.
//
// The assertion is on the rank strings. The rendered order cannot see this:
// two issues sharing a key still list in some order, decided by the id
// tiebreak, so the listing looks settled while the keyspace has no order in it
// at all.
func TestRelativeMoveDrawsItsRoomFromTheWholeWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	standalone, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(standalone) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Child", Topic: "frame", IssueType: "task", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	// The precondition that gives this case its teeth: the child's key is the
	// one immediately past the anchor in the whole workspace, and it belongs to
	// another frame — so a frame-scoped read of the room finds nothing there
	// and calls the span open, while the span is exactly one key wide.
	if !(standalone.Rank < child.Rank && epic.Rank < standalone.Rank) {
		t.Fatalf("fixture no longer sets up this case: epic %q, standalone %q, child %q", epic.Rank, standalone.Rank, child.Rank)
	}

	move, err := st.RankBelow(ctx, child.ID, standalone.ID)
	if err != nil {
		t.Fatalf("RankBelow(child, standalone) error = %v", err)
	}
	// The pair resolves to the epic: a child named against a top-level issue is
	// ranked through the ancestor they can be compared in.
	if move.MovedID != epic.ID {
		t.Fatalf("RankBelow moved %s, want the epic %s — this case is about the resolved move", move.MovedID, epic.ID)
	}

	after := currentRanks(t, ctx, st, []model.Issue{epic, standalone, child})
	if want := mustBottomOf(t, map[string]string{epic.ID: epic.Rank, standalone.ID: standalone.Rank, child.ID: child.Rank}, standalone.Rank); after[epic.ID] != want {
		t.Errorf("the moved epic has rank %q, want %q = the room below its anchor %q", after[epic.ID], want, standalone.Rank)
	}
	if after[epic.ID] == after[child.ID] {
		t.Errorf("the moved epic took rank %q, the key its own child holds; a rank orders one issue", after[epic.ID])
	}
	if !(after[standalone.ID] < after[epic.ID] && after[epic.ID] < after[child.ID]) {
		t.Errorf("ranks %q (standalone), %q (epic), %q (child) do not place the epic between its anchor and the next key", after[standalone.ID], after[epic.ID], after[child.ID])
	}
}

// TestRankToEdgeOfAFrameWithNoRankedMemberFilesBesideItsContainer covers the
// one input the room lookup has no answer for: an empty key.
//
// An issue whose own rank is blank is not in the population frameEdgeHolderTx
// reads, so a frame holding only that issue reports no edge at all, while the
// verb still counts the move as real. The comparison then reads that empty key
// differently at each end — nothing sorts below "" but every rank sorts above
// it — so asking for the room beside it would send an issue to its frame's
// BOTTOM with a key above every issue in the workspace, an inversion no
// duplicate check would catch.
//
// A blank rank is not reachable through the API (ensureIssueRanks backfills at
// open), so it is written here directly: the point of the case is that the
// absent key is answered rather than averaged, whatever put it there.
func TestRankToEdgeOfAFrameWithNoRankedMemberFilesBesideItsContainer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	standalone, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(standalone) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	only, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Only child", Topic: "frame", IssueType: "task", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	// Ranked last, so that "beside the container" and "past the workspace" are
	// different keys. Without it the epic holds the trailing key, the two
	// answers coincide, and the case cannot tell which rule produced the one it
	// sees.
	trailing, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Trailing", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(trailing) error = %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET item_rank = '' WHERE id = ?`, only.ID); err != nil {
		t.Fatalf("blank the child's rank: %v", err)
	}

	if _, err := st.RankToBottom(ctx, only.ID); err != nil {
		t.Fatalf("RankToBottom(only child) error = %v", err)
	}
	after, err := st.GetIssue(ctx, only.ID)
	if err != nil {
		t.Fatalf("GetIssue error = %v", err)
	}
	if after.Rank == "" {
		t.Fatal("RankToBottom left the child with no rank at all")
	}
	// It lands beside the issue that frames it, which is the only key an empty
	// frame offers. Filing it past the workspace's last key would put it after
	// the trailing issue, which is what --bottom of a frame does not mean.
	if want := mustBottomOf(t, map[string]string{standalone.ID: standalone.Rank, epic.ID: epic.Rank, trailing.ID: trailing.Rank}, epic.Rank); after.Rank != want {
		t.Errorf("the child took rank %q, want %q = the room just past its epic %q", after.Rank, want, epic.Rank)
	}
	if !(after.Rank > epic.Rank && after.Rank < trailing.Rank) {
		t.Errorf("the child took rank %q, want it between its epic %q and the workspace's trailing key %q", after.Rank, epic.Rank, trailing.Rank)
	}
	for _, other := range []model.Issue{standalone, epic, trailing} {
		if after.Rank == other.Rank {
			t.Errorf("the child took rank %q, the key %s holds; a rank orders one issue", after.Rank, other.ID)
		}
	}
}

// TestRankSetOverAWholeFrameIsIdempotent pins that repeating a rank set over
// every member of a frame rewrites nothing. No unnamed sibling is left to
// anchor the stack, which is where a rank set that minted fresh keys walked
// them longer on every repeat — "V", then "VV", then "VF". The assertion is on
// the rank strings, because the rendered order is identical either way.
func TestRankSetOverAWholeFrameIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	// An outsider ranked after the epic gives the container's gap a far wall,
	// so a bound that excludes the stack is stable while one that does not
	// keeps closing in.
	outsider, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Outsider", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(outsider) error = %v", err)
	}
	children := make([]model.Issue, 0, 2)
	for _, title := range []string{"C1", "C2"} {
		child, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "frame", IssueType: "task", ParentID: epic.ID})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		children = append(children, child)
	}

	ids := []string{children[1].ID, children[0].ID}
	if _, err := st.RankSet(ctx, ids); err != nil {
		t.Fatalf("RankSet error = %v", err)
	}
	first := currentRanks(t, ctx, st, children)
	for round := 2; round <= 4; round++ {
		if _, err := st.RankSet(ctx, ids); err != nil {
			t.Fatalf("RankSet round %d error = %v", round, err)
		}
		after := currentRanks(t, ctx, st, children)
		for _, child := range children {
			if after[child.ID] != first[child.ID] {
				t.Errorf("round %d rewrote %s from %q to %q; the same stack asked for twice is the same order", round, child.ID, first[child.ID], after[child.ID])
			}
		}
	}
	// Nothing outside the frame moved, and every key still orders one issue.
	held := currentRanks(t, ctx, st, append([]model.Issue{epic, outsider}, children...))
	for _, outside := range []model.Issue{epic, outsider} {
		if held[outside.ID] != outside.Rank {
			t.Errorf("rank set among an epic's children moved %s: rank %q -> %q", outside.ID, outside.Rank, held[outside.ID])
		}
	}
	seen := map[string]string{}
	for id, r := range held {
		if other, dup := seen[r]; dup {
			t.Errorf("issues %s and %s both hold rank %q", other, id, r)
		}
		seen[r] = id
	}
}

// TestRankSetOverEveryTopLevelIssueTakesADistinctKey covers naming every
// top-level issue while issues inside an epic still hold keys.
//
// The fixture reparents the workspace's FIRST issue, the one holding
// the opening key, into an epic. That is what makes the case adversarial: a rank
// set that measured its room as though the workspace were empty would be
// handed the opening key, the key that issue still holds. Without the reparent
// every surviving key is above the opening key, the collision cannot happen, and
// the case passes however the keys are chosen.
func TestRankSetOverEveryTopLevelIssueTakesADistinctKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	opener, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Opener", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(opener) error = %v", err)
	}
	if opener.Rank != openingKey(t) {
		t.Fatalf("the first issue holds %q, want the opening key %q; this case rests on it holding the key open bounds yield", opener.Rank, openingKey(t))
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	other, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Other top-level", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(other) error = %v", err)
	}
	// The opener moves inside the epic, keeping its key. The top level now
	// holds exactly the two issues the stack is about to name.
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: opener.ID, ParentID: epic.ID, CreatedBy: "tester"}); err != nil {
		t.Fatalf("SetParent(opener under epic) error = %v", err)
	}
	if held := currentRanks(t, ctx, st, []model.Issue{opener})[opener.ID]; held != openingKey(t) {
		t.Fatalf("the reparented opener holds %q, want the opening key %q; reparenting must not rewrite a rank", held, openingKey(t))
	}

	if _, err := st.RankSet(ctx, []string{other.ID, epic.ID}); err != nil {
		t.Fatalf("RankSet(every top-level issue) error = %v", err)
	}

	after := currentRanks(t, ctx, st, []model.Issue{epic, other, opener})
	for _, id := range []string{epic.ID, other.ID} {
		if after[id] == after[opener.ID] {
			t.Errorf("rank set wrote %q onto %s, the key %s holds inside the epic; a rank orders one issue", after[id], id, opener.ID)
		}
	}
	listed, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues error = %v", err)
	}
	holder := map[string]string{}
	for _, issue := range listed {
		if issue.Rank == "" {
			continue
		}
		if prior, taken := holder[issue.Rank]; taken {
			t.Errorf("issues %s and %s both hold rank %q; a rank orders one issue", prior, issue.ID, issue.Rank)
		}
		holder[issue.Rank] = issue.ID
	}
	if after[other.ID] >= after[epic.ID] {
		t.Errorf("rank set put %s at %q and %s at %q; the named order was other, then epic", other.ID, after[other.ID], epic.ID, after[epic.ID])
	}
}

// TestFilingIntoAContainerWithNoRankOfItsOwnFallsBackToTheWorkspace covers the
// branch firstInFrameBoundsTx takes when the frame names an issue carrying no
// rank: there is no container key to sit beside, so the placement goes past the
// workspace's last key instead.
//
// A container without a rank is not reachable through the API — ensureIssueRanks
// backfills at open — but it is reachable between a restore and that backfill,
// and the branch exists for it. It is written here directly, the same way the
// child's blank rank is in the case above, because the point is that an absent
// container key is answered rather than turned into a position.
func TestFilingIntoAContainerWithNoRankOfItsOwnFallsBackToTheWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	standalone, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(standalone) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Rankless epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	trailing, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Trailing", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(trailing) error = %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET item_rank = '' WHERE id = ?`, epic.ID); err != nil {
		t.Fatalf("blank the epic's rank: %v", err)
	}

	child, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Child", Topic: "frame", IssueType: "task", ParentID: epic.ID, Placement: storage.RankTop})
	if err != nil {
		t.Fatalf("CreateIssue(child, --top) error = %v", err)
	}
	if child.Rank == "" {
		t.Fatal("the child was filed with no rank at all")
	}
	// The container offers no key, so the workspace's last one bounds it.
	if want := mustBottomOf(t, map[string]string{standalone.ID: standalone.Rank, trailing.ID: trailing.Rank}, trailing.Rank); child.Rank != want {
		t.Errorf("the child took rank %q, want %q = the room past the workspace's last key %q", child.Rank, want, trailing.Rank)
	}
	for _, other := range []model.Issue{standalone, trailing} {
		if child.Rank == other.Rank {
			t.Errorf("the child took rank %q, the key %s holds; a rank orders one issue", child.Rank, other.ID)
		}
	}
}

// TestRepeatingARelativeMoveThatHasArrivedKeepsItsKey pins that the key a
// relative move vacates is not a wall against it. Counted as one, a move
// repeated after it has already arrived is bounded by its own current key and
// halves the gap every time. The order is identical either way, so the
// assertion is on the moved issue's key.
func TestRepeatingARelativeMoveThatHasArrivedKeepsItsKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	issues := makeChildrenOf(t, ctx, st, epic.ID, "A", "B", "C")
	a, b, c := issues[0], issues[1], issues[2]

	moves := []struct {
		name  string
		moved model.Issue
		move  func() error
	}{
		{"RankAbove(C, B)", c, func() error { _, err := st.RankAbove(ctx, c.ID, b.ID); return err }},
		{"RankBelow(A, B)", a, func() error { _, err := st.RankBelow(ctx, a.ID, b.ID); return err }},
	}
	for _, m := range moves {
		if err := m.move(); err != nil {
			t.Fatalf("%s error = %v", m.name, err)
		}
		arrived := currentRanks(t, ctx, st, []model.Issue{m.moved})[m.moved.ID]
		if err := m.move(); err != nil {
			t.Fatalf("%s repeated error = %v", m.name, err)
		}
		if again := currentRanks(t, ctx, st, []model.Issue{m.moved})[m.moved.ID]; again != arrived {
			t.Errorf("%s repeated moved %s from %q to %q; a move that has arrived is already where it was asked to be", m.name, m.moved.ID, arrived, again)
		}
	}
}

// TestRankSetReversingALargeFrameHandsRoundEveryKey reverses a frame larger than
// two id batches. Every issue is named, so no key is minted and the rewrite spans
// several batched statements: every key the frame held before is held after, by
// exactly one issue, in the named order.
func TestRankSetReversingALargeFrameHandsRoundEveryKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeTopLevelIssues(t, ctx, st, 2*idChunkSize+3, "row %d")
	before, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	named := slices.Clone(ids)
	slices.Reverse(named)
	if _, err := st.RankSet(ctx, named); err != nil {
		t.Fatalf("RankSet error = %v", err)
	}
	after, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}

	if got := issueIDs(after); !slices.Equal(got, named) {
		t.Fatalf("order after rank set = %v, want %v", got, named)
	}
	for i, issue := range after {
		if issue.Rank != before[i].Rank {
			t.Errorf("slot %d holds %q after rank set, want the key %q it held before; %s did not take its slot's key", i, issue.Rank, before[i].Rank, issue.ID)
		}
	}
}

// TestRankSetWritesOnlyTheIssuesItNamed is what keeps a rank set's cost the
// set's and not the frame's. Two issues from the middle of a frame lead it, and
// every issue they pass keeps the key it held, so the write is the two rows named.
func TestRankSetWritesOnlyTheIssuesItNamed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeTopLevelIssues(t, ctx, st, 7, "row %d")
	before := storedKeys(t, ctx, st)
	named := []string{ids[4], ids[3]}
	if _, err := st.RankSet(ctx, named); err != nil {
		t.Fatalf("RankSet error = %v", err)
	}
	after, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if got, want := issueIDs(after), slices.Concat(named, ids[:3], ids[5:]); !slices.Equal(got, want) {
		t.Fatalf("order after rank set = %v, want %v", got, want)
	}
	assertDistinctStoredRanks(t, after)
	for _, issue := range after {
		if !slices.Contains(named, issue.ID) && issue.Rank != before[issue.ID] {
			t.Errorf("rank set rewrote %s from %q to %q; it was not named", issue.ID, before[issue.ID], issue.Rank)
		}
	}
}

// TestRankSetHandsRoundTheKeysOfTheIssuesLeadingTheFrame pins that a set among
// the issues already leading a frame takes no new key: reversing two is a swap,
// so the keyspace past the frame's top is not spent on an order that is
// reachable without it.
func TestRankSetHandsRoundTheKeysOfTheIssuesLeadingTheFrame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeTopLevelIssues(t, ctx, st, 4, "row %d")
	before := storedKeys(t, ctx, st)
	if _, err := st.RankSet(ctx, []string{ids[1], ids[0]}); err != nil {
		t.Fatalf("RankSet error = %v", err)
	}
	after := storedKeys(t, ctx, st)
	if after[ids[1]] != before[ids[0]] || after[ids[0]] != before[ids[1]] {
		t.Errorf("reversing the two leading issues left keys %q and %q, want the pair swapped from %q and %q", after[ids[1]], after[ids[0]], before[ids[1]], before[ids[0]])
	}
	for _, id := range ids[2:] {
		if after[id] != before[id] {
			t.Errorf("rank set rewrote %s from %q to %q; it was not named", id, before[id], after[id])
		}
	}
}

func storedKeys(t *testing.T, ctx context.Context, st *Store) map[string]string {
	t.Helper()
	listed, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	keys := make(map[string]string, len(listed))
	for _, issue := range listed {
		keys[issue.ID] = issue.Rank
	}
	return keys
}

// TestRankSetStampsOnlyTheIssuesItNamed pins whose clock a rank set moves. The
// named issue that changed place is updated now; a frame-mate the stack merely
// displaced keeps its order among the unnamed and keeps its updated_at, which
// is what says how long a ticket has sat untouched.
func TestRankSetStampsOnlyTheIssuesItNamed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return created }

	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	issues := makeChildrenOf(t, ctx, st, epic.ID, "A", "B", "C")
	a, b, c := issues[0], issues[1], issues[2]

	ranked := created.Add(time.Hour)
	st.clock = func() time.Time { return ranked }
	if _, err := st.RankSet(ctx, []string{c.ID, a.ID}); err != nil {
		t.Fatalf("RankSet(C, A) error = %v", err)
	}

	for _, want := range []struct {
		issue model.Issue
		at    time.Time
		why   string
	}{
		{c, ranked, "it was named and moved to the top"},
		{a, ranked, "it was named and moved below C"},
		{b, b.UpdatedAt, "it was not named; the stack only displaced it"},
	} {
		got, err := st.GetIssue(ctx, want.issue.ID)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", want.issue.ID, err)
		}
		if !got.UpdatedAt.Equal(want.at) {
			t.Errorf("%s updated_at = %s, want %s: %s", got.Title, got.UpdatedAt, want.at, want.why)
		}
	}
}

// TestRankSetStampsByPlaceAndNotByKey pins that a stamp follows an issue's place
// in the frame and not whether its key changed. Joining D below A and B gives the
// set a key past the frame's top, so A and B take new keys while standing exactly
// where they stood, and only D moved.
func TestRankSetStampsByPlaceAndNotByKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return created }

	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	issues := makeChildrenOf(t, ctx, st, epic.ID, "A", "B", "C", "D")
	a, b, c, d := issues[0], issues[1], issues[2], issues[3]

	ranked := created.Add(time.Hour)
	st.clock = func() time.Time { return ranked }
	if _, err := st.RankSet(ctx, []string{a.ID, b.ID, d.ID}); err != nil {
		t.Fatalf("RankSet(A, B, D) error = %v", err)
	}

	for _, want := range []struct {
		issue model.Issue
		at    time.Time
		why   string
	}{
		{a, a.UpdatedAt, "it stands where it stood"},
		{b, b.UpdatedAt, "it stands where it stood"},
		{c, c.UpdatedAt, "it was not named"},
		{d, ranked, "it was named and moved above C"},
	} {
		got, err := st.GetIssue(ctx, want.issue.ID)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", want.issue.ID, err)
		}
		if !got.UpdatedAt.Equal(want.at) {
			t.Errorf("%s updated_at = %s, want %s: %s", got.Title, got.UpdatedAt, want.at, want.why)
		}
	}
}

// TestRankSetOverIssuesSharingAKey pins what a rank set does with two issues
// on one key, as imported keys can be, when it names one directly above the
// other. A listing breaks the tie by id: an order that agrees with the ids has
// arrived, and one that reverses them is refused by name with nothing changed.
func TestRankSetOverIssuesSharingAKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		reversed  bool
		wantError string
	}{
		{name: "in id order", reversed: false},
		{name: "against id order", reversed: true, wantError: "share the key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := openIssueStore(t, ctx)

			ids := makeTopLevelIssues(t, ctx, st, 3, "row %d")
			slices.Sort(ids)
			low, high, other := ids[0], ids[1], ids[2]
			for id, key := range map[string]string{low: "M", high: "M", other: "T"} {
				if _, err := st.db.ExecContext(ctx, `UPDATE issues SET item_rank = ? WHERE id = ?`, key, id); err != nil {
					t.Fatalf("plant %q on %s: %v", key, id, err)
				}
			}
			named := []string{low, high}
			if tc.reversed {
				named = []string{high, low}
			}
			before, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
			if err != nil {
				t.Fatalf("ListIssues() error = %v", err)
			}

			_, err = st.RankSet(ctx, named)
			after, listErr := st.ListIssues(ctx, storage.ListIssuesFilter{})
			if listErr != nil {
				t.Fatalf("ListIssues() error = %v", listErr)
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), "lit rank "+high+" --above "+low) {
					t.Fatalf("RankSet(%v) error = %v, want a refusal naming the tie and the move that separates it", named, err)
				}
				// The tie stays until a different command separates it, so the
				// refusal is a deterministic one, never advice to retry.
				var refused model.ValidationError
				if !errors.As(err, &refused) {
					t.Fatalf("RankSet(%v) error = %T, want model.ValidationError", named, err)
				}
				for i := range after {
					if after[i].ID != before[i].ID || after[i].Rank != before[i].Rank {
						t.Fatalf("a refused rank set changed the store: %v -> %v", before, after)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("RankSet(%v) error = %v", named, err)
			}
			if got, want := issueIDs(after)[:2], named; !slices.Equal(got, want) {
				t.Fatalf("order after rank set starts %v, want %v (then %s)", got, want, other)
			}
		})
	}
}

// plantKeys hands each issue a key directly, as an import can, sharing one if
// two are given it.
func plantKeys(t *testing.T, ctx context.Context, st *Store, keys map[string]string) {
	t.Helper()
	for id, key := range keys {
		if _, err := st.db.ExecContext(ctx, `UPDATE issues SET item_rank = ? WHERE id = ?`, key, id); err != nil {
			t.Fatalf("plant %q on %s: %v", key, id, err)
		}
	}
}

// TestRankSetNeverHandsOnAKeyAnUnnamedIssueShares covers a named issue tied with
// an unnamed one. Passing that key to another named issue would leave the two on
// one key with a listing breaking the tie by id, so `other`, which sorts past
// `high`, would land below it.
func TestRankSetNeverHandsOnAKeyAnUnnamedIssueShares(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeTopLevelIssues(t, ctx, st, 3, "row %d")
	slices.Sort(ids)
	low, high, other := ids[0], ids[1], ids[2]
	plantKeys(t, ctx, st, map[string]string{low: "M", high: "M", other: "T"})

	if _, err := st.RankSet(ctx, []string{low, other}); err != nil {
		t.Fatalf("RankSet error = %v", err)
	}
	after, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if got, want := issueIDs(after), []string{low, other, high}; !slices.Equal(got, want) {
		t.Fatalf("order after rank set = %v, want %v", got, want)
	}
}

// TestRankSetMintsPastATieItCannotHandRound covers sets whose order a tie does not
// stand in the way of. Issues that share a key leave it behind and take minted
// ones, so nothing lands on a key beside an issue that never held it, and the run
// stops at the tie, so the rows it keeps are still the frame's first.
func TestRankSetMintsPastATieItCannotHandRound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		keys  []string
		named []int
		want  []int
	}{
		{name: "a joiner would land on a leader's tied key", keys: []string{"M", "M", "N", "Z"}, named: []int{0, 3, 1}, want: []int{0, 3, 1, 2}},
		{name: "the tie follows a leader that keeps its key", keys: []string{"L", "M", "M", "Z"}, named: []int{0, 3, 1, 2}, want: []int{0, 3, 1, 2}},
		{name: "the tie stands below the places the set fills", keys: []string{"M", "M", "L"}, named: []int{1, 0}, want: []int{1, 0, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := openIssueStore(t, ctx)

			ids := makeTopLevelIssues(t, ctx, st, len(tc.keys), "row %d")
			slices.Sort(ids)
			planted := make(map[string]string, len(ids))
			for i, id := range ids {
				planted[id] = tc.keys[i]
			}
			plantKeys(t, ctx, st, planted)

			named := make([]string, len(tc.named))
			for i, index := range tc.named {
				named[i] = ids[index]
			}
			want := make([]string, len(tc.want))
			for i, index := range tc.want {
				want[i] = ids[index]
			}
			if _, err := st.RankSet(ctx, named); err != nil {
				t.Fatalf("RankSet(%v) error = %v", named, err)
			}
			after, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
			if err != nil {
				t.Fatalf("ListIssues() error = %v", err)
			}
			if got := issueIDs(after); !slices.Equal(got, want) {
				t.Fatalf("order after RankSet(%v) = %v, want %v", named, got, want)
			}
			held := make(map[string]string, len(after))
			for _, issue := range after {
				if mate, taken := held[issue.Rank]; taken && slices.Contains(named, issue.ID) {
					t.Fatalf("%s took the key %q %s holds", issue.ID, issue.Rank, mate)
				}
				held[issue.Rank] = issue.ID
			}
		})
	}
}

// TestRankSetRefusalNamesTheMoveThatClearsIt runs the move a refused tie names,
// and the same set then has nothing to refuse.
func TestRankSetRefusalNamesTheMoveThatClearsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeTopLevelIssues(t, ctx, st, 3, "row %d")
	slices.Sort(ids)
	low, high, other := ids[0], ids[1], ids[2]
	plantKeys(t, ctx, st, map[string]string{low: "M", high: "M", other: "T"})

	if _, err := st.RankSet(ctx, []string{high, low}); err == nil {
		t.Fatal("RankSet error = nil, want a refusal of the tie")
	}
	if _, err := st.RankAbove(ctx, high, low); err != nil {
		t.Fatalf("RankAbove() error = %v", err)
	}
	if _, err := st.RankSet(ctx, []string{high, low}); err != nil {
		t.Fatalf("RankSet after the move the refusal named: %v", err)
	}
}

// TestRankSetNamesAKeyItCannotPlaceBeside plants a malformed key at the head of
// the frame. A placement cannot be made beside it, and the error has to say which
// key stopped it, because the issue that holds it is not one the set named.
func TestRankSetNamesAKeyItCannotPlaceBeside(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeTopLevelIssues(t, ctx, st, 3, "row %d")
	plantKeys(t, ctx, st, map[string]string{ids[0]: "M-1"})

	_, err := st.RankSet(ctx, []string{ids[1], ids[2]})
	if err == nil || !strings.Contains(err.Error(), `"M-1"`) {
		t.Fatalf("RankSet error = %v, want one naming the key %q", err, "M-1")
	}
}

// TestRankSetOfAnOrderThatHasArrivedOnATieWritesNothing names the issue a tie
// already lists first. Its key is one an unnamed issue shares, so a set moving
// anything would give it a new one, and this set has nothing to move.
func TestRankSetOfAnOrderThatHasArrivedOnATieWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return created }

	ids := makeTopLevelIssues(t, ctx, st, 2, "row %d")
	slices.Sort(ids)
	low, high := ids[0], ids[1]
	plantKeys(t, ctx, st, map[string]string{low: "M", high: "M"})
	st.clock = func() time.Time { return created.Add(time.Hour) }

	if _, err := st.RankSet(ctx, []string{low}); err != nil {
		t.Fatalf("RankSet error = %v", err)
	}
	got, err := st.GetIssue(ctx, low)
	if err != nil {
		t.Fatalf("GetIssue(%s) error = %v", low, err)
	}
	if got.Rank != "M" || !got.UpdatedAt.Equal(created) {
		t.Errorf("a rank set of an arrived order left %s on %q updated %s, want it untouched on %q from %s", low, got.Rank, got.UpdatedAt, "M", created)
	}
}

// TestRankSetReadsATieInIDOrder pins what the frame's head leans on: it reads no
// further than the set needs, so where several issues share a key it has to meet
// them in listing order. Naming each of three tied issues in turn would report
// one that a lower id still lists above as already first if it did not. Both
// frame shapes run, because a container's rows are read through a join.
func TestRankSetReadsATieInIDOrder(t *testing.T) {
	t.Parallel()
	for _, frame := range []string{"top level", "container"} {
		for at := range 3 {
			t.Run(fmt.Sprintf("%s/id %d", frame, at), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				st := openIssueStore(t, ctx)

				ids := makeTopLevelIssues(t, ctx, st, 3, "row %d")
				if frame == "container" {
					epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic"})
					if err != nil {
						t.Fatalf("CreateIssue(epic) error = %v", err)
					}
					ids = ids[:0]
					for _, child := range makeChildrenOf(t, ctx, st, epic.ID, "A", "B", "C") {
						ids = append(ids, child.ID)
					}
				}
				slices.Sort(ids)
				plantKeys(t, ctx, st, map[string]string{ids[0]: "M", ids[1]: "M", ids[2]: "M"})

				if _, err := st.RankSet(ctx, []string{ids[at]}); err != nil {
					t.Fatalf("RankSet error = %v", err)
				}
				byID, err := st.ListIssues(ctx, storage.ListIssuesFilter{IDs: ids})
				if err != nil {
					t.Fatalf("ListIssues() error = %v", err)
				}
				want := slices.Insert(slices.Delete(slices.Clone(ids), at, at+1), 0, ids[at])
				if got := issueIDs(byID); !slices.Equal(got, want) {
					t.Fatalf("order after naming %s = %v, want %v", ids[at], got, want)
				}
			})
		}
	}
}

// TestRankSetPlacesANamedIssueThatHoldsNoKey pins that an unranked issue can be
// named in a rank set. An import writes a row unranked when its source had
// none, so it has no key to hand round; it takes a minted one like any other
// issue that joins the head.
func TestRankSetPlacesANamedIssueThatHoldsNoKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeTopLevelIssues(t, ctx, st, 3, "row %d")
	a, b, c := ids[0], ids[1], ids[2]
	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET item_rank = '' WHERE id = ?`, b); err != nil {
		t.Fatalf("unrank %s: %v", b, err)
	}

	if _, err := st.RankSet(ctx, []string{b, c}); err != nil {
		t.Fatalf("RankSet naming an unranked issue error = %v", err)
	}
	after, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if got, want := issueIDs(after), []string{b, c, a}; !slices.Equal(got, want) {
		t.Fatalf("order after rank set = %v, want %v", got, want)
	}
	assertDistinctStoredRanks(t, after)
}

// assertDistinctStoredRanks fails for every issue holding no key or a key
// another listed issue holds.
func assertDistinctStoredRanks(t *testing.T, issues []model.Issue) {
	t.Helper()
	seen := map[string]string{}
	for _, issue := range issues {
		if issue.Rank == "" {
			t.Errorf("%s holds no key", issue.ID)
		}
		if prior, dup := seen[issue.Rank]; dup {
			t.Errorf("%s and %s both hold %q", prior, issue.ID, issue.Rank)
		}
		seen[issue.Rank] = issue.ID
	}
}
