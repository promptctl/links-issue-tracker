package conformance

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
)

// aPurgeRemovesTheIssueAndEverythingItOwns pins what a purge takes and what it
// leaves. It takes the issue's own data — the row, its comments, labels and
// history, and every edge with it at either end — and reports how many of each
// went. It leaves every other issue as it was, including the ones that name
// the purged id: a follow-up keeps its follows_up and a duplicate keeps its
// redirect target, the way a pasted sha outlives a rewritten commit. What is
// left of the purged issue is its purge record and nothing else.
func aPurgeRemovesTheIssueAndEverythingItOwns(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	doomed := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "doomed", Topic: "core", ParentID: epic.ID, Labels: []string{"leak"}, Plan: "a doomed plan"})
	// The blocked-by neighbors live in another epic: between two leaves of one
	// epic, rank orders the work and the edge is refused.
	neighbors := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "neighbors", Topic: "core", IssueType: model.TypeEpic})
	blocker := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "blocker", Topic: "core", ParentID: neighbors.ID})
	waiter := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "waiter", Topic: "core", ParentID: neighbors.ID, Plan: "a waiting plan"})
	followUp := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "follow-up", Topic: "core", ParentID: epic.ID, FollowsUp: doomed.ID})
	duplicate := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "duplicate", Topic: "core", ParentID: epic.ID})

	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: doomed.ID, Body: storagetest.CommentBody(t, "secret"), CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddComment error = %v", err)
	}
	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: waiter.ID, Body: storagetest.CommentBody(t, "mentions "+doomed.ID), CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddComment error = %v", err)
	}
	for _, planned := range []model.Issue{doomed, waiter} {
		if _, err := st.AddPlanReview(ctx, storage.AddPlanReviewInput{IssueID: planned.ID, Plan: string(model.DigestPlan(planned.Plan)), Verdict: "pass", CreatedBy: "grace"}); err != nil {
			t.Fatalf("AddPlanReview(%s) error = %v", planned.ID, err)
		}
	}
	for _, edge := range []storage.AddRelationInput{
		{SrcID: doomed.ID, DstID: blocker.ID, Type: model.RelBlockedBy, CreatedBy: "ada"},
		{SrcID: waiter.ID, DstID: doomed.ID, Type: model.RelBlockedBy, CreatedBy: "ada"},
		{SrcID: blocker.ID, DstID: waiter.ID, Type: model.RelRelatedTo, CreatedBy: "ada"},
	} {
		if _, err := st.AddRelation(ctx, edge); err != nil {
			t.Fatalf("AddRelation(%+v) error = %v", edge, err)
		}
	}
	if _, err := st.Apply(ctx, duplicate.ID, storage.Change{Action: model.Close{Outcome: model.Duplicate{Of: doomed.ID}}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply close-as-duplicate error = %v", err)
	}
	history, err := st.ListEvents(ctx, doomed.ID)
	if err != nil {
		t.Fatalf("ListEvents error = %v", err)
	}
	before := mustExport(t, ctx, st)

	clk.Pin(laterInstant)
	result, err := st.Purge(ctx, storage.PurgeInput{IssueID: doomed.ID, Actor: "grace"})
	if err != nil {
		t.Fatalf("Purge error = %v", err)
	}

	want := model.Purge{IssueID: doomed.ID, IssueCreatedAt: doomed.CreatedAt, PurgedAt: laterInstant, PurgedBy: "grace"}
	if !result.Purge.PurgedAt.Equal(want.PurgedAt) || !result.Purge.IssueCreatedAt.Equal(want.IssueCreatedAt) || result.Purge.IssueID != want.IssueID || result.Purge.PurgedBy != want.PurgedBy {
		t.Errorf("purge record = %+v, want %+v", result.Purge, want)
	}
	// Three edges touch the doomed issue: its parent edge, the blocker it
	// waits on, and the waiter that waits on it. The related-to edge between
	// two other issues is not its to take.
	counts := [5]int{result.Comments, result.Labels, result.Relations, result.Events, result.PlanReviews}
	if wantCounts := [5]int{1, 1, 3, len(history), 1}; counts != wantCounts {
		t.Errorf("purge counts (comments, labels, relations, events, plan reviews) = %v, want %v", counts, wantCounts)
	}
	if len(history) == 0 {
		t.Fatal("the doomed issue recorded no history, so the events count above asserts nothing")
	}

	_, err = st.GetIssue(ctx, doomed.ID)
	assertNotFound(t, err, "issue", "GetIssue(purged)")

	after := mustExport(t, ctx, st)
	assertNoRowOf(t, after, doomed.ID)
	if len(after.Purges) != 1 || after.Purges[0].IssueID != doomed.ID || !after.Purges[0].IssueCreatedAt.Equal(doomed.CreatedAt) || after.Purges[0].PurgedBy != "grace" || !after.Purges[0].PurgedAt.Equal(laterInstant) {
		t.Errorf("export purges = %+v, want exactly %+v", after.Purges, want)
	}
	// Every other issue's row is exactly what it was: the purge edits no text
	// and no column of another ticket, so the mentions stay.
	assertSameRows(t, before.ApplyPurges([]model.Purge{want}), after)
	if got := mustGet(t, ctx, st, followUp.ID).FollowsUp; got != doomed.ID {
		t.Errorf("follow-up FollowsUp = %q, want the purged id %q kept", got, doomed.ID)
	}
	if target := mustGet(t, ctx, st, duplicate.ID).RedirectTargetValue(); target == nil || *target != doomed.ID {
		t.Errorf("duplicate redirect target = %v, want the purged id %s kept", target, doomed.ID)
	}
}

// aPurgeRefusesAContainerThatHoldsTickets pins the containment half: purging
// an epic with a leaf under it would leave the leaf outside any epic, so it is
// refused, the refusal names the leaf, and nothing is removed. A child epic is
// no reason to refuse — it becomes top-level, which is legal — so once only
// the child epic is left the purge goes through and leaves it top-level.
func aPurgeRefusesAContainerThatHoldsTickets(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})
	childEpic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child epic", Topic: "core", IssueType: model.TypeEpic, ParentID: epic.ID})
	if _, err := st.Apply(ctx, child.ID, storage.Change{Action: model.Delete{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply delete error = %v", err)
	}

	_, err := st.Purge(ctx, storage.PurgeInput{IssueID: epic.ID, Actor: "ada"})
	assertRefused(t, err, "Purge(epic holding a deleted child)")
	if err != nil && (!strings.Contains(err.Error(), child.ID) || strings.Contains(err.Error(), childEpic.ID)) {
		t.Errorf("refusal = %q, want it to name the stranded leaf %s and not the child epic %s", err, child.ID, childEpic.ID)
	}
	mustGet(t, ctx, st, epic.ID)
	if purges := mustExport(t, ctx, st).Purges; len(purges) != 0 {
		t.Errorf("a refused purge left records %+v", purges)
	}

	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: child.ID, Actor: "ada"}); err != nil {
		t.Fatalf("Purge(child) error = %v", err)
	}
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: epic.ID, Actor: "ada"}); err != nil {
		t.Fatalf("Purge(epic holding only an epic) error = %v", err)
	}
	after := mustExport(t, ctx, st)
	assertNoRowOf(t, after, epic.ID)
	if got := mustGet(t, ctx, st, childEpic.ID); got.ID != childEpic.ID {
		t.Errorf("child epic = %+v, want it kept", got)
	}
	for _, rel := range after.Relations {
		if rel.SrcID == childEpic.ID && rel.Type == model.RelParentChild {
			t.Errorf("child epic still has a parent edge %+v, want it top-level", rel)
		}
	}
}

// aPurgeOfAMissingIssueIsNotFound pins absence: an id that never existed, and
// one already purged, are both NotFoundError — a second purge has no row to
// remove and does not write a second record.
func aPurgeOfAMissingIssueIsNotFound(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	_, err := st.Purge(ctx, storage.PurgeInput{IssueID: "no-such-issue", Actor: "ada"})
	assertNotFound(t, err, "issue", "Purge(missing)")

	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: epic.ID, Actor: "ada"}); err != nil {
		t.Fatalf("Purge error = %v", err)
	}
	_, err = st.Purge(ctx, storage.PurgeInput{IssueID: epic.ID, Actor: "grace"})
	assertNotFound(t, err, "issue", "Purge(already purged)")
	if purges := mustExport(t, ctx, st).Purges; len(purges) != 1 || purges[0].PurgedBy != "ada" {
		t.Errorf("purges after a repeated purge = %+v, want the first record alone", purges)
	}
}

// aPurgedIDIsNeverMintedAgain pins that a purge does not free its id. Minting
// is a hash of the ticket's content and the instant it was made, so with the
// clock pinned, the same ticket created again lands on the same candidate; a
// store that let the purged id go would hand it straight back, and every
// mention of the purged ticket elsewhere would then name the new one.
func aPurgedIDIsNeverMintedAgain(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(earlierInstant)
	in := storage.CreateIssueInput{Title: "same content", Topic: "core", IssueType: model.TypeEpic}
	first := mustCreate(t, ctx, st, in)
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: first.ID, Actor: "ada"}); err != nil {
		t.Fatalf("Purge error = %v", err)
	}
	second := mustCreate(t, ctx, st, in)
	if second.ID == first.ID {
		t.Errorf("re-created ticket minted the purged id %s", first.ID)
	}
}

func mustExport(t *testing.T, ctx context.Context, st storage.Store) model.Export {
	t.Helper()
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export error = %v", err)
	}
	return export
}

// assertNoRowOf fails for any row of the export that belongs to id.
func assertNoRowOf(t *testing.T, export model.Export, id string) {
	t.Helper()
	for _, issue := range export.Issues {
		if issue.ID == id {
			t.Errorf("export still holds issue %s", id)
		}
	}
	for _, rel := range export.Relations {
		if rel.SrcID == id || rel.DstID == id {
			t.Errorf("export still holds edge %s -%s-> %s", rel.SrcID, rel.Type, rel.DstID)
		}
	}
	for _, c := range export.Comments {
		if c.IssueID == id {
			t.Errorf("export still holds comment %s of %s", c.ID, id)
		}
	}
	for _, l := range export.Labels {
		if l.IssueID == id {
			t.Errorf("export still holds label %s of %s", l.Name, id)
		}
	}
	for _, ev := range export.Events {
		if ev.IssueID == id {
			t.Errorf("export still holds event %s of %s", ev.ID, id)
		}
	}
	for _, review := range export.PlanReviews {
		if review.IssueID == id {
			t.Errorf("export still holds plan review %s of %s", review.ID, id)
		}
	}
}

// assertSameRows compares the rows two exports hold for the issues both name,
// ignoring when each was taken. An issue row is compared by id, title,
// description and the follows_up and redirect columns that can name another
// ticket — the columns a purge must never touch on a survivor.
func assertSameRows(t *testing.T, want, got model.Export) {
	t.Helper()
	if len(want.Issues) != len(got.Issues) || len(want.Relations) != len(got.Relations) ||
		len(want.Comments) != len(got.Comments) || len(want.Labels) != len(got.Labels) || len(want.Events) != len(got.Events) ||
		len(want.PlanReviews) != len(got.PlanReviews) {
		t.Errorf("row counts (issues, relations, comments, labels, events, plan reviews) = (%d, %d, %d, %d, %d, %d), want (%d, %d, %d, %d, %d, %d)",
			len(got.Issues), len(got.Relations), len(got.Comments), len(got.Labels), len(got.Events), len(got.PlanReviews),
			len(want.Issues), len(want.Relations), len(want.Comments), len(want.Labels), len(want.Events), len(want.PlanReviews))
	}
	gotByID := map[string]model.Issue{}
	for _, issue := range got.Issues {
		gotByID[issue.ID] = issue
	}
	for _, w := range want.Issues {
		g, ok := gotByID[w.ID]
		if !ok {
			t.Errorf("issue %s is gone after purging another", w.ID)
			continue
		}
		if g.Title != w.Title || g.Description != w.Description || g.FollowsUp != w.FollowsUp || !sameTarget(g.RedirectTargetValue(), w.RedirectTargetValue()) {
			t.Errorf("issue %s changed: got %q/%q follows_up=%q, want %q/%q follows_up=%q", w.ID, g.Title, g.Description, g.FollowsUp, w.Title, w.Description, w.FollowsUp)
		}
	}
	gotBodies := map[string]string{}
	for _, c := range got.Comments {
		gotBodies[c.ID] = c.Body
	}
	for _, c := range want.Comments {
		if gotBodies[c.ID] != c.Body {
			t.Errorf("comment %s body = %q, want %q", c.ID, gotBodies[c.ID], c.Body)
		}
	}
}

func sameTarget(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
