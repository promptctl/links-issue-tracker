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

func TestApplyProseResolutionsSplicesExactBijection(t *testing.T) {
	result := prosePendingFixture(t)
	export, ok := ApplyProseResolutions(result, []ProseResolution{
		{IssueID: "i1", Field: ProseTitle, Text: "merged-title"},
		{IssueID: "i1", Field: ProseDescription, Text: "merged-desc"},
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
		{IssueID: "i1", Field: ProseTitle, Text: "merged-title"},
	}); ok {
		t.Fatalf("partial resolution accepted; a pending field would keep its provisional value")
	}
}

func TestApplyProseResolutionsRejectsUnknownField(t *testing.T) {
	result := prosePendingFixture(t)
	// Resolving a field that is not pending (agent_prompt here) means the agent
	// merged against a divergence that does not match the live one.
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{IssueID: "i1", Field: ProseTitle, Text: "merged-title"},
		{IssueID: "i1", Field: ProsePrompt, Text: "stray"},
	}); ok {
		t.Fatalf("resolution for a non-pending field accepted")
	}
}

func TestApplyProseResolutionsRejectsWrongIssue(t *testing.T) {
	result := prosePendingFixture(t)
	if _, ok := ApplyProseResolutions(result, []ProseResolution{
		{IssueID: "i1", Field: ProseTitle, Text: "merged-title"},
		{IssueID: "nope", Field: ProseDescription, Text: "merged-desc"},
	}); ok {
		t.Fatalf("resolution for a non-pending issue accepted")
	}
}
