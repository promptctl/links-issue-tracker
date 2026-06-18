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
func fingerprintOf(t *testing.T, result MergeResult, field ProseField) string {
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
		{IssueID: "i1", Field: ProseTitle, Fingerprint: fingerprintOf(t, result, ProseTitle), Text: "merged-title"},
		{IssueID: "i1", Field: ProseDescription, Fingerprint: fingerprintOf(t, result, ProseDescription), Text: "merged-desc"},
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
	if result.Provisional().Issues[0].Title == "merged-title" {
		t.Fatalf("splice mutated the provisional export in place")
	}
}

func TestApplyProseResolutionsRejectsPartialSet(t *testing.T) {
	result := prosePendingFixture(t)
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{IssueID: "i1", Field: ProseTitle, Fingerprint: fingerprintOf(t, result, ProseTitle), Text: "merged-title"},
	}); ok {
		t.Fatalf("partial resolution accepted; a pending field would keep its provisional value")
	}
}

func TestApplyProseResolutionsRejectsStaleFingerprint(t *testing.T) {
	result := prosePendingFixture(t)
	// Right key, but the fingerprint is from a different conflict — the agent merged
	// against a since-changed base/ours/theirs. The text must not be committed.
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{IssueID: "i1", Field: ProseTitle, Fingerprint: "deadbeefcafe", Text: "merged-title"},
		{IssueID: "i1", Field: ProseDescription, Fingerprint: fingerprintOf(t, result, ProseDescription), Text: "merged-desc"},
	}); ok {
		t.Fatalf("stale fingerprint accepted; a merge of an old conflict would be committed")
	}
}

func TestApplyProseResolutionsRejectsUnknownField(t *testing.T) {
	result := prosePendingFixture(t)
	// Resolving a field that is not pending (agent_prompt here) means the agent
	// merged against a divergence that does not match the live one.
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{IssueID: "i1", Field: ProseTitle, Fingerprint: fingerprintOf(t, result, ProseTitle), Text: "merged-title"},
		{IssueID: "i1", Field: ProsePrompt, Fingerprint: "0", Text: "stray"},
	}); ok {
		t.Fatalf("resolution for a non-pending field accepted")
	}
}

func TestApplyProseResolutionsRejectsDuplicateField(t *testing.T) {
	result := prosePendingFixture(t)
	// Two resolutions for the SAME pending field: keeping the last would silently
	// finalize one of two conflicting texts. The count gate cannot catch this (the
	// duplicate keeps the map the same size), so the duplicate itself must reject.
	titleFP := fingerprintOf(t, result, ProseTitle)
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{IssueID: "i1", Field: ProseTitle, Fingerprint: titleFP, Text: "first"},
		{IssueID: "i1", Field: ProseTitle, Fingerprint: titleFP, Text: "second"},
		{IssueID: "i1", Field: ProseDescription, Fingerprint: fingerprintOf(t, result, ProseDescription), Text: "merged-desc"},
	}); ok {
		t.Fatalf("duplicate resolution for one field accepted; the last would silently win")
	}
}

func TestApplyProseResolutionsRejectsWrongIssue(t *testing.T) {
	result := prosePendingFixture(t)
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{IssueID: "i1", Field: ProseTitle, Fingerprint: fingerprintOf(t, result, ProseTitle), Text: "merged-title"},
		{IssueID: "nope", Field: ProseDescription, Fingerprint: "0", Text: "merged-desc"},
	}); ok {
		t.Fatalf("resolution for a non-pending issue accepted")
	}
}
