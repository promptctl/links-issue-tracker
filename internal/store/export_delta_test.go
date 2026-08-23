package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The delta is the reconcile replay's write path, and a delta that is subtly
// wrong does not fail loudly — it lands a commit whose contents quietly
// disagree with the history it claims to replay. So the contract asserted here
// is total and behavioral: applying the delta must leave the tables in EXACTLY
// the state the wholesale rewrite would have left them in, for every shape of
// change. A caller can then never tell which path ran, which is the only
// property that makes the fast path safe to prefer.

// TestExportDeltaMatchesFullRewriteAcrossEveryChangeShape drives the delta and
// the clear-and-rewrite down two identical stores through the same sequence of
// backlog states, and after EVERY transition demands the two exports agree.
// Divergence at any step fails, so a bug is reported at the transition that
// caused it rather than at whatever later state exposed it.
func TestExportDeltaMatchesFullRewriteAcrossEveryChangeShape(t *testing.T) {
	ctx := context.Background()
	states := buildDeltaScenarioStates(t, ctx)
	if len(states) < 2 {
		t.Fatalf("scenario built %d states, want several transitions to exercise", len(states))
	}

	// Two stores stepped through the same states by different means: one always
	// rewrites wholesale, the other always applies the delta from the state it
	// last held. [LAW:behavior-not-structure] the assertion is the observable
	// export, never which statements ran.
	rewritten := openIssueStore(t, ctx)
	delta := openIssueStore(t, ctx)
	held := model.Export{}

	for i, state := range states {
		if err := rewritten.replaceFromExport(ctx, state.export, commitStamp{Message: "rewrite"}); err != nil {
			t.Fatalf("step %d (%s): full rewrite: %v", i, state.name, err)
		}
		if err := applyDeltaForTest(ctx, delta, held, state.export); err != nil {
			t.Fatalf("step %d (%s): apply delta: %v", i, state.name, err)
		}
		held = state.export

		want, err := rewritten.Export(ctx)
		if err != nil {
			t.Fatalf("step %d (%s): export rewritten store: %v", i, state.name, err)
		}
		got, err := delta.Export(ctx)
		if err != nil {
			t.Fatalf("step %d (%s): export delta store: %v", i, state.name, err)
		}
		assertSameRows(t, i, state.name, want, got)
	}
}

// TestExportDeltaRewritesARelationChangedOutsideItsKey pins the enumeration gap
// a key-only diff would leak: relations carry created_at/created_by OUTSIDE
// their (src_id, dst_id, type) primary key, so two relations can share a key
// and still differ. A diff that matched on the key alone would call this pair
// unchanged and silently keep the stale row forever.
func TestExportDeltaRewritesARelationChangedOutsideItsKey(t *testing.T) {
	relation := model.Relation{SrcID: "a", DstID: "b", Type: model.RelBlocks, CreatedAt: time.Unix(0, 0).UTC(), CreatedBy: "first"}
	restamped := relation
	restamped.CreatedBy = "second"

	issues := []model.Issue{{ID: "a"}, {ID: "b"}}
	prev := model.Export{Issues: issues, Relations: []model.Relation{relation}}
	next := model.Export{Issues: issues, Relations: []model.Relation{restamped}}

	got := diffExports(prev, next)
	if len(got.issues.remove) != 0 || len(got.issues.add) != 0 {
		t.Fatalf("issues delta = remove %v add %v, want untouched: only the relation changed", got.issues.remove, got.issues.add)
	}
	wantKey := relationKey{srcID: "a", dstID: "b", kind: model.RelBlocks}
	if len(got.relations.remove) != 1 || got.relations.remove[0] != wantKey {
		t.Fatalf("relation removals = %v, want exactly %v: a value change under a stable key must delete the stale row", got.relations.remove, wantKey)
	}
	if len(got.relations.add) != 1 || got.relations.add[0].CreatedBy != "second" {
		t.Fatalf("relation additions = %v, want the restamped row", got.relations.add)
	}
}

// TestExportDeltaReinsertsChildrenOfARewrittenIssue pins the cascade. Every
// child table is ON DELETE CASCADE from issues, so rewriting an issue row takes
// its comments, labels, events and relations with it — including a relation
// whose OTHER endpoint never changed. A delta that reasoned only about which
// rows differ, ignoring what the delete drags away, would leave those rows
// missing with no error anywhere.
func TestExportDeltaReinsertsChildrenOfARewrittenIssue(t *testing.T) {
	touched := model.Issue{ID: "touched", Title: "before"}
	retitled := touched
	retitled.Title = "after"
	untouched := model.Issue{ID: "untouched"}

	comment := model.Comment{ID: "c1", IssueID: "touched", Body: "unchanged"}
	label := model.Label{IssueID: "touched", Name: "keep"}
	event := model.IssueEvent{ID: "e1", IssueID: "touched"}
	// Spans the rewritten issue and the untouched one: cascades away with
	// "touched", so it must come back even though the row itself never changed.
	spanning := model.Relation{SrcID: "touched", DstID: "untouched", Type: model.RelBlocks}

	prev := model.Export{
		Issues:    []model.Issue{touched, untouched},
		Relations: []model.Relation{spanning},
		Comments:  []model.Comment{comment},
		Labels:    []model.Label{label},
		Events:    []model.IssueEvent{event},
	}
	next := prev
	next.Issues = []model.Issue{retitled, untouched}

	got := diffExports(prev, next)
	if len(got.issues.remove) != 1 || got.issues.remove[0] != "touched" {
		t.Fatalf("issue removals = %v, want [touched]", got.issues.remove)
	}
	if len(got.issues.add) != 1 || got.issues.add[0].Title != "after" {
		t.Fatalf("issue additions = %v, want the retitled row only", got.issues.add)
	}
	// Each child cascaded away with the issue, so each must be re-added — and
	// none may be queued for deletion, because the cascade already removed it.
	if len(got.relations.add) != 1 || len(got.relations.remove) != 0 {
		t.Fatalf("relations delta = remove %v add %v, want the spanning edge re-added and nothing deleted", got.relations.remove, got.relations.add)
	}
	if len(got.comments.add) != 1 || len(got.comments.remove) != 0 {
		t.Fatalf("comments delta = remove %v add %v, want the comment re-added", got.comments.remove, got.comments.add)
	}
	if len(got.labels.add) != 1 || len(got.labels.remove) != 0 {
		t.Fatalf("labels delta = remove %v add %v, want the label re-added", got.labels.remove, got.labels.add)
	}
	if len(got.events.add) != 1 || len(got.events.remove) != 0 {
		t.Fatalf("events delta = remove %v add %v, want the event re-added", got.events.remove, got.events.add)
	}
}

// TestExportDeltaLeavesAnUnchangedBacklogAlone is the property the whole
// optimization rests on: a folded commit that changed nothing in the backlog
// must produce no SQL at all. If this regresses, every step pays a full rewrite
// again and the replay is back to O(chain × backlog) writes with no test
// failing to say so.
func TestExportDeltaLeavesAnUnchangedBacklogAlone(t *testing.T) {
	export := model.Export{
		Issues:    []model.Issue{{ID: "a", Title: "t"}, {ID: "b"}},
		Relations: []model.Relation{{SrcID: "a", DstID: "b", Type: model.RelBlocks}},
		Comments:  []model.Comment{{ID: "c", IssueID: "a"}},
		Labels:    []model.Label{{IssueID: "a", Name: "l"}},
		Events:    []model.IssueEvent{{ID: "e", IssueID: "a"}},
	}
	if got := diffExports(export, export); !got.empty() {
		t.Fatalf("diff of an export against itself = %+v, want no work", got)
	}
}

// TestExportDeltaDropsARemovedIssueWithoutResurrectingItsChildren checks the
// deletion direction: the issue goes, and nothing that hung off it is queued
// for re-insertion (which would fail the foreign key) or for deletion (which
// the cascade already did).
func TestExportDeltaDropsARemovedIssueWithoutResurrectingItsChildren(t *testing.T) {
	prev := model.Export{
		Issues:   []model.Issue{{ID: "gone"}, {ID: "stays"}},
		Comments: []model.Comment{{ID: "c", IssueID: "gone"}},
		Events:   []model.IssueEvent{{ID: "e", IssueID: "gone"}},
	}
	next := model.Export{Issues: []model.Issue{{ID: "stays"}}}

	got := diffExports(prev, next)
	if len(got.issues.remove) != 1 || got.issues.remove[0] != "gone" {
		t.Fatalf("issue removals = %v, want [gone]", got.issues.remove)
	}
	if len(got.issues.add) != 0 {
		t.Fatalf("issue additions = %v, want none", got.issues.add)
	}
	if !got.comments.empty() || !got.events.empty() {
		t.Fatalf("children delta = comments %+v events %+v, want no work: the cascade owns them", got.comments, got.events)
	}
}

// deltaScenarioState is one backlog state in the sequence the equivalence test
// walks, named so a failure reports which transition broke.
type deltaScenarioState struct {
	name   string
	export model.Export
}

// buildDeltaScenarioStates drives a real store through the change shapes a
// folded chain produces — creation, field edits, comments, labels, relations,
// re-parenting, and removal — snapshotting the export after each. Building the
// states through the ordinary API rather than by hand keeps them valid,
// hydrated, and representative of what the replay will actually diff.
func buildDeltaScenarioStates(t *testing.T, ctx context.Context) []deltaScenarioState {
	t.Helper()
	st := openIssueStore(t, ctx)
	var states []deltaScenarioState
	snapshot := func(name string) {
		t.Helper()
		export, err := st.Export(ctx)
		if err != nil {
			t.Fatalf("export after %q: %v", name, err)
		}
		states = append(states, deltaScenarioState{name: name, export: export})
	}

	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "delta", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic): %v", err)
	}
	first, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "First", Topic: "delta", IssueType: "task", ParentID: epic.ID, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(first): %v", err)
	}
	second, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Second", Topic: "delta", IssueType: "task", ParentID: epic.ID, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(second): %v", err)
	}
	snapshot("epic with two children")

	if _, err := st.Apply(ctx, first.ID, Change{Fields: UpdateIssueInput{Lane: strptr("alpha")}}); err != nil {
		t.Fatalf("Apply(lane): %v", err)
	}
	snapshot("one field edited on one issue")

	if _, _, err := st.AddComment(ctx, AddCommentInput{IssueID: first.ID, Body: "a comment", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	snapshot("comment added")

	if _, err := st.AddLabel(ctx, AddLabelInput{IssueID: second.ID, Name: "urgent", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	snapshot("label added")

	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: first.ID, DstID: second.ID, Type: model.RelBlocks, CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(blocks): %v", err)
	}
	snapshot("relation spanning two issues added")

	// Retitling the BLOCKING endpoint cascades the edge away — the case a
	// key-wise diff gets wrong — so the delta must restore it.
	if _, err := st.Apply(ctx, first.ID, Change{Fields: UpdateIssueInput{Title: strptr("First, retitled")}}); err != nil {
		t.Fatalf("Apply(title): %v", err)
	}
	snapshot("relation endpoint rewritten")

	third, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Third", Topic: "delta", IssueType: "task", ParentID: epic.ID, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(third): %v", err)
	}
	snapshot("issue added")

	// Whole-row removal has no ordinary API — routine deletion is a soft
	// DeletedAt stamp — but a merge projection reaches it whenever an id exists
	// on only one side, so the delta must handle it. Derive that state by
	// dropping the issue and everything hanging off it from the last export,
	// which is precisely the shape merge.ThreeWay would hand the replay.
	states = append(states, deltaScenarioState{
		name:   "issue removed outright",
		export: withoutIssue(states[len(states)-1].export, third.ID),
	})

	return states
}

// withoutIssue drops an issue and every row that hangs off it, producing the
// export a merge projection yields for an id that exists on only one side.
func withoutIssue(export model.Export, id string) model.Export {
	export.Issues = filterRows(export.Issues, func(i model.Issue) bool { return i.ID != id })
	export.Relations = filterRows(export.Relations, func(r model.Relation) bool { return r.SrcID != id && r.DstID != id })
	export.Comments = filterRows(export.Comments, func(c model.Comment) bool { return c.IssueID != id })
	export.Labels = filterRows(export.Labels, func(l model.Label) bool { return l.IssueID != id })
	export.Events = filterRows(export.Events, func(e model.IssueEvent) bool { return e.IssueID != id })
	return export
}

// applyDeltaForTest lands the prev->next transition on a store through the same
// pure diff and applier the replay uses, under the ordinary mutation pipeline.
func applyDeltaForTest(ctx context.Context, st *Store, prev, next model.Export) error {
	return st.withMutation(ctx, "apply delta", func(ctx context.Context, tx *sql.Tx) error {
		return applyExportDelta(ctx, tx, diffExports(prev, next))
	})
}

// assertSameRows compares the two exports' ROWS. The envelope (workspace id,
// export timestamp) legitimately differs between two stores and is not what the
// delta is responsible for.
func assertSameRows(t *testing.T, step int, name string, want, got model.Export) {
	t.Helper()
	for _, table := range []struct {
		table string
		want  any
		got   any
	}{
		{"issues", want.Issues, got.Issues},
		{"relations", want.Relations, got.Relations},
		{"comments", want.Comments, got.Comments},
		{"labels", want.Labels, got.Labels},
		{"events", want.Events, got.Events},
	} {
		if !reflect.DeepEqual(table.want, table.got) {
			t.Errorf("step %d (%s): %s after the delta differ from the full rewrite\n full rewrite: %+v\n       delta: %+v", step, name, table.table, table.want, table.got)
		}
	}
}
