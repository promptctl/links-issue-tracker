package storage_test

import (
	"errors"
	"reflect"
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

// TestApplyProseFieldsHoldsMergedTextToTheFieldRules: merged text lands under
// the rules every edit of that field obeys — trimmed, and never an empty title —
// because it is decided by the same field table, not written around it.
func TestApplyProseFieldsHoldsMergedTextToTheFieldRules(t *testing.T) {
	provisional := model.Issue{ID: "i1", Title: "ours title", Description: "ours desc"}
	issue, _, err := storage.ApplyProseFields(provisional, merge.ProseTexts{merge.ProseDescription: "  merged desc\n"})
	if err != nil {
		t.Fatalf("ApplyProseFields: %v", err)
	}
	if issue.Description != "merged desc" {
		t.Fatalf("description = %q, want the merged text trimmed", issue.Description)
	}
	var invalid model.ValidationError
	if _, _, err := storage.ApplyProseFields(provisional, merge.ProseTexts{merge.ProseTitle: "   "}); !errors.As(err, &invalid) {
		t.Fatalf("ApplyProseFields(blank title) error = %v, want a ValidationError", err)
	}
}

// TestAPatchStatingAnyOneFieldIsNotEmpty: every pointer on the patch is an
// editable field, and a patch stating any one of them states something. The
// fields are read off the type, so a field added to the patch is held to this
// with no edit here. A reason alone states no field.
func TestAPatchStatingAnyOneFieldIsNotEmpty(t *testing.T) {
	patchType := reflect.TypeFor[storage.UpdateIssueInput]()
	walked := 0
	for i := range patchType.NumField() {
		field := patchType.Field(i)
		if field.Type.Kind() != reflect.Pointer {
			continue
		}
		walked++
		var in storage.UpdateIssueInput
		reflect.ValueOf(&in).Elem().Field(i).Set(reflect.New(field.Type.Elem()))
		if in.IsEmpty() {
			t.Errorf("a patch stating only %s reads as stating nothing", field.Name)
		}
	}
	if walked == 0 {
		t.Fatal("UpdateIssueInput has no pointer fields, so the walk checked nothing")
	}
	if !(storage.UpdateIssueInput{Reason: "why"}).IsEmpty() {
		t.Error("a patch stating only a reason reads as stating a field")
	}
}

// TestABulkUpdateKeyStatesTheFieldItNames: an update document that states one
// key is either refused for that key or admitted as a patch stating exactly
// the field of that key's name — never refused as stating nothing, which is
// the refusal a key the patch translation forgot would get, and never a patch
// stating some other field, which is what a miswired row would make. The keys
// are read off the type, so a key added to the document is held to this with
// no edit here.
func TestABulkUpdateKeyStatesTheFieldItNames(t *testing.T) {
	samples := map[reflect.Type]any{
		reflect.TypeFor[string]():   "task",
		reflect.TypeFor[int]():      int(model.PriorityUrgent),
		reflect.TypeFor[[]string](): []string{"label"},
	}
	// The gate's own refusal of a document stating nothing, so that verdict is
	// recognised by what the gate says rather than by its wording.
	empty := storage.BulkIssueSpec{ID: "links-x-1"}
	_, emptyRefusal := storage.BulkWriteOrder([]storage.BulkIssueSpec{empty}, nil)
	if emptyRefusal == nil {
		t.Fatal("the gate admitted an update document stating nothing")
	}
	docType := reflect.TypeFor[storage.BulkIssueSpec]()
	admitted := 0
	for i := range docType.NumField() {
		field := docType.Field(i)
		if field.Type.Kind() != reflect.Pointer {
			continue
		}
		sample, ok := samples[field.Type.Elem()]
		if !ok {
			t.Fatalf("no sample value for %s's type %s; add one", field.Name, field.Type.Elem())
		}
		doc := empty
		value := reflect.New(field.Type.Elem())
		value.Elem().Set(reflect.ValueOf(sample))
		reflect.ValueOf(&doc).Elem().Field(i).Set(value)
		_, err := storage.BulkWriteOrder([]storage.BulkIssueSpec{doc}, nil)
		switch {
		case err == nil:
			admitted++
			patch, err := storage.BulkUpdatePatch(doc)
			if err != nil {
				t.Fatalf("BulkUpdatePatch(%s) after the gate admitted it: %v", field.Name, err)
			}
			if got := statedFields(patch); !slices.Equal(got, []string{field.Name}) {
				t.Errorf("a document stating only %s became a patch stating %v", field.Name, got)
			}
		case err.Error() == emptyRefusal.Error():
			t.Errorf("a document stating only %s is refused as stating nothing: %v", field.Name, err)
		default:
			// Refused for the key itself: a key an update does not own.
		}
	}
	if admitted == 0 {
		t.Fatal("the gate refused every one-key document, so no translation was checked")
	}
}

// statedFields names the patch's non-nil pointers.
func statedFields(patch storage.UpdateIssueInput) []string {
	v := reflect.ValueOf(patch)
	var stated []string
	for i := range v.NumField() {
		if v.Field(i).Kind() == reflect.Pointer && !v.Field(i).IsNil() {
			stated = append(stated, v.Type().Field(i).Name)
		}
	}
	return stated
}
