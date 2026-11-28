package conformance

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/selector"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
)

// selectorWorkspace is one workspace every selector case reads, built so each
// kind, op and path has an issue that holds it and one that does not.
//
//	home (epic)            alpha, gamma, nested, xray (archived), yankee (deleted)
//	  nested (epic)        delta, echo
//	away (epic, area)      beta, follow, dup
//	done (epic)            hotel, zulu (archived)
//	empty (epic)
//	shelved (epic, archived)  papa, romeo (archived)
type selectorWorkspace struct {
	home, nested, away, done, empty, shelved                  model.ID
	alpha, beta, gamma, delta, echo, follow, dup, hotel, papa model.ID
	xray, yankee, zulu, romeo                                 model.ID
}

func buildSelectorWorkspace(t *testing.T, ctx context.Context, st storage.Store, clk *clock) selectorWorkspace {
	t.Helper()
	// Every write is stamped by the case's clock, so the one write stamped
	// later is the one a stamp atom read against now can tell apart.
	clk.Pin(earlierInstant)
	create := func(in storage.CreateIssueInput) model.ID {
		t.Helper()
		if in.Lane == (storage.Lane{}) {
			in.Lane = storage.SharedLane
		}
		if in.Topic == "" {
			in.Topic = "core"
		}
		return mustCreate(t, ctx, st, in).ID
	}
	apply := func(id model.ID, action model.Action) {
		t.Helper()
		if _, err := st.Apply(ctx, id, storage.Change{Action: action, Actor: "ada"}); err != nil {
			t.Fatalf("Apply %s to %s error = %v", action.Name(), id, err)
		}
	}
	epic := func(title string, parent model.ID, labels ...string) model.ID {
		return create(storage.CreateIssueInput{Title: storage.MustParseTitle(title), IssueType: model.TypeEpic, ParentID: parent, Labels: labels})
	}
	leaf := func(title string, parent model.ID) model.ID {
		return create(storage.CreateIssueInput{Title: storage.MustParseTitle(title), ParentID: parent})
	}

	var w selectorWorkspace
	w.home = epic("home", "")
	w.nested = epic("nested", w.home)
	w.away = epic("away", "", "area")
	w.done = epic("done", "")
	w.empty = epic("empty", "")
	w.shelved = epic("shelved", "")

	w.alpha = create(storage.CreateIssueInput{
		Title: storage.MustParseTitle("Alpha widget"), Topic: "renderer", ParentID: w.home,
		Priority: model.PriorityUrgent, Labels: []string{"perf", "ui"},
		Description: "Burns 100% of the cpu_time", Prompt: "port it", Plan: "first step",
	})
	w.beta = create(storage.CreateIssueInput{
		Title: storage.MustParseTitle("Beta gadget"), Topic: "parser", IssueType: model.TypeBug, ParentID: w.away,
		Assignee: "grace", Lane: storage.MustParseLane("night"), Description: "a cpuxtime spike in 2010",
	})
	w.gamma = leaf("Gamma closing", w.home)
	w.delta = leaf("Delta deep", w.nested)
	w.echo = leaf("Echo plan", w.nested)
	w.follow = create(storage.CreateIssueInput{Title: storage.MustParseTitle("Follow on"), ParentID: w.away, FollowsUp: w.alpha})
	w.dup = leaf("Dup copy", w.away)
	w.hotel = leaf("Hotel done", w.done)
	w.papa = leaf("Papa kept", w.shelved)
	w.xray = leaf("Xray shelved", w.home)
	w.yankee = leaf("Yankee gone", w.home)
	w.zulu = leaf("Zulu shelved", w.done)
	w.romeo = leaf("Romeo shelved", w.shelved)

	if _, err := st.Apply(ctx, w.alpha, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply start alpha error = %v", err)
	}
	apply(w.alpha, model.SetPhase{Phase: model.PhaseImpl})
	// delta reaches review and is reclaimed by ada, which moves no status and
	// so keeps the phase the work had reached; echo is started and no more,
	// so it is in plan.
	for _, id := range []model.ID{w.delta, w.echo} {
		if _, err := st.Apply(ctx, id, storage.Change{Action: model.Start{Assignee: "grace"}, Actor: "grace"}); err != nil {
			t.Fatalf("Apply start %s error = %v", id, err)
		}
	}
	apply(w.delta, model.SetPhase{Phase: model.PhaseReview})
	if _, err := st.Apply(ctx, w.delta, storage.Change{Action: model.Start{Assignee: "ada", Reclaim: true}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply reclaim delta error = %v", err)
	}
	apply(w.gamma, model.Close{Outcome: model.Wontfix{}})
	apply(w.dup, model.Close{Outcome: model.Duplicate{Of: string(w.alpha)}})
	apply(w.hotel, model.Done{})
	apply(w.papa, model.Done{})
	for _, id := range []model.ID{w.xray, w.zulu, w.romeo} {
		apply(id, model.Archive{})
	}
	apply(w.yankee, model.Delete{})
	// Archived after its children settled, the shelved epic keeps the state it
	// left with: papa done and romeo open.
	apply(w.shelved, model.Archive{})

	for _, edge := range []storage.AddRelationInput{
		{SrcID: w.beta, DstID: w.alpha, Type: model.RelBlockedBy, CreatedBy: "ada"},
		{SrcID: w.gamma, DstID: w.alpha, Type: model.RelRelatedTo, CreatedBy: "ada"},
	} {
		if _, err := st.AddRelation(ctx, edge); err != nil {
			t.Fatalf("AddRelation %+v error = %v", edge, err)
		}
	}
	for _, id := range []model.ID{w.alpha, w.alpha, w.beta} {
		if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: id, Body: storagetest.CommentBody(t, "note"), CreatedBy: "ada"}); err != nil {
			t.Fatalf("AddComment on %s error = %v", id, err)
		}
	}
	if _, err := st.AddPlanReview(ctx, storage.AddPlanReviewInput{IssueID: w.alpha, Plan: string(model.DigestPlan("first step")), Verdict: "pass", CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddPlanReview error = %v", err)
	}

	clk.Pin(laterInstant)
	if _, err := st.Apply(ctx, w.beta, storage.Change{Actor: "grace", Fields: storage.UpdateIssueInput{Title: ptr("Beta gadget v2")}}); err != nil {
		t.Fatalf("Apply retitle beta error = %v", err)
	}
	return w
}

// live is every issue of the workspace a read that admits neither archived nor
// deleted issues holds.
func (w selectorWorkspace) live() []model.ID {
	return []model.ID{w.home, w.nested, w.away, w.done, w.empty, w.alpha, w.beta, w.gamma, w.delta, w.echo, w.follow, w.dup, w.hotel, w.papa}
}

// except is the live issues other than out.
func (w selectorWorkspace) except(out ...model.ID) []model.ID {
	return slices.DeleteFunc(w.live(), func(id model.ID) bool { return slices.Contains(out, id) })
}

// selectorCase is one selector and the issues it selects, among the issues the
// universe admits.
type selectorCase struct {
	selector string
	want     []model.ID
}

// assertSelects holds both reads of a selection to the issues want names: the
// ids alone, and the listing.
func assertSelects(t *testing.T, ctx context.Context, st storage.Store, clk *clock, universe storage.Selection, tc selectorCase) {
	t.Helper()
	universe.Selector = mustParse(t, clk, tc.selector)
	want := slices.Clone(tc.want)
	slices.Sort(want)
	ids, err := st.SelectIssueIDs(ctx, universe)
	if err != nil {
		t.Fatalf("SelectIssueIDs(%q) error = %v", tc.selector, err)
	}
	assertStrings(t, "the ids "+tc.selector+" selects", ids, want)
	listed := mustList(t, ctx, st, storage.Listing{Selection: universe})
	got := make([]model.ID, 0, len(listed))
	for _, issue := range listed {
		got = append(got, issue.ID)
	}
	slices.Sort(got)
	assertStrings(t, "the issues "+tc.selector+" lists", got, want)
}

// aSelectorReadsEveryKindAlike states each kind's ops over the fields that
// have it: a value list is any of its values, != is the negation of = and so
// holds of an issue with no value, ~ is a case-blind fragment whose % and _
// mean themselves, and a stamp compares as the instant it spells, now among
// them.
func aSelectorReadsEveryKindAlike(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	w := buildSelectorWorkspace(t, ctx, st, clk)
	for _, tc := range []selectorCase{
		{`title="Alpha widget"`, []model.ID{w.alpha}},
		{"title~WIDGET", []model.ID{w.alpha}},
		{`description~"100%"`, []model.ID{w.alpha}},
		{`description~"0%"`, []model.ID{w.alpha}},
		{"description~cpu_time", []model.ID{w.alpha}},
		{"topic=parser", []model.ID{w.beta}},
		{"topic~rend", []model.ID{w.alpha}},
		{"assignee=ada,grace", []model.ID{w.alpha, w.beta, w.delta, w.echo}},
		{"assignee", []model.ID{w.alpha, w.beta, w.delta, w.echo}},
		{"!assignee", w.except(w.alpha, w.beta, w.delta, w.echo)},
		{"assignee!=ada", w.except(w.alpha, w.delta)},
		{"lane=night", []model.ID{w.beta}},
		{"prompt~port", []model.ID{w.alpha}},
		{"plan", []model.ID{w.alpha}},

		{"type=bug", []model.ID{w.beta}},
		{"type=epic", []model.ID{w.home, w.nested, w.away, w.done, w.empty}},
		{"type!=epic,task", []model.ID{w.beta}},
		{"priority=urgent", []model.ID{w.alpha}},
		{"priority!=urgent", w.except(w.alpha)},
		{"resolution=wontfix", []model.ID{w.gamma}},
		{"resolution=duplicate", []model.ID{w.dup}},
		{"resolution", []model.ID{w.gamma, w.dup}},
		{"!resolution status=closed", []model.ID{w.done, w.hotel, w.papa}},
		{"phase=impl", []model.ID{w.alpha}},
		{"phase=review", []model.ID{w.delta}},
		{"phase=plan", []model.ID{w.echo}},
		{"phase=plan,impl", []model.ID{w.alpha, w.echo}},
		{"phase!=impl", w.except(w.alpha)},
		{"!phase", w.except(w.alpha, w.delta, w.echo)},

		{"updated_at>=now", []model.ID{w.beta}},
		{"updated_at<now", w.except(w.beta)},
		{"created_at<=now-1h", nil},
		{"created_at>now-1d", w.live()},
		{"closed_at", []model.ID{w.gamma, w.dup, w.hotel, w.papa}},
		{"closed_at<" + model.StampText(laterInstant), []model.ID{w.gamma, w.dup, w.hotel, w.papa}},

		{"text~gamma", []model.ID{w.gamma}},
		{"text~renderer", []model.ID{w.alpha}},
		{`text~"port it"`, []model.ID{w.alpha}},
		{`text~"first step"`, []model.ID{w.alpha}},

		{"labels=perf labels=ui", []model.ID{w.alpha}},
		{"labels~er", []model.ID{w.alpha}},
		{"!labels", w.except(w.alpha, w.away)},
		{"labels/count=2", []model.ID{w.alpha}},
		{"labels/count!=0", []model.ID{w.alpha, w.away}},
		{"labels/count>1", []model.ID{w.alpha}},
		{"labels/count<=1", w.except(w.alpha)},
	} {
		assertSelects(t, ctx, st, clk, storage.Selection{}, tc)
	}
}

// aSelectorReadsAcrossEdges states the paths: a step reads every issue its
// edge reaches, a filter keeps those it holds of, a count counts them, and an
// atom over a path holds when some value the path reads satisfies it.
func aSelectorReadsAcrossEdges(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	w := buildSelectorWorkspace(t, ctx, st, clk)
	id := func(id model.ID) string { return string(id) }
	// long names ids among sixty no issue has: a list longer than an engine
	// may bind one value at a time still reads as the list.
	long := func(ids ...model.ID) string {
		names := make([]string, 0, len(ids)+60)
		for _, id := range ids {
			names = append(names, string(id))
		}
		for n := range 60 {
			names = append(names, fmt.Sprintf("no-such-%d", n))
		}
		return strings.Join(names, ",")
	}
	for _, tc := range []selectorCase{
		{"parent=" + id(w.home), []model.ID{w.alpha, w.gamma, w.nested}},
		{"id=" + long(w.alpha, w.beta), []model.ID{w.alpha, w.beta}},
		{"id!=" + long(w.alpha), w.except(w.alpha)},
		{"parent=" + long(w.home, w.nested), []model.ID{w.alpha, w.gamma, w.nested, w.delta, w.echo}},
		{"parent/type=epic", []model.ID{w.alpha, w.beta, w.gamma, w.delta, w.echo, w.follow, w.dup, w.hotel, w.nested}},
		{"parent/labels=area", []model.ID{w.beta, w.follow, w.dup}},
		{"parent/parent=" + id(w.home), []model.ID{w.delta, w.echo}},
		{"parent=no-such-issue", nil},
		{"follows_up=no-such-issue", nil},
		{"!blocked_by", w.except(w.beta)},
		{"parent(status=closed)", []model.ID{w.hotel}},
		{"children", []model.ID{w.home, w.nested, w.away, w.done}},
		{"children/count=3", []model.ID{w.home, w.away}},
		{"children/count=0", w.except(w.home, w.nested, w.away, w.done)},
		{"children(status=closed)/count=1", []model.ID{w.home, w.away, w.done}},
		{"children(status=in_progress)", []model.ID{w.home, w.nested}},
		{"children(type=bug,task status=open)", []model.ID{w.away}},
		{"siblings=" + id(w.gamma), []model.ID{w.alpha, w.nested}},
		{"siblings/type=bug", []model.ID{w.follow, w.dup}},
		{"siblings/count=2", []model.ID{w.alpha, w.gamma, w.nested, w.beta, w.follow, w.dup}},
		{"blocked_by=" + id(w.alpha), []model.ID{w.beta}},
		{"blocks=" + id(w.beta), []model.ID{w.alpha}},
		{"related=" + id(w.alpha), []model.ID{w.gamma}},
		{"related=" + id(w.gamma), []model.ID{w.alpha}},
		{"follows_up=" + id(w.alpha), []model.ID{w.follow}},
		{"follow_ups/count=1", []model.ID{w.alpha}},
		{"redirect_target=" + id(w.alpha), []model.ID{w.dup}},
		{"redirect_target/status=in_progress", []model.ID{w.dup}},
		{"comments/count=2", []model.ID{w.alpha}},
		{"comments", []model.ID{w.alpha, w.beta}},
		{"!comments", w.except(w.alpha, w.beta)},
		{"comments/count<2", w.except(w.alpha)},
		{"plan_reviews/count=1", []model.ID{w.alpha}},
		{"type=epic /issues(status=open)/count=3", []model.ID{w.home, w.nested, w.away, w.done, w.empty}},
		{"/issues(type=bug)/count=2", nil},
	} {
		assertSelects(t, ctx, st, clk, storage.Selection{}, tc)
	}
}

// aSelectorReadsAnEpicsDerivedState states that an epic's status is its
// leaves' at every depth, read the same way through a path as at the issue.
func aSelectorReadsAnEpicsDerivedState(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	w := buildSelectorWorkspace(t, ctx, st, clk)
	for _, tc := range []selectorCase{
		// home holds alpha started, gamma closed and, through nested, delta
		// started; done holds hotel closed and zulu archived, which a live epic
		// does not count; empty holds nothing.
		{"status=in_progress", []model.ID{w.home, w.nested, w.away, w.alpha, w.delta, w.echo}},
		{"status=closed", []model.ID{w.done, w.gamma, w.dup, w.hotel, w.papa}},
		{"status=open", []model.ID{w.empty, w.beta, w.follow}},
		{"status!=open type=epic", []model.ID{w.home, w.nested, w.away, w.done}},
		{"parent/status=closed", []model.ID{w.hotel}},
		{"children/status=in_progress", []model.ID{w.home, w.nested}},
	} {
		assertSelects(t, ctx, st, clk, storage.Selection{}, tc)
	}
}

// aSelectionsUniverseHoldsAtEveryStep states that the issues a read admits
// are the issues every path reads: an archived parent is no parent to a read
// that admits none, and an admitted one is. An epic's state is not the
// universe's: a live epic counts its live leaves whatever the read admits, and
// an archived one every leaf.
func aSelectionsUniverseHoldsAtEveryStep(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	w := buildSelectorWorkspace(t, ctx, st, clk)
	id := func(id model.ID) string { return string(id) }
	archived := storage.Selection{IncludeArchived: true}
	deleted := storage.Selection{IncludeDeleted: true}
	both := storage.Selection{IncludeArchived: true, IncludeDeleted: true}
	for _, tc := range []struct {
		universe storage.Selection
		selectorCase
	}{
		{storage.Selection{}, selectorCase{"!parent", []model.ID{w.home, w.away, w.done, w.empty, w.papa}}},
		{archived, selectorCase{"!parent", []model.ID{w.home, w.away, w.done, w.empty, w.shelved}}},
		{storage.Selection{}, selectorCase{"parent=" + id(w.home), []model.ID{w.alpha, w.gamma, w.nested}}},
		{archived, selectorCase{"parent=" + id(w.home), []model.ID{w.alpha, w.gamma, w.nested, w.xray}}},
		{deleted, selectorCase{"parent=" + id(w.home), []model.ID{w.alpha, w.gamma, w.nested, w.yankee}}},
		{both, selectorCase{"parent=" + id(w.home), []model.ID{w.alpha, w.gamma, w.nested, w.xray, w.yankee}}},
		{archived, selectorCase{"type=epic children/count=2", []model.ID{w.nested, w.done, w.shelved}}},
		{archived, selectorCase{"status=in_progress", []model.ID{w.home, w.nested, w.away, w.alpha, w.delta, w.echo, w.shelved}}},
		{archived, selectorCase{"status=closed type=epic", []model.ID{w.done}}},
	} {
		assertSelects(t, ctx, st, clk, tc.universe, tc.selectorCase)
	}
}

// everyFieldAnswers reads every field of the issue table in each form its
// kind admits, so a field with no reading in an engine fails here rather than
// at the first person who names it.
func everyFieldAnswers(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	buildSelectorWorkspace(t, ctx, st, clk)
	for _, f := range storage.IssueTable.Fields() {
		forms := []string{"!" + f.Name()}
		switch f.Kind() {
		case selector.Search:
			forms = []string{f.Name() + "~x"}
		case selector.Rows, selector.Texts, selector.Records:
			forms = append(forms, f.Name()+"/count>0")
		}
		if f.Of() != nil {
			forms = append(forms, f.Name()+"(id=x)")
		}
		for _, form := range forms {
			for _, universe := range []storage.Selection{{}, {IncludeArchived: true, IncludeDeleted: true}} {
				universe.Selector = mustParse(t, clk, form)
				if _, err := st.SelectIssueIDs(ctx, universe); err != nil {
					t.Errorf("SelectIssueIDs(%q) error = %v", form, err)
				}
			}
		}
	}
}
