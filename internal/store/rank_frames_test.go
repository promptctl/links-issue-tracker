package store

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// twoEpics files epics first and second at the top level, in that order, each
// with one task, and returns the four issues.
func twoEpics(t *testing.T, ctx context.Context, st *Store) (first, firstTask, second, secondTask model.Issue) {
	t.Helper()
	first = createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "first", Topic: "rank", IssueType: "epic"})
	second = createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "second", Topic: "rank", IssueType: "epic", Placement: storage.RankBottom})
	firstTask = createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "first task", Topic: "rank", IssueType: "task", ParentID: first.ID})
	secondTask = createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "second task", Topic: "rank", IssueType: "task", ParentID: second.ID})
	return first, firstTask, second, secondTask
}

func blockOrFatal(t *testing.T, ctx context.Context, st *Store, dependent, dependency string) {
	t.Helper()
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: dependent, DstID: dependency, Type: "blocked-by", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(%s blocked by %s): %v", dependent, dependency, err)
	}
}

// storedRanks reads every issue's key through the listing rather than the
// repair's own loader, so a loader that dropped an issue cannot hide it here.
func storedRanks(t *testing.T, ctx context.Context, st *Store) map[string]string {
	t.Helper()
	issues, err := st.ListIssues(ctx, storage.ListIssuesFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	ranks := make(map[string]string, len(issues))
	for _, issue := range issues {
		ranks[issue.ID] = issue.Rank
	}
	return ranks
}

func rankInversionsOf(t *testing.T, ctx context.Context, st *Store, id string) []string {
	t.Helper()
	relations, err := st.GetRelationsByIDs(ctx, []string{id})
	if err != nil {
		t.Fatalf("GetRelationsByIDs(%s): %v", id, err)
	}
	return relations[id].RankInversions
}

// A task waiting on a task in the epic listed after its own is inverted
// however the two tasks' own keys compare, because those keys are read in
// different frames. Doctor counts it, the repair moves an epic — the frame
// that holds both — and no task's key is written.
func TestARankInversionAcrossFramesIsReadAndRepairedInTreeOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	first, firstTask, second, secondTask := twoEpics(t, ctx, st)
	plantRank(t, ctx, st, firstTask.ID, "y")
	plantRank(t, ctx, st, secondTask.ID, "b")
	blockOrFatal(t, ctx, st, firstTask.ID, secondTask.ID)
	before := storedRanks(t, ctx, st)
	if !(before[secondTask.ID] < before[firstTask.ID]) || !(before[first.ID] < before[second.ID]) {
		t.Fatalf("fixture: ranks %v must rank the dependency's own key first and its epic second", before)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.RankInversions != 1 {
		t.Errorf("Doctor.RankInversions = %d, want 1", report.RankInversions)
	}
	if got := rankInversionsOf(t, ctx, st, firstTask.ID); !slices.Equal(got, []string{secondTask.ID}) {
		t.Errorf("RankInversions of %s = %v, want [%s]", firstTask.ID, got, secondTask.ID)
	}

	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions: %v", err)
	}
	if fixed != 1 {
		t.Errorf("FixRankInversions moved %d issue(s), want 1", fixed)
	}
	after := storedRanks(t, ctx, st)
	for _, task := range []model.Issue{firstTask, secondTask} {
		if after[task.ID] != before[task.ID] {
			t.Errorf("%s key %q -> %q; the repair must move an epic, not a task inside one", task.ID, before[task.ID], after[task.ID])
		}
	}
	if want := []string{second.ID, secondTask.ID, first.ID, firstTask.ID}; !slices.Equal(listedIDs(t, ctx, st), want) {
		t.Errorf("listing after repair = %v, want %v", listedIDs(t, ctx, st), want)
	}
	report, err = st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor after repair: %v", err)
	}
	if report.RankInversions != 0 {
		t.Errorf("Doctor.RankInversions after repair = %d, want 0", report.RankInversions)
	}
	if got := rankInversionsOf(t, ctx, st, firstTask.ID); len(got) != 0 {
		t.Errorf("RankInversions of %s after repair = %v, want none", firstTask.ID, got)
	}
}

// The other way round: the tasks' own keys put the dependency last, but its
// epic lists first, so nothing is inverted and the repair writes nothing.
func TestTreeOrderClearsWhatRawKeysWouldCallInverted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	_, firstTask, _, secondTask := twoEpics(t, ctx, st)
	plantRank(t, ctx, st, firstTask.ID, "y")
	plantRank(t, ctx, st, secondTask.ID, "b")
	blockOrFatal(t, ctx, st, secondTask.ID, firstTask.ID)

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.RankInversions != 0 {
		t.Errorf("Doctor.RankInversions = %d, want 0", report.RankInversions)
	}
	if got := rankInversionsOf(t, ctx, st, secondTask.ID); len(got) != 0 {
		t.Errorf("RankInversions of %s = %v, want none", secondTask.ID, got)
	}
	if fixed, err := st.FixRankInversions(ctx); err != nil || fixed != 0 {
		t.Errorf("FixRankInversions = %d, %v; want 0, nil", fixed, err)
	}
}

// An epic waiting on an epic inside it lists before it whatever any key
// holds, so the edge is no rank inversion and the repair has nothing to move.
// The store refuses that edge outright, so the inner epic is detached while
// it is written.
func TestAnEdgeToAnIssuesOwnContainerIsNoRankInversion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	outer, _, _, _ := twoEpics(t, ctx, st)
	inner := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "inner", Topic: "rank", IssueType: "epic", ParentID: outer.ID})
	createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "inner task", Topic: "rank", IssueType: "task", ParentID: inner.ID})
	if err := st.ClearParent(ctx, inner.ID); err != nil {
		t.Fatalf("ClearParent: %v", err)
	}
	blockOrFatal(t, ctx, st, outer.ID, inner.ID)
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: inner.ID, ParentID: outer.ID, CreatedBy: "tester"}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.RankInversions != 0 || len(report.DependencyCycle) != 0 {
		t.Errorf("Doctor = %d inversion(s), cycle %v; want neither", report.RankInversions, report.DependencyCycle)
	}
	if got := rankInversionsOf(t, ctx, st, outer.ID); len(got) != 0 {
		t.Errorf("RankInversions of %s = %v, want none", outer.ID, got)
	}
	if fixed, err := st.FixRankInversions(ctx); err != nil || fixed != 0 {
		t.Errorf("FixRankInversions = %d, %v; want 0, nil", fixed, err)
	}
}

// Two epics whose tasks wait on each other's have no tree order that puts
// each dependency first, though no edge closes a cycle among the tasks.
// Doctor names the cycle through the epics, and the repair refuses on the
// same path rather than writing an order that satisfies half of it.
func TestEpicsWhoseTasksWaitOnEachOtherAreACycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	first, firstTask, second, secondTask := twoEpics(t, ctx, st)
	firstOther := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "first other", Topic: "rank", IssueType: "task", ParentID: first.ID})
	secondOther := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "second other", Topic: "rank", IssueType: "task", ParentID: second.ID})
	blockOrFatal(t, ctx, st, firstTask.ID, secondTask.ID)
	blockOrFatal(t, ctx, st, secondOther.ID, firstOther.ID)
	before := storedRanks(t, ctx, st)

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	cycle := report.DependencyCycle
	if len(cycle) != 3 || cycle[0] != cycle[2] || !slices.Contains(cycle, first.ID) || !slices.Contains(cycle, second.ID) {
		t.Fatalf("Doctor.DependencyCycle = %v, want the two epics", cycle)
	}
	if !slices.ContainsFunc(report.Warnings, func(w string) bool { return strings.Contains(w, cycleMembers) }) {
		t.Errorf("Warnings = %v, want the cycle's warning to say what an epic on it stands for", report.Warnings)
	}
	_, err = st.FixRankInversions(ctx)
	if err == nil || !strings.Contains(err.Error(), strings.Join(cycle, " -> ")) {
		t.Fatalf("FixRankInversions error = %v, want a refusal naming %v", err, cycle)
	}
	if after := storedRanks(t, ctx, st); !maps.Equal(after, before) {
		t.Errorf("ranks after the refusal = %v, want them untouched %v", after, before)
	}
}

// A mover the repair spaces between two live issues lands beside a closed one
// that holds the key the same arithmetic over those two neighbours gives — the
// key a repair over live issues alone writes.
// Every placed issue keeps a key of its own, and the live order is the one the
// edge asks for.
func TestTheRankRepairWritesNoKeyAClosedIssueHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic := func(title string) model.Issue {
		e := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "rank", IssueType: "epic", Placement: storage.RankBottom})
		createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: title + " task", Topic: "rank", IssueType: "task", ParentID: e.ID})
		return e
	}
	p, q, r, holder := epic("p"), epic("q"), epic("r"), epic("holder")
	closed := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "closed", Topic: "rank", IssueType: "task", ParentID: holder.ID})
	if _, err := st.Apply(ctx, closed.ID, storage.Change{Action: model.Close{Outcome: model.Obsolete{}}, Actor: "tester"}); err != nil {
		t.Fatalf("close %s: %v", closed.ID, err)
	}
	// The key a repair over live issues alone writes for the one issue it
	// moves.
	order, err := loadRankOrder(ctx, st.db)
	if err != nil {
		t.Fatalf("loadRankOrder: %v", err)
	}
	ancestry, err := loadRankAncestry(ctx, st.db)
	if err != nil {
		t.Fatalf("loadRankAncestry: %v", err)
	}
	live := slices.DeleteFunc(order, func(item rankedIssue) bool { return item.id == closed.ID })
	liveOnly, err := repairRankOrder(live, rankConstraints([]blocksEdge{{dependent: p.ID, dependency: q.ID}}, ancestry))
	if err != nil || len(liveOnly) != 1 {
		t.Fatalf("repair over live issues alone = %v, %v; want one write", liveOnly, err)
	}
	plantRank(t, ctx, st, closed.ID, liveOnly[0].newRank)
	blockOrFatal(t, ctx, st, p.ID, q.ID)

	if _, err := st.FixRankInversions(ctx); err != nil {
		t.Fatalf("FixRankInversions: %v", err)
	}
	ranks := storedRanks(t, ctx, st)
	holders := map[string]string{}
	for id, key := range ranks {
		if other, taken := holders[key]; taken {
			t.Errorf("%s and %s both hold %q after the repair", other, id, key)
		}
		holders[key] = id
	}
	if !(ranks[q.ID] < ranks[p.ID] && ranks[p.ID] < ranks[r.ID]) {
		t.Errorf("ranks q=%q p=%q r=%q, want q, p, r in that order", ranks[q.ID], ranks[p.ID], ranks[r.ID])
	}
}
