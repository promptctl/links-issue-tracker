package merge

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// plantCollision builds the defect's exact field shape: two disconnected stores
// that each hold the same fifteen children of one epic and each mint `.16` for a
// DIFFERENT job. Nothing races — both stores compute max+1 over their own rows
// and both get 16 — so the two rows share a name and nothing else, including
// their birth certificates.
func plantCollision(t *testing.T) (base, local, remote model.Export) {
	t.Helper()
	shared := leaf(t, "epic.1", model.StatusView{Value: model.StateClosed}, func(i *model.Issue) {
		i.Title = "a child both stores already had"
	})
	ourSixteen := leaf(t, "epic.16", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) {
		i.Title = "Wire the release watchdog"
		i.Description = "alarm when a pending release sits past its window"
		i.CreatedAt = time.Date(2026, 8, 27, 9, 14, 2, 118_000_000, time.UTC)
	})
	theirSixteen := leaf(t, "epic.16", model.StatusView{Value: model.StateInProgress}, func(i *model.Issue) {
		i.Title = "Adaptive id length for large backlogs"
		i.Description = "hash ids grow a character past 4k issues"
		i.CreatedAt = time.Date(2026, 8, 27, 16, 40, 55, 907_000_000, time.UTC)
	})
	export := func(ws string, issues ...model.Issue) model.Export {
		return model.Export{WorkspaceID: ws, Issues: issues}
	}
	return export("wsA", shared), export("wsA", shared, ourSixteen), export("wsB", shared, theirSixteen)
}

// TestThreeWayReportsIDCollisionInsteadOfFusing is the ticket's planted case. On
// the pre-fix engine `epic.16` came back as ONE row — a title from one job over a
// description tiebroken from the other, the loser gone, nothing said. The contract
// now: both rows survive, the operator is told, and the export cannot be committed.
func TestThreeWayReportsIDCollisionInsteadOfFusing(t *testing.T) {
	base, local, remote := plantCollision(t)
	got := ThreeWay(base, local, remote)

	if len(got.Collisions) != 1 {
		t.Fatalf("collisions = %d, want 1; a collision reported as a merge is the fusion defect", len(got.Collisions))
	}
	c := got.Collisions[0]
	if c.IssueID != "epic.16" {
		t.Fatalf("collision id = %q, want epic.16", c.IssueID)
	}
	// Both jobs travel whole: a report naming one of them is the silent-loser bug
	// wearing a warning label.
	if c.Ours.Title != "Wire the release watchdog" {
		t.Fatalf("ours title = %q, want the local job intact", c.Ours.Title)
	}
	if c.Theirs.Title != "Adaptive id length for large backlogs" {
		t.Fatalf("theirs title = %q, want the remote job intact", c.Theirs.Title)
	}
	// [LAW:no-silent-failure] the merge is not committable while two tickets wear
	// one id — no autonomous path may pick a survivor.
	if _, ok := got.Settled(); ok {
		t.Fatalf("Settled() ok=true with an id collision; the autonomous-commit path must refuse")
	}

	// The provisional export keeps OUR row unfused rather than a synthesized blend.
	provisional, ok := got.Provisional()
	if ok {
		t.Fatalf("Provisional() ok=true with an id collision; no consumer may reach an export past an unresolved collision")
	}
	var merged model.Issue
	for _, issue := range provisional.Issues {
		if issue.ID == "epic.16" {
			merged = issue
		}
	}
	if merged.Title != "Wire the release watchdog" || merged.Description != "alarm when a pending release sits past its window" {
		t.Fatalf("provisional epic.16 = {%q, %q}; want the local row carried through whole, never a field-merged blend",
			merged.Title, merged.Description)
	}
	// The fusion signature specifically: one job's title beside the other's
	// description. If this ever holds again, the defect is back.
	if merged.Title == "Wire the release watchdog" && merged.Description == "hash ids grow a character past 4k issues" {
		t.Fatalf("epic.16 fused two unrelated tickets into one row")
	}
}

// TestThreeWayReportsIDCollisionWithNoProseDivergence is the defect at its
// quietest, and the reason a prose conflict is not a safety net. Two agents on two
// machines file the same-sounding ticket — both notice one flaky test — so both
// rows carry identical text and the field merge raises nothing to hold. On the
// pre-fix engine this measured Settled() ok=TRUE: one row committed autonomously,
// the other destroyed, no operator signal anywhere. Only the birth certificates
// ever differed.
func TestThreeWayReportsIDCollisionWithNoProseDivergence(t *testing.T) {
	twin := func(born time.Time) model.Issue {
		return leaf(t, "epic.16", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) {
			i.Title = "Fix the flaky sync test"
			i.Description = "TestSyncPull times out under load on CI"
			i.CreatedAt = born
		})
	}
	ours := twin(time.Date(2026, 8, 27, 9, 14, 2, 118_000_000, time.UTC))
	theirs := twin(time.Date(2026, 8, 27, 16, 40, 55, 907_000_000, time.UTC))

	got := ThreeWay(model.Export{},
		model.Export{WorkspaceID: "wsA", Issues: []model.Issue{ours}},
		model.Export{WorkspaceID: "wsB", Issues: []model.Issue{theirs}})

	if len(got.Pending) != 0 {
		t.Fatalf("pending = %#v, want none; this case exists because identical text raises nothing", got.Pending)
	}
	if len(got.Collisions) != 1 {
		t.Fatalf("collisions = %d, want 1; with no prose to hold, the collision report is the ONLY signal", len(got.Collisions))
	}
	if _, ok := got.Settled(); ok {
		t.Fatalf("Settled() ok=true: two independently created tickets would commit as one with nothing said")
	}
}

// TestClassifySharedRowsAcrossUnrelatedHistories is the false-positive guard, and
// the reason base-absence alone cannot be the discriminator: the combine path
// hands the engine an EMPTY base, so every shared row arrives base-less. Rows that
// are genuinely one ticket must still merge there.
func TestClassifySharedRowsAcrossUnrelatedHistories(t *testing.T) {
	born := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	mk := func(status model.State, title string) model.Issue {
		return leaf(t, "epic.7", model.StatusView{Value: status}, func(i *model.Issue) {
			i.Title = title
			i.CreatedAt = born
		})
	}
	ours, theirs := mk(model.StateInProgress, "one ticket"), mk(model.StateClosed, "one ticket")

	if _, collision := Classify(nil, ours, theirs, "wsA", "wsB"); collision != nil {
		t.Fatalf("one ticket replicated to two unrelated histories reported as a collision; the combine path merges every shared row with no base")
	}

	got := ThreeWay(model.Export{}, model.Export{WorkspaceID: "wsA", Issues: []model.Issue{ours}},
		model.Export{WorkspaceID: "wsB", Issues: []model.Issue{theirs}})
	if len(got.Collisions) != 0 {
		t.Fatalf("collisions = %#v, want none for a replicated row", got.Collisions)
	}
	if _, ok := got.Settled(); !ok {
		t.Fatalf("Settled() ok=false for a clean base-less merge of one ticket")
	}
}

// TestClassifyAncestryOutranksBirthCertificate pins the evidence order: a
// merge-base row proves the two sides descend from one creation, so they are one
// ticket even if their created_at has drifted — the birth certificate is the
// fallback for when there is no ancestry, never an override of it.
func TestClassifyAncestryOutranksBirthCertificate(t *testing.T) {
	base := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, nil)
	ours := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.CreatedAt = t1 })
	theirs := leaf(t, "i1", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.CreatedAt = t2 })

	if _, collision := Classify(&base, ours, theirs, "wsA", "wsB"); collision != nil {
		t.Fatalf("a row present in the merge-base reported as a collision; shared ancestry is proof of one entity")
	}
	if _, collision := Classify(nil, ours, theirs, "wsA", "wsB"); collision == nil {
		t.Fatalf("the same two rows with no ancestry must fall back to the birth certificate and collide")
	}
}

// TestThreeWayDoesNotFuseCollidingChildRows is the fusion defect one level below
// the fields. The colliding id survives as OUR row, so unioning the child tables
// onto it would hand our ticket the other job's comments, edges, labels and
// history — a well-formed ticket nobody filed, exactly what refusing the field
// merge exists to prevent.
func TestThreeWayDoesNotFuseCollidingChildRows(t *testing.T) {
	born := func(hour int) time.Time { return time.Date(2026, 8, 27, hour, 0, 0, 0, time.UTC) }
	sixteen := func(hour int, title string) model.Issue {
		return leaf(t, "epic.16", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) {
			i.Title = title
			i.CreatedAt = born(hour)
		})
	}
	endpoints := []model.Issue{open(t, "epic.a"), open(t, "epic.b")}
	side := func(ws string, hour int, title, suffix, endpoint string) model.Export {
		return model.Export{
			WorkspaceID: ws,
			Issues:      append([]model.Issue{sixteen(hour, title)}, endpoints...),
			Relations:   []model.Relation{{SrcID: "epic.16", DstID: endpoint, Type: model.RelBlocks, CreatedAt: born(hour)}},
			Comments:    []model.Comment{{ID: "c-" + suffix, IssueID: "epic.16", Body: suffix, CreatedAt: born(hour)}},
			Labels:      []model.Label{{IssueID: "epic.16", Name: suffix, CreatedAt: born(hour)}},
			Events:      []model.IssueEvent{{ID: "e-" + suffix, IssueID: "epic.16", Reason: suffix, CreatedAt: born(hour)}},
		}
	}
	local := side("wsA", 9, "Wire the release watchdog", "ours", "epic.a")
	remote := side("wsB", 16, "Adaptive id length for large backlogs", "theirs", "epic.b")

	got := ThreeWay(model.Export{WorkspaceID: "wsA", Issues: endpoints}, local, remote)
	if len(got.Collisions) != 1 {
		t.Fatalf("collisions = %d, want 1; this fixture's premise is a collided id", len(got.Collisions))
	}
	export, _ := got.Provisional()

	for _, comment := range export.Comments {
		if comment.ID != "c-ours" {
			t.Errorf("comment %q rode onto the colliding id; our ticket now carries the other job's discussion", comment.ID)
		}
	}
	for _, event := range export.Events {
		if event.ID != "e-ours" {
			t.Errorf("event %q rode onto the colliding id; our ticket now carries the other job's history", event.ID)
		}
	}
	for _, label := range export.Labels {
		if label.Name != "ours" {
			t.Errorf("label %q rode onto the colliding id", label.Name)
		}
	}
	for _, relation := range export.Relations {
		if relation.DstID != "epic.a" {
			t.Errorf("relation epic.16 -> %q rode onto the colliding id; our ticket now blocks their job's dependency", relation.DstID)
		}
	}
	// The local side's own rows are still there — refusing the union must not
	// silently drop the rows that DO describe the surviving ticket.
	if len(export.Comments) != 1 || len(export.Events) != 1 || len(export.Labels) != 1 || len(export.Relations) != 1 {
		t.Fatalf("local child rows lost: comments=%d events=%d labels=%d relations=%d",
			len(export.Comments), len(export.Events), len(export.Labels), len(export.Relations))
	}
}
