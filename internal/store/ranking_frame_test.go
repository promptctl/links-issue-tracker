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

func mustBefore(t *testing.T, r string) string {
	t.Helper()
	before, err := rank.Before(r)
	if err != nil {
		t.Fatalf("rank.Before(%q) error = %v", r, err)
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
// every placement that passes the top edge. rank.Midpoint once returned "100V"
// for the first pair, and the move wrote it: the issue landed below both
// neighbors and the command reported success.
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
			other := createRankTestIssue(t, ctx, st, "Other")
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
			created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Created", Topic: "rank", IssueType: "task", Placement: storage.RankTop})
			if err != nil {
				t.Fatalf("CreateIssue(top) error = %v", err)
			}
			return []string{created.ID, ids.lower}
		}},
		{"created at the top past an all-zero leader", "", "0", func(t *testing.T, ctx context.Context, st *Store, ids noRoomFixture) []string {
			created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Created", Topic: "rank", IssueType: "task", Placement: storage.RankTop})
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
				upper: createRankTestIssue(t, ctx, st, "Upper"),
				lower: createRankTestIssue(t, ctx, st, "Lower"),
				moved: createRankTestIssue(t, ctx, st, "Moved"),
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

// TestMutationValueYieldsTheZeroValueWhenTheMutationFails pins the contract
// that lets every rank verb return its result and its error together without
// the two disagreeing.
//
// The verbs used to declare the result outside the closure and assign to it
// partway through, so a step failing afterwards returned a populated value
// beside a non-nil error — a RankEnd naming the frame of a move that never
// happened. Callers check the error first, so nothing observed it; that is why
// it survived three review rounds, not why it was safe.
//
// Asserting it here rather than through a verb is deliberate: forcing a verb to
// fail midway needs an injection seam that exists for no other reason, and the
// guarantee belongs to this helper, which is what every verb now returns
// through. [LAW:behavior-not-structure]
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
		_, _, pairErr = rankPairTx(ctx, tx, child.ID, fx.standalone.ID)
		return nil
	}); err != nil {
		t.Fatalf("withMutation error = %v", err)
	}
	if pairErr == nil {
		t.Fatalf("rankPairTx accepted the deleted %s by substituting its epic %s; want a refusal", child.ID, fx.epic.ID)
	}
}

// An unranked anchor has no place in the order to stand beside. Its "" once
// reached rank.Midpoint as an open end, so a move above it landed at the
// keyspace's midpoint and a move below it above every ranked issue, each
// reported as a success.
func TestRelativeMoveRefusesAnUnrankedAnchor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	moved := createRankTestIssue(t, ctx, st, "Moved")
	anchor := createRankTestIssue(t, ctx, st, "Anchor")
	createRankTestIssue(t, ctx, st, "Other")
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
// the same reason: the rendered order cannot see this defect. A child filed at
// the top of its epic leads its siblings whichever key it holds, so the
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
	// span, above the epic and below the sibling it now leads, rather than
	// burrowing under every top-level issue the way an unscoped placement did.
	if !(lead.Rank > before[fx.epic.ID] && lead.Rank < before[fx.children[0].ID]) {
		t.Errorf("child filed --top has rank %q, want it between its epic %q and C1 %q", lead.Rank, before[fx.epic.ID], before[fx.children[0].ID])
	}

	// A top-level --top create now: its key must come from the top-level
	// leader, never from the child that happens to hold a smaller string.
	topLead, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "New top-level lead", Topic: "frame", IssueType: "task", Placement: storage.RankTop})
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
	// contract rather than the other half of this bug: filing at the bottom
	// must land after everything that already exists, which is what keeps an
	// authored batch in the order its file states. A child filed there is
	// seeded from the workspace's last key — the standalone's — not from C3's.
	trail, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "New trail", Topic: "frame", IssueType: "task", ParentID: fx.epic.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(child, default placement) error = %v", err)
	}
	if want := rank.After(before[fx.standalone.ID]); trail.Rank != want {
		t.Errorf("child filed at the bottom has rank %q, want %q = After(the workspace's last key %q)", trail.Rank, want, before[fx.standalone.ID])
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
// come back open, and the midpoint of the whole keyspace is rank.Initial —
// which the workspace's first issue is already holding. The rendered order
// cannot see that either. Two issues sharing a key still list in some order,
// decided by the id tiebreak, and only the keys say that nothing ranked them.
func TestCreateAtTopOfAnEmptyFrameTakesADistinctKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	first, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "frame", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(first) error = %v", err)
	}
	if first.Rank != rank.Initial() {
		t.Fatalf("the first issue in a workspace has rank %q, want the initial rank %q; this case rests on it holding the key an empty-bounds midpoint yields", first.Rank, rank.Initial())
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Childless epic", Topic: "frame", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}

	only, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Only child", Topic: "frame", IssueType: "task", ParentID: epic.ID, Placement: storage.RankTop})
	if err != nil {
		t.Fatalf("CreateIssue(first child, --top) error = %v", err)
	}
	if only.Rank == first.Rank {
		t.Errorf("the first child filed --top took rank %q, the key %s already holds; a rank orders one issue", only.Rank, first.ID)
	}
	// There is no sibling to lead, so it files where the default placement
	// would have filed it: past the workspace's last key, which is the epic's.
	if want := rank.After(epic.Rank); only.Rank != want {
		t.Errorf("the first child filed --top took rank %q, want %q = After(the workspace's last key %q)", only.Rank, want, epic.Rank)
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
// the child named — and the room beside that anchor used to be read with the
// frame's scope too. Nothing in the top level sat past the anchor, so the bound
// came back open, and the midpoint of an open span is a key something outside
// the frame was already holding.
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
	standalone, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone", Topic: "frame", IssueType: "task"})
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

// TestRankToEdgeOfAFrameWithNoRankedMemberFilesPastTheWorkspace covers the one
// input the room lookup has no answer for: an empty key.
//
// An issue whose own rank is blank is not in the population frameEdgeHolderTx
// reads, so a frame holding only that issue reports no edge at all, while the
// verb still counts the move as real. The comparison then reads that empty key
// differently at each end — nothing sorts below "" but every rank sorts above
// it — so asking for the room beside it sent an issue to its frame's BOTTOM and
// handed it a key above every issue in the workspace, an inversion no duplicate
// check would catch.
//
// A blank rank is not reachable through the API (ensureIssueRanks backfills at
// open), so it is written here directly: the point of the case is that the
// absent key is answered rather than averaged, whatever put it there.
func TestRankToEdgeOfAFrameWithNoRankedMemberFilesPastTheWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	standalone, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone", Topic: "frame", IssueType: "task"})
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
	// Sent to the bottom, it lands past everything — never above the key that
	// leads the workspace, which is what reading the room beside "" produced.
	if after.Rank < standalone.Rank {
		t.Errorf("the child sent to its frame's bottom holds %q, above the workspace's leading key %q", after.Rank, standalone.Rank)
	}
	if after.Rank < epic.Rank {
		t.Errorf("the child sent to its frame's bottom holds %q, above its own epic %q", after.Rank, epic.Rank)
	}
	for _, other := range []model.Issue{standalone, epic} {
		if after.Rank == other.Rank {
			t.Errorf("the child took rank %q, the key %s holds; a rank orders one issue", after.Rank, other.ID)
		}
	}
}
