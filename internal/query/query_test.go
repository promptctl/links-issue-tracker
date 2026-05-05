package query

import (
	"testing"
)

func TestParseBuildsFilterFromQueryExpression(t *testing.T) {
	result, err := Parse(`status:in_progress type:task assignee:bmf has:comments updated>=2026-03-07T10:00:00Z "render contract" id:issue-123 label:renderer`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(result.Filter.Statuses) != 1 || result.Filter.Statuses[0] != "in_progress" {
		t.Fatalf("Statuses = %q", result.Filter.Statuses)
	}
	if len(result.Filter.IssueTypes) != 1 || result.Filter.IssueTypes[0] != "task" {
		t.Fatalf("IssueTypes = %q", result.Filter.IssueTypes)
	}
	if len(result.Filter.Assignees) != 1 || result.Filter.Assignees[0] != "bmf" {
		t.Fatalf("Assignees = %q", result.Filter.Assignees)
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

func TestMergeMultipleStatusesCombines(t *testing.T) {
	base, err := Parse(`status:open`)
	if err != nil {
		t.Fatalf("Parse(base) error = %v", err)
	}
	incoming, err := Parse(`status:closed`)
	if err != nil {
		t.Fatalf("Parse(incoming) error = %v", err)
	}
	merged, err := Merge(base.Filter, incoming.Filter)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if len(merged.Statuses) != 2 {
		t.Fatalf("Statuses = %q, want [open closed]", merged.Statuses)
	}
}

func TestStatusAliasInProgressNormalizesToBeadsValue(t *testing.T) {
	result, err := Parse(`status:in-progress`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(result.Filter.Statuses) != 1 || result.Filter.Statuses[0] != "in_progress" {
		t.Fatalf("Statuses = %q, want [in_progress]", result.Filter.Statuses)
	}
}

func TestParseRejectsInvalidStatus(t *testing.T) {
	if _, err := Parse(`status:todo`); err == nil {
		t.Fatal("expected invalid status error")
	}
}
