package merge

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

var (
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
)

// leaf builds a hydrated leaf issue; mut tailors the scalar fields before the
// lifecycle is attached so status/assignee/closed_at travel through the real
// model API the resolver depends on.
func leaf(t *testing.T, id string, view model.StatusView, mut func(*model.Issue)) model.Issue {
	t.Helper()
	iss := model.Issue{ID: id, IssueType: "task", CreatedAt: t0, UpdatedAt: t0}
	if mut != nil {
		mut(&iss)
	}
	hydrated, err := model.HydrateStatus(iss, view)
	if err != nil {
		t.Fatalf("HydrateStatus(%s): %v", id, err)
	}
	return hydrated
}

func open(t *testing.T, id string) model.Issue {
	return leaf(t, id, model.StatusView{Value: model.StateOpen}, nil)
}

func TestResolveIssueStatusTwoTier(t *testing.T) {
	cases := []struct {
		name               string
		base, ours, theirs model.State
		want               model.State
	}{
		{"unchanged keeps base", model.StateInProgress, model.StateInProgress, model.StateInProgress, model.StateInProgress},
		{"reopen via tier1 (only ours moved off closed)", model.StateClosed, model.StateOpen, model.StateClosed, model.StateOpen},
		{"only theirs moved -> take theirs", model.StateOpen, model.StateOpen, model.StateInProgress, model.StateInProgress},
		{"concurrent closed vs in_progress -> closed dominates", model.StateOpen, model.StateInProgress, model.StateClosed, model.StateClosed},
		{"concurrent open vs in_progress (off closed) -> in_progress dominates", model.StateClosed, model.StateOpen, model.StateInProgress, model.StateInProgress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := leaf(t, "i1", model.StatusView{Value: tc.base}, nil)
			ours := leaf(t, "i1", model.StatusView{Value: tc.ours}, nil)
			theirs := leaf(t, "i1", model.StatusView{Value: tc.theirs}, nil)
			got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB")
			if got.Provisional().StatusValue() != string(tc.want) {
				t.Fatalf("status = %q, want %q", got.Provisional().StatusValue(), tc.want)
			}
			if len(got.Pending) != 0 {
				t.Fatalf("status is code-resolvable; unexpected pending = %#v", got.Pending)
			}
		})
	}
}

func TestResolveIssuePriorityUrgentWins(t *testing.T) {
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Priority = model.PriorityNormal })
	ours := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Priority = model.PriorityUrgent })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Priority = model.PriorityNormal })
	got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB")
	if got.Provisional().Priority != model.PriorityUrgent {
		t.Fatalf("priority = %d, want urgent", got.Provisional().Priority)
	}
}

func TestResolveIssueProseTier1TakesMover(t *testing.T) {
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "a"; i.Description = "d" })
	ours := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "b"; i.Description = "d" })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "a"; i.Description = "d" })
	got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB")
	if len(got.Pending) != 0 {
		t.Fatalf("only one side rewrote title; should not need the agent: %#v", got.Pending)
	}
	if got.Provisional().Title != "b" {
		t.Fatalf("title = %q, want b", got.Provisional().Title)
	}
}

func TestIssueResolutionSettledGatesOnPending(t *testing.T) {
	// No prose conflict -> Settled hands back a committable row.
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, nil)
	ours := leaf(t, "i1", model.StatusView{Value: model.StateClosed}, nil)
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateClosed}, nil)
	if row, ok := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB").Settled(); !ok || row.StatusValue() != string(model.StateClosed) {
		t.Fatalf("Settled() = (%v, %v), want a committable closed row", row.StatusValue(), ok)
	}

	// Concurrent prose rewrite -> Settled refuses; only Provisional yields the row.
	pbase := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "a" })
	pours := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "ours" })
	ptheirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "theirs" })
	res := ResolveIssue(&pbase, &pours, &ptheirs, "wsA", "wsB")
	if _, ok := res.Settled(); ok {
		t.Fatalf("Settled() ok=true with prose pending; the autonomous-commit path must refuse unresolved prose")
	}
}

func TestResolveIssueProseTier2EmitsPendingNeverAutoPicks(t *testing.T) {
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "a"; i.Description = "base d"; i.Prompt = "p" })
	ours := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "a"; i.Description = "ours d"; i.Prompt = "p" })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "a"; i.Description = "theirs d"; i.Prompt = "p" })
	got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB")
	if len(got.Pending) != 1 {
		t.Fatalf("pending = %#v, want exactly the description", got.Pending)
	}
	p := got.Pending[0]
	if p.Field != ProseDescription || p.IssueID != "i1" {
		t.Fatalf("pending ref = %#v", p)
	}
	if p.Base != "base d" || p.Ours != "ours d" || p.Theirs != "theirs d" {
		t.Fatalf("pending carries all three versions for the agent: %#v", p)
	}
}

func TestResolveIssueTiebreakSymmetry(t *testing.T) {
	// Topic diverged on both sides: a single-valued field with no semantic winner.
	mk := func(topic string) func(*model.Issue) {
		return func(i *model.Issue) { i.Topic = topic }
	}
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, mk("root"))
	alice := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, mk("alice"))
	bob := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, mk("bob"))

	forward := ResolveIssue(&base, &alice, &bob, "wsA", "wsB")
	swapped := ResolveIssue(&base, &bob, &alice, "wsB", "wsA")
	if forward.Provisional().Topic != swapped.Provisional().Topic {
		t.Fatalf("tiebreak not symmetric: forward=%q swapped=%q", forward.Provisional().Topic, swapped.Provisional().Topic)
	}
	if forward.Provisional().Topic != "bob" {
		t.Fatalf("tiebreak winner = %q, want the value from the greater workspace id (wsB->bob)", forward.Provisional().Topic)
	}
}

func TestResolveIssueAssigneeTiebreakSymmetry(t *testing.T) {
	// Both sides reassign to different agents off a shared base: a single-valued
	// field with no semantic winner, settled by the symmetric workspace-id
	// tiebreak. Swapping ours/theirs (and their workspace ids) must yield the
	// same winner, or the two machines would converge to different assignees.
	// Assignee is an issue-level field now, set via the mutator rather than the
	// status view — the lifecycle no longer carries it.
	mk := func(assignee string) func(*model.Issue) {
		return func(i *model.Issue) { i.Assignee = assignee }
	}
	inProgress := model.StatusView{Value: model.StateInProgress}
	base := leaf(t, "i1", inProgress, mk("root"))
	alice := leaf(t, "i1", inProgress, mk("alice"))
	bob := leaf(t, "i1", inProgress, mk("bob"))

	forward := ResolveIssue(&base, &alice, &bob, "wsA", "wsB")
	swapped := ResolveIssue(&base, &bob, &alice, "wsB", "wsA")
	if forward.Provisional().AssigneeValue() != swapped.Provisional().AssigneeValue() {
		t.Fatalf("assignee tiebreak not symmetric: forward=%q swapped=%q",
			forward.Provisional().AssigneeValue(), swapped.Provisional().AssigneeValue())
	}
	if forward.Provisional().AssigneeValue() != "bob" {
		t.Fatalf("assignee tiebreak winner = %q, want the value from the greater workspace id (wsB->bob)",
			forward.Provisional().AssigneeValue())
	}
	if len(forward.Pending) != 0 {
		t.Fatalf("assignee is code-resolvable; unexpected pending = %#v", forward.Pending)
	}
}

func TestThreeWayUnionsConcurrentNonParentRelations(t *testing.T) {
	// blocks / related-to are additively unioned: a blocks edge added on one side
	// and a related-to edge added on the other both survive. They are different
	// keys with no single-valued constraint, so unlike parent-child neither is
	// pruned — concurrent links from two machines accrue rather than overwrite.
	issues := []model.Issue{open(t, "a"), open(t, "b"), open(t, "c")}
	base := model.Export{WorkspaceID: "wsA", Issues: issues}
	local := model.Export{
		WorkspaceID: "wsA",
		Issues:      issues,
		Relations:   []model.Relation{{SrcID: "a", DstID: "b", Type: model.RelBlocks, CreatedAt: t1}},
	}
	remote := model.Export{
		WorkspaceID: "wsB",
		Issues:      issues,
		Relations:   []model.Relation{{SrcID: "a", DstID: "c", Type: model.RelRelatedTo, CreatedAt: t2}},
	}
	got := ThreeWay(base, local, remote)
	var hasBlocks, hasRelated bool
	for _, relation := range got.Provisional().Relations {
		if relation.SrcID == "a" && relation.DstID == "b" && relation.Type == model.RelBlocks {
			hasBlocks = true
		}
		if relation.SrcID == "a" && relation.DstID == "c" && relation.Type == model.RelRelatedTo {
			hasRelated = true
		}
	}
	if !hasBlocks || !hasRelated || len(got.Provisional().Relations) != 2 {
		t.Fatalf("relations = %#v, want both concurrent non-parent edges unioned", got.Provisional().Relations)
	}
}

func TestResolveIssueClosedAtSlavedToStatus(t *testing.T) {
	// Both sides closed concurrently -> closed; closed_at is the earliest close.
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, nil)
	ours := leaf(t, "i1", model.StatusView{Value: model.StateClosed, ClosedAt: &t2}, nil)
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateClosed, ClosedAt: &t1}, nil)
	got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB")
	if got.Provisional().StatusValue() != string(model.StateClosed) {
		t.Fatalf("status = %q, want closed", got.Provisional().StatusValue())
	}
	closedAt := got.Provisional().ClosedAtValue()
	if closedAt == nil || !closedAt.Equal(t1) {
		t.Fatalf("closed_at = %v, want earliest close %v", closedAt, t1)
	}

	// Reopen wins -> closed_at must be cleared even though theirs still carries one.
	reopenBase := leaf(t, "i1", model.StatusView{Value: model.StateClosed, ClosedAt: &t1}, nil)
	reopenOurs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, nil)
	reopenTheirs := leaf(t, "i1", model.StatusView{Value: model.StateClosed, ClosedAt: &t1}, nil)
	reopened := ResolveIssue(&reopenBase, &reopenOurs, &reopenTheirs, "wsA", "wsB")
	if reopened.Provisional().StatusValue() != string(model.StateOpen) {
		t.Fatalf("reopen status = %q, want open", reopened.Provisional().StatusValue())
	}
	if reopened.Provisional().ClosedAtValue() != nil {
		t.Fatalf("closed_at = %v, want nil (slaved to non-closed status)", reopened.Provisional().ClosedAtValue())
	}
}

func TestResolveIssueArchivedAtEarliestWhenBothArchive(t *testing.T) {
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, nil)
	ours := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.ArchivedAt = &t2 })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.ArchivedAt = &t1 })
	got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB")
	if got.Provisional().ArchivedAt == nil || !got.Provisional().ArchivedAt.Equal(t1) {
		t.Fatalf("archived_at = %v, want earliest %v", got.Provisional().ArchivedAt, t1)
	}

	// Only ours archived -> tier1 takes the archive; timestamp is ours.
	soloBase := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, nil)
	soloOurs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.ArchivedAt = &t2 })
	soloTheirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, nil)
	solo := ResolveIssue(&soloBase, &soloOurs, &soloTheirs, "wsA", "wsB")
	if solo.Provisional().ArchivedAt == nil || !solo.Provisional().ArchivedAt.Equal(t2) {
		t.Fatalf("solo archive = %v, want %v", solo.Provisional().ArchivedAt, t2)
	}
}

func TestResolveIssueLabelsUnion(t *testing.T) {
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Labels = []string{"keep"} })
	ours := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Labels = []string{"keep", "ours"} })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Labels = []string{"keep", "theirs"} })
	got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB")
	want := []string{"keep", "ours", "theirs"}
	if len(got.Provisional().Labels) != len(want) {
		t.Fatalf("labels = %#v, want union %#v", got.Provisional().Labels, want)
	}
	for idx, label := range want {
		if got.Provisional().Labels[idx] != label {
			t.Fatalf("labels = %#v, want sorted union %#v", got.Provisional().Labels, want)
		}
	}
}

func TestResolveIssueLabelRemovalNotResurrected(t *testing.T) {
	// base=[a,b]; ours removed b; theirs left both -> b stays removed (Tier 1),
	// a survives. A naive union would resurrect b.
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Labels = []string{"a", "b"} })
	ours := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Labels = []string{"a"} })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Labels = []string{"a", "b"} })
	got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB").Provisional()
	if len(got.Labels) != 1 || got.Labels[0] != "a" {
		t.Fatalf("labels = %#v, want [a] (b removed by ours must not be resurrected)", got.Labels)
	}
}

func TestThreeWayLabelTableHonorsConcurrentRemoval(t *testing.T) {
	issues := []model.Issue{open(t, "i1")}
	base := model.Export{
		WorkspaceID: "wsA",
		Issues:      issues,
		Labels:      []model.Label{{IssueID: "i1", Name: "a"}, {IssueID: "i1", Name: "b"}},
	}
	local := model.Export{ // local removed b
		WorkspaceID: "wsA",
		Issues:      issues,
		Labels:      []model.Label{{IssueID: "i1", Name: "a"}},
	}
	remote := model.Export{ // remote left both untouched
		WorkspaceID: "wsB",
		Issues:      issues,
		Labels:      []model.Label{{IssueID: "i1", Name: "a"}, {IssueID: "i1", Name: "b"}},
	}
	got := ThreeWay(base, local, remote)
	names := map[string]bool{}
	for _, label := range got.Provisional().Labels {
		names[label.Name] = true
	}
	if names["b"] || !names["a"] {
		t.Fatalf("labels = %#v, want only [a] (authoritative table must honor removal)", got.Provisional().Labels)
	}
}

func TestResolveIssueImmutableIDAndCreatedAt(t *testing.T) {
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, nil) // created_at = t0
	ours := leaf(t, "i1", model.StatusView{Value: model.StateInProgress}, func(i *model.Issue) { i.CreatedAt = t2 })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.CreatedAt = t1 })
	got := ResolveIssue(&base, &ours, &theirs, "wsA", "wsB")
	if got.Provisional().ID != "i1" {
		t.Fatalf("id = %q, want i1", got.Provisional().ID)
	}
	if !got.Provisional().CreatedAt.Equal(t0) {
		t.Fatalf("created_at = %v, want immutable base %v", got.Provisional().CreatedAt, t0)
	}
}

func TestResolveIssueNoMergeBaseTreatsEveryFieldAsChanged(t *testing.T) {
	// Same id created independently on both sides: no merge-base. The resolver
	// must not touch the zero-value base's lifecycle accessors, and must converge
	// every field as "both changed".
	ours := leaf(t, "i1", model.StatusView{Value: model.StateInProgress}, func(i *model.Issue) { i.Title = "ours" })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateClosed}, func(i *model.Issue) { i.Title = "theirs" })
	got := ResolveIssue(nil, &ours, &theirs, "wsA", "wsB")
	if got.Provisional().StatusValue() != string(model.StateClosed) {
		t.Fatalf("status = %q, want closed (dominant join with no base)", got.Provisional().StatusValue())
	}
	if len(got.Pending) != 1 || got.Pending[0].Field != ProseTitle {
		t.Fatalf("pending = %#v, want concurrent title", got.Pending)
	}
}

func TestMergeResultSettledGatesOnPending(t *testing.T) {
	mk := func(title string) model.Export {
		return model.Export{WorkspaceID: "ws", Issues: []model.Issue{
			leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = title }),
		}}
	}
	// Concurrent title rewrite -> export carries provisional prose -> Settled refuses.
	base, local, remote := mk("a"), mk("ours"), mk("theirs")
	local.WorkspaceID, remote.WorkspaceID = "wsA", "wsB"
	res := ThreeWay(base, local, remote)
	if len(res.Pending) == 0 {
		t.Fatalf("expected pending prose")
	}
	if _, ok := res.Settled(); ok {
		t.Fatalf("Settled() ok=true with prose pending; a provisional export must not reach the autonomous-commit path")
	}
	// Clean merge -> Settled hands back the export.
	clean := ThreeWay(mk("a"), mk("a"), mk("a"))
	if _, ok := clean.Settled(); !ok {
		t.Fatalf("Settled() ok=false for a clean merge; want a committable export")
	}
}

func TestThreeWayDetectsPromptAndLaneOnlyEdits(t *testing.T) {
	// A change to a field ResolveIssue merges must be seen by ThreeWay's change
	// detection, or the edit is silently dropped.
	cases := []struct {
		name  string
		mut   func(*model.Issue)
		check func(model.Issue) bool
	}{
		{"prompt-only", func(i *model.Issue) { i.Prompt = "new prompt" }, func(i model.Issue) bool { return i.Prompt == "new prompt" }},
		{"lane-only", func(i *model.Issue) { i.Lane = "fast" }, func(i model.Issue) bool { return i.Lane == "fast" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{open(t, "i1")}}
			edited := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, tc.mut)
			local := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{edited}}
			remote := model.Export{WorkspaceID: "wsB", Issues: []model.Issue{open(t, "i1")}}
			got := ThreeWay(base, local, remote)
			if len(got.Provisional().Issues) != 1 || !tc.check(got.Provisional().Issues[0]) {
				t.Fatalf("%s edit dropped: merged = %#v", tc.name, got.Provisional().Issues)
			}
		})
	}
}

func TestThreeWayDeleteVsEditPreservesSurvivingEdit(t *testing.T) {
	base := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{open(t, "i1")}}
	local := model.Export{WorkspaceID: "wsA"} // local removed the whole row
	edited := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = "edited" })
	remote := model.Export{WorkspaceID: "wsB", Issues: []model.Issue{edited}}
	got := ThreeWay(base, local, remote)
	if len(got.Provisional().Issues) != 1 || got.Provisional().Issues[0].Title != "edited" {
		t.Fatalf("issues = %#v, want the surviving remote edit kept (no silent drop)", got.Provisional().Issues)
	}
}

func TestThreeWayBothRemovedBaseRowAppendsNothing(t *testing.T) {
	// Regression guard: a base-only id absent on both sides must converge to a
	// removal, never an appended zero-value issue.
	base := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{open(t, "i1")}}
	local := model.Export{WorkspaceID: "wsA"}
	remote := model.Export{WorkspaceID: "wsB"}
	got := ThreeWay(base, local, remote)
	if len(got.Provisional().Issues) != 0 {
		t.Fatalf("issues = %#v, want none (both removed; no zero-value row)", got.Provisional().Issues)
	}
}

func TestThreeWayUnionsConcurrentComments(t *testing.T) {
	base := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{open(t, "i1")}}
	local := model.Export{
		WorkspaceID: "wsA",
		Issues:      []model.Issue{open(t, "i1")},
		Comments:    []model.Comment{{ID: "c-ours", IssueID: "i1", Body: "ours", CreatedAt: t1}},
	}
	remote := model.Export{
		WorkspaceID: "wsB",
		Issues:      []model.Issue{open(t, "i1")},
		Comments:    []model.Comment{{ID: "c-theirs", IssueID: "i1", Body: "theirs", CreatedAt: t2}},
	}
	got := ThreeWay(base, local, remote)
	ids := map[string]bool{}
	for _, comment := range got.Provisional().Comments {
		ids[comment.ID] = true
	}
	if !ids["c-ours"] || !ids["c-theirs"] || len(got.Provisional().Comments) != 2 {
		t.Fatalf("comments = %#v, want both concurrent comments kept", got.Provisional().Comments)
	}
}

func TestThreeWayBreaksConcurrentParentCycle(t *testing.T) {
	// ours makes a the child of b; theirs makes b the child of a. Each is a valid
	// single-parent edge alone, but the union is a cycle the store cannot reject.
	issues := []model.Issue{open(t, "a"), open(t, "b")}
	base := model.Export{WorkspaceID: "wsA", Issues: issues}
	local := model.Export{
		WorkspaceID: "wsA",
		Issues:      issues,
		Relations:   []model.Relation{{SrcID: "a", DstID: "b", Type: model.RelParentChild, CreatedAt: t1}},
	}
	remote := model.Export{
		WorkspaceID: "wsB",
		Issues:      issues,
		Relations:   []model.Relation{{SrcID: "b", DstID: "a", Type: model.RelParentChild, CreatedAt: t2}},
	}
	got := ThreeWay(base, local, remote)
	parentOf := map[string]string{}
	for _, relation := range got.Provisional().Relations {
		if relation.Type == model.RelParentChild {
			parentOf[relation.SrcID] = relation.DstID
		}
	}
	if len(parentOf) != 1 {
		t.Fatalf("parent edges = %#v, want exactly one (cycle broken)", parentOf)
	}
	if parentOf["a"] == "b" && parentOf["b"] == "a" {
		t.Fatalf("both edges survived; the merged graph is a cycle: %#v", parentOf)
	}
}

func TestThreeWayKeepsSingleParentOnConcurrentReparent(t *testing.T) {
	issues := []model.Issue{open(t, "c1"), open(t, "p1"), open(t, "p2")}
	base := model.Export{WorkspaceID: "wsA", Issues: issues}
	local := model.Export{
		WorkspaceID: "wsA",
		Issues:      issues,
		Relations:   []model.Relation{{SrcID: "c1", DstID: "p1", Type: model.RelParentChild, CreatedAt: t1}},
	}
	remote := model.Export{
		WorkspaceID: "wsB",
		Issues:      issues,
		Relations:   []model.Relation{{SrcID: "c1", DstID: "p2", Type: model.RelParentChild, CreatedAt: t2}},
	}
	got := ThreeWay(base, local, remote)
	parents := 0
	for _, relation := range got.Provisional().Relations {
		if relation.SrcID == "c1" && relation.Type == model.RelParentChild {
			parents++
		}
	}
	if parents != 1 {
		t.Fatalf("parent-child edges for c1 = %d, want exactly one parent preserved", parents)
	}
}
