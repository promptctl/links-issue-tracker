package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/model"
)

func TestKindStringReturnsKey(t *testing.T) {
	if MissingField.String() != "missing_field" {
		t.Fatalf("MissingField.String() = %q, want missing_field", MissingField.String())
	}
	if BlockedBy.String() != "open_dependency" {
		t.Fatalf("BlockedBy.String() = %q, want open_dependency", BlockedBy.String())
	}
}

func TestKindJSONRoundTrip(t *testing.T) {
	original := MissingField
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal(MissingField) error = %v", err)
	}
	if string(data) != `"missing_field"` {
		t.Fatalf("json.Marshal(MissingField) = %s, want %q", data, "missing_field")
	}
	var recovered Kind
	if err := json.Unmarshal(data, &recovered); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if recovered != original {
		t.Fatalf("round-trip kind = %#v, want %#v", recovered, original)
	}
}

func TestKindMarshalJSONRejectsInvalidKind(t *testing.T) {
	var invalid Kind
	if _, err := json.Marshal(invalid); err == nil {
		t.Fatal("json.Marshal(invalid Kind) expected error")
	}
}

func TestKindUnmarshalJSONRejectsUnknownKind(t *testing.T) {
	var recovered Kind
	err := json.Unmarshal([]byte(`"unknown_kind"`), &recovered)
	if err == nil {
		t.Fatal("json.Unmarshal(unknown kind) expected error")
	}
	if err.Error() != `unknown annotation kind "unknown_kind"` {
		t.Fatalf("json.Unmarshal(unknown kind) error = %q", err.Error())
	}
}

func TestAnnotateRunsAllAnnotators(t *testing.T) {
	ctx := context.Background()
	issues := []model.Issue{
		{ID: "a", Description: ""},
		{ID: "b", Description: "has desc"},
	}
	descChecker := func(_ context.Context, issue model.Issue) ([]Annotation, error) {
		if issue.Description == "" {
			return []Annotation{{Kind: MissingField, Message: "description"}}, nil
		}
		return nil, nil
	}
	alwaysAnnotates := func(_ context.Context, _ model.Issue) ([]Annotation, error) {
		return []Annotation{{Kind: BlockedBy, Message: "lit-xyz"}}, nil
	}

	result, err := Annotate(ctx, issues, descChecker, alwaysAnnotates)
	if err != nil {
		t.Fatalf("Annotate() error = %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}
	// Issue "a": missing description + blocked = 2 annotations
	if len(result[0].Annotations) != 2 {
		t.Fatalf("result[0].Annotations = %d, want 2", len(result[0].Annotations))
	}
	// Issue "b": only blocked = 1 annotation
	if len(result[1].Annotations) != 1 {
		t.Fatalf("result[1].Annotations = %d, want 1", len(result[1].Annotations))
	}
	if result[1].Annotations[0].Kind.String() != "open_dependency" {
		t.Fatalf("result[1].Annotations[0].Kind = %q, want open_dependency", result[1].Annotations[0].Kind.String())
	}
}

func TestAnnotateEmptyAnnotatorsProducesEmptySlice(t *testing.T) {
	ctx := context.Background()
	issues := []model.Issue{{ID: "a"}}

	result, err := Annotate(ctx, issues)
	if err != nil {
		t.Fatalf("Annotate() error = %v", err)
	}
	if result[0].Annotations == nil {
		t.Fatal("Annotations should be empty slice, not nil")
	}
	if len(result[0].Annotations) != 0 {
		t.Fatalf("len(Annotations) = %d, want 0", len(result[0].Annotations))
	}
}

func TestAnnotateAnnotatorError(t *testing.T) {
	ctx := context.Background()
	issues := []model.Issue{{ID: "a"}}
	failing := func(_ context.Context, _ model.Issue) ([]Annotation, error) {
		return nil, errors.New("lookup failed")
	}

	_, err := Annotate(ctx, issues, failing)
	if err == nil {
		t.Fatal("Annotate() expected error")
	}
	if err.Error() != "lookup failed" {
		t.Fatalf("error = %q, want %q", err.Error(), "lookup failed")
	}
}

func TestAnnotatedIssueJSONShape(t *testing.T) {
	ai := AnnotatedIssue{
		Issue: model.Issue{
			ID:    "lit-abc",
			Title: "Test issue",
		},
		Annotations: []Annotation{
			{Kind: MissingField, Message: "description"},
		},
	}
	data, err := json.Marshal(ai)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	// Issue fields should be at top level (embedded)
	if _, ok := raw["id"]; !ok {
		t.Fatal("JSON missing top-level 'id' field")
	}
	if _, ok := raw["title"]; !ok {
		t.Fatal("JSON missing top-level 'title' field")
	}
	// Annotations should be present
	if _, ok := raw["annotations"]; !ok {
		t.Fatal("JSON missing 'annotations' field")
	}
	// Should NOT have a nested "issue" key
	if _, ok := raw["issue"]; ok {
		t.Fatal("JSON should not have nested 'issue' key — Issue should be embedded")
	}
}

func TestHasAnyMatchesKind(t *testing.T) {
	annotations := []Annotation{
		{Kind: MissingField, Message: "description"},
		{Kind: BlockedBy, Message: "lit-xyz"},
	}
	if !HasAny(annotations, BlockedBy) {
		t.Fatal("HasAny should match BlockedBy")
	}
	if !HasAny(annotations, MissingField, BlockedBy) {
		t.Fatal("HasAny should match with multiple kinds")
	}
}

func TestHasAnyNoMatch(t *testing.T) {
	annotations := []Annotation{
		{Kind: MissingField, Message: "description"},
	}
	if HasAny(annotations, BlockedBy) {
		t.Fatal("HasAny should not match BlockedBy when only MissingField present")
	}
}

func TestHasAnyEmptyAnnotations(t *testing.T) {
	if HasAny([]Annotation{}, MissingField) {
		t.Fatal("HasAny on empty annotations should return false")
	}
	if HasAny(nil, MissingField) {
		t.Fatal("HasAny on nil annotations should return false")
	}
}
