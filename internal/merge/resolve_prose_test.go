package merge

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// prosePendingFixture builds a merge whose only unresolved part is a concurrent
// title AND description rewrite of one issue, so ApplyProseResolutions has a
// two-field pending set to validate against.
func prosePendingFixture(t *testing.T) MergeResult {
	t.Helper()
	now := time.Now().UTC()
	base := model.Export{Issues: []model.Issue{issueWithStatus(t, model.Issue{ID: "i1", Title: "base-title", Description: "base-desc", Priority: 0, IssueType: "task", CreatedAt: now, UpdatedAt: now}, model.StateOpen)}}
	local := model.Export{Issues: append([]model.Issue(nil), base.Issues...)}
	remote := model.Export{Issues: append([]model.Issue(nil), base.Issues...)}
	local.Issues[0].Title = "ours-title"
	local.Issues[0].Description = "ours-desc"
	remote.Issues[0].Title = "theirs-title"
	remote.Issues[0].Description = "theirs-desc"

	result := ThreeWay(base, local, remote)
	if len(result.Pending) != 2 {
		t.Fatalf("fixture expected 2 pending fields, got %#v", result.Pending)
	}
	return result
}

// fingerprintOf returns the live fingerprint of a pending field in the result, so
// tests pin a resolution to the conflict the fixture actually produced.
func fingerprintOf(t *testing.T, result MergeResult, field ProseField) Fingerprint {
	t.Helper()
	for _, p := range result.Pending {
		if p.Field == field {
			return p.Fingerprint()
		}
	}
	t.Fatalf("no pending field %q in fixture", field)
	return ""
}

func TestApplyProseResolutionsSplicesExactBijection(t *testing.T) {
	result := prosePendingFixture(t)
	export, ok := ApplyProseResolutions(result, []ProseResolution{
		{Fingerprint: fingerprintOf(t, result, ProseTitle), Text: "merged-title"},
		{Fingerprint: fingerprintOf(t, result, ProseDescription), Text: "merged-desc"},
	})
	if !ok {
		t.Fatalf("exact bijection rejected")
	}
	if got := export.Issues[0].Title; got != "merged-title" {
		t.Fatalf("title = %q, want merged-title", got)
	}
	if got := export.Issues[0].Description; got != "merged-desc" {
		t.Fatalf("description = %q, want merged-desc", got)
	}
	// [LAW:no-silent-failure] the splice must not mutate the original provisional
	// export the caller still holds.
	if provisional(t, result).Issues[0].Title == "merged-title" {
		t.Fatalf("splice mutated the provisional export in place")
	}
}

func TestApplyProseResolutionsRejectsPartialSet(t *testing.T) {
	result := prosePendingFixture(t)
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{Fingerprint: fingerprintOf(t, result, ProseTitle), Text: "merged-title"},
	}); ok {
		t.Fatalf("partial resolution accepted; a pending field would keep its provisional value")
	}
}

func TestApplyProseResolutionsRejectsStaleFingerprint(t *testing.T) {
	result := prosePendingFixture(t)
	// A fingerprint that names no live conflict — the agent merged against a
	// since-changed base/ours/theirs, or a field that is not pending at all. The text
	// must not be committed.
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{Fingerprint: "deadbeefcafe", Text: "merged-title"},
		{Fingerprint: fingerprintOf(t, result, ProseDescription), Text: "merged-desc"},
	}); ok {
		t.Fatalf("stale fingerprint accepted; a merge of an old conflict would be committed")
	}
}

func TestApplyProseResolutionsRejectsDuplicateField(t *testing.T) {
	result := prosePendingFixture(t)
	// Two resolutions for the SAME pending field: keeping the last would silently
	// finalize one of two conflicting texts. The count gate cannot catch this (the
	// duplicate keeps the map the same size), so the duplicate itself must reject.
	titleFP := fingerprintOf(t, result, ProseTitle)
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{Fingerprint: titleFP, Text: "first"},
		{Fingerprint: titleFP, Text: "second"},
		{Fingerprint: fingerprintOf(t, result, ProseDescription), Text: "merged-desc"},
	}); ok {
		t.Fatalf("duplicate resolution for one field accepted; the last would silently win")
	}
}

// TestApplyProseResolutionsTellsIdenticalConflictsApart pins what makes a
// fingerprint a whole address. The resolve command carries nothing else, so the
// same title rewrite landing on two tickets must still be two conflicts: a digest
// that left the issue out would give both one fingerprint, and neither could be
// resolved.
func TestApplyProseResolutionsTellsIdenticalConflictsApart(t *testing.T) {
	now := time.Now().UTC()
	issue := func(id string) model.Issue {
		return issueWithStatus(t, model.Issue{ID: id, Title: "base-title", Description: "desc", Priority: 0, IssueType: "task", CreatedAt: now, UpdatedAt: now}, model.StateOpen)
	}
	base := model.Export{Issues: []model.Issue{issue("i1"), issue("i2")}}
	local := model.Export{Issues: append([]model.Issue(nil), base.Issues...)}
	remote := model.Export{Issues: append([]model.Issue(nil), base.Issues...)}
	for i := range base.Issues {
		local.Issues[i].Title = "ours-title"
		remote.Issues[i].Title = "theirs-title"
	}
	result := ThreeWay(base, local, remote)
	if len(result.Pending) != 2 {
		t.Fatalf("fixture expected 2 pending fields, got %#v", result.Pending)
	}

	resolutions := make([]ProseResolution, 0, len(result.Pending))
	for _, p := range result.Pending {
		resolutions = append(resolutions, ProseResolution{Fingerprint: p.Fingerprint(), Text: "merged for " + p.IssueID})
	}
	export, ok := ApplyProseResolutions(result, resolutions)
	if !ok {
		t.Fatalf("the same title conflict on two tickets could not be resolved apart")
	}
	for _, got := range export.Issues {
		if got.Title != "merged for "+got.ID {
			t.Errorf("issue %s title = %q, want its own merged text", got.ID, got.Title)
		}
	}
}

// TestParseFingerprintAdmitsOnlyWhatFingerprintRenders keeps the command boundary
// exact: the one shape Fingerprint prints goes through, and every near miss —
// including the retired ID:FIELD:FINGERPRINT address — is refused there rather
// than reaching the store as a fingerprint that matches nothing.
func TestParseFingerprintAdmitsOnlyWhatFingerprintRenders(t *testing.T) {
	live := ProsePending{IssueID: "i1", Field: ProseTitle, Base: "b", Ours: "o", Theirs: "t"}.Fingerprint()
	if got, ok := ParseFingerprint(string(live)); !ok || got != live {
		t.Fatalf("ParseFingerprint(%q) = %q, %v; want the fingerprint back", live, got, ok)
	}
	for _, text := range []string{"", "deadbeef", "deadbeefcafe00", "DEADBEEFCAFE", "deadbeefcafg", "i1:title:" + string(live)} {
		if _, ok := ParseFingerprint(text); ok {
			t.Errorf("ParseFingerprint(%q) admitted a value Fingerprint never renders", text)
		}
	}
}

// proseAndCollisionFixture builds ONE merge that carries both halves at once: a
// title diverged on both sides of `epic.2` (resolvable by the agent) and `epic.16`
// naming a different ticket on each side (resolvable by nobody). Built through the
// real engine, so the pending set and the collision set are both what ThreeWay
// actually produces.
func proseAndCollisionFixture(t *testing.T) MergeResult {
	t.Helper()
	base, local, remote := plantCollision(t)
	titled := func(title string) model.Issue {
		return leaf(t, "epic.2", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) { i.Title = title })
	}
	base.Issues = append(base.Issues, titled("base-title"))
	local.Issues = append(local.Issues, titled("ours-title"))
	remote.Issues = append(remote.Issues, titled("theirs-title"))

	result := ThreeWay(base, local, remote)
	if len(result.Pending) != 1 || len(result.Collisions) != 1 {
		t.Fatalf("fixture = %d pending, %d collisions; want exactly one of each", len(result.Pending), len(result.Collisions))
	}
	return result
}

// TestApplyProseResolutionsRefusesCollisionDespiteExactBijection pins the gate that
// stands between the agent's merged prose and an export in which one of two
// colliding tickets is silently the survivor. The resolutions here are a PERFECT
// bijection with the live pending set — right fingerprint, no duplicates — so
// every other refusal in this function is satisfied and the only thing that can
// return ok=false is the collision gate itself. Its one production caller cannot
// reach it today, which is exactly why it is pinned here: reordering Provisional's
// tuple or dropping the early return upstream would otherwise remove it with
// nothing failing. [LAW:no-silent-failure]
func TestApplyProseResolutionsRefusesCollisionDespiteExactBijection(t *testing.T) {
	result := proseAndCollisionFixture(t)
	export, ok := ApplyProseResolutions(result, []ProseResolution{
		{Fingerprint: fingerprintOf(t, result, ProseTitle), Text: "merged-title"},
	})
	if ok {
		t.Fatalf("an id collision spliced into a committable export; two tickets under one id have no merged text an agent can supply")
	}
	if len(export.Issues) != 0 {
		t.Fatalf("refused export carries %d issues, want the zero export", len(export.Issues))
	}
}
