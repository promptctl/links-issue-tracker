package storage_test

import (
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestApplyProseFieldsRecordsEachFieldItMoves pins the one mapping from a
// merge.ProseField to the editable field it writes, in history's vocabulary:
// agent_prompt is the field history calls prompt. A field whose merged text is
// its provisional text moves nothing and records nothing.
func TestApplyProseFieldsRecordsEachFieldItMoves(t *testing.T) {
	provisional := model.Issue{ID: "i1", Title: "ours title", Description: "ours desc", Prompt: "ours prompt"}
	issue, changes, err := storage.ApplyProseFields(provisional, merge.ProseTexts{
		merge.ProseTitle:       "merged title",
		merge.ProseDescription: "ours desc",
		merge.ProsePrompt:      "merged prompt",
	})
	if err != nil {
		t.Fatalf("ApplyProseFields: %v", err)
	}
	if issue.Title != "merged title" || issue.Description != "ours desc" || issue.Prompt != "merged prompt" {
		t.Fatalf("issue = %q / %q / %q, want the merged texts", issue.Title, issue.Description, issue.Prompt)
	}
	want := []model.FieldChange{
		{Field: "title", From: "ours title", To: "merged title"},
		{Field: "prompt", From: "ours prompt", To: "merged prompt"},
	}
	if !slices.Equal(changes, want) {
		t.Fatalf("changes = %+v, want %+v", changes, want)
	}
}

// TestApplyProseFieldsRefusesAFieldItCannotWrite: a merged text for a field
// with no editable counterpart is an error, never a text silently dropped while
// the provisional value is published as merged.
func TestApplyProseFieldsRefusesAFieldItCannotWrite(t *testing.T) {
	_, _, err := storage.ApplyProseFields(model.Issue{ID: "i1", Title: "t"}, merge.ProseTexts{"lane": "x"})
	if err == nil {
		t.Fatalf("ApplyProseFields accepted a merged text for a field it cannot write")
	}
}
