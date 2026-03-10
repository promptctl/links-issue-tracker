package query

import (
	"testing"
)

func TestParseBuildsFilterFromQueryExpression(t *testing.T) {
	result, err := Parse(`status:in-progress type:task assignee:bmf priority<=2 has:comments updated>=2026-03-07T10:00:00Z "render contract" id:issue-123 label:renderer`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if result.Filter.Status != "in_progress" {
		t.Fatalf("Status = %q", result.Filter.Status)
	}
	if result.Filter.IssueType != "task" {
		t.Fatalf("IssueType = %q", result.Filter.IssueType)
	}
	if result.Filter.Assignee != "bmf" {
		t.Fatalf("Assignee = %q", result.Filter.Assignee)
	}
	if result.Filter.PriorityMax == nil || *result.Filter.PriorityMax != 2 {
		t.Fatalf("PriorityMax = %#v", result.Filter.PriorityMax)
	}
	if result.Filter.HasComments == nil || !*result.Filter.HasComments {
		t.Fatalf("HasComments = %#v", result.Filter.HasComments)
	}
	if result.Filter.UpdatedAfter == nil {
		t.Fatal("UpdatedAfter is nil")
	}
	if len(result.Filter.SearchTerms) != 1 || result.Filter.SearchTerms[0] != "render contract" {
		t.Fatalf("SearchTerms = %#v", result.Filter.SearchTerms)
	}
	if len(result.Filter.IDs) != 1 || result.Filter.IDs[0] != "issue-123" {
		t.Fatalf("IDs = %#v", result.Filter.IDs)
	}
	if len(result.Filter.LabelsAll) != 1 || result.Filter.LabelsAll[0] != "renderer" {
		t.Fatalf("LabelsAll = %#v", result.Filter.LabelsAll)
	}
}

func TestMergeRejectsConflictingScalarFilters(t *testing.T) {
	base, err := Parse(`status:open`)
	if err != nil {
		t.Fatalf("Parse(base) error = %v", err)
	}
	incoming, err := Parse(`status:closed`)
	if err != nil {
		t.Fatalf("Parse(incoming) error = %v", err)
	}
	if _, err := Merge(base.Filter, incoming.Filter); err == nil {
		t.Fatal("expected conflict error")
	}
}

func TestParseRejectsInvalidStatus(t *testing.T) {
	if _, err := Parse(`status:todo`); err == nil {
		t.Fatal("expected invalid status error")
	}
}
