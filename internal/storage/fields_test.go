package storage_test

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"gopkg.in/yaml.v3"
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
// key is either refused for that key, or admitted as a patch stating exactly
// the field of that key's name, or admitted as one entry of a part of the
// file that is not a patch (its reparents or its ordering) — never refused as
// stating nothing, which is the refusal a key the patch translation forgot
// would get, and never a patch stating some other field, which is what a
// miswired row would make. The keys are read off the type, so a key added to
// the document is held to this with no edit here.
func TestABulkUpdateKeyStatesTheFieldItNames(t *testing.T) {
	samples := map[reflect.Type]any{
		reflect.TypeFor[string]():   "task",
		reflect.TypeFor[int]():      int(model.PriorityUrgent),
		reflect.TypeFor[[]string](): []string{"label"},
	}
	// The gate's own refusal of a document stating nothing, so that verdict is
	// recognised by what the gate says rather than by its wording.
	empty := storage.BulkIssueSpec{ID: "links-x-1"}
	_, emptyRefusal := storage.ParseBulkFile([]storage.BulkIssueSpec{empty}, nil)
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
		file, err := storage.ParseBulkFile([]storage.BulkIssueSpec{doc}, nil)
		switch {
		case err == nil:
			admitted++
			if len(file.Updates) == 0 {
				if carried := len(file.Reparents) + len(file.BlockedBy) + len(file.Ranked); carried != 1 {
					t.Errorf("a document stating only %s parsed to no patch and %d reparents, blocked_by sets or ranked entries, want 1", field.Name, carried)
				}
				continue
			}
			if len(file.Updates) != 1 {
				t.Fatalf("a document stating only %s parsed to %d updates, want 1", field.Name, len(file.Updates))
			}
			if got := statedFields(file.Updates[0].Patch); !slices.Equal(got, []string{field.Name}) {
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

// TestABulkUpdateDocumentRoundTripsItsIssue: the document stating an issue's
// editable state, written out as YAML and read back through the bulk gate,
// states every field of the patch and moves none of them, and restates the
// issue's parent and blocked-by set as they stand. The patch's fields are read
// off the type, so a field added to the table without its way back out of it
// fails here.
func TestABulkUpdateDocumentRoundTripsItsIssue(t *testing.T) {
	epic := model.Issue{ID: "links-x-epic", IssueType: model.TypeEpic}
	blocker := model.Issue{ID: "links-x-blocker", IssueType: model.TypeTask}
	issue := model.Issue{
		ID:          "links-x-1",
		Title:       "a title",
		Description: "first line\n\nsecond paragraph",
		Prompt:      "a prompt",
		Plan:        "a plan",
		IssueType:   model.TypeTask,
		Priority:    model.PriorityUrgent,
		Assignee:    "ada",
		Lane:        "a-lane",
		Labels:      []string{"alpha", "beta"},
	}
	text, err := yaml.Marshal(storage.BulkUpdateDocument(storage.IssueRelations{Issue: issue, Parent: &epic, BlockedBy: []model.Issue{blocker}}))
	if err != nil {
		t.Fatalf("marshal the document: %v", err)
	}
	specs, err := storage.ParseBulkSpecs(text)
	if err != nil {
		t.Fatalf("ParseBulkSpecs(the document) = %v; the document:\n%s", err, text)
	}
	file, err := storage.ParseBulkFile(specs, map[string]model.IssueType{issue.ID: issue.IssueType, epic.ID: epic.IssueType, blocker.ID: blocker.IssueType})
	if err != nil {
		t.Fatalf("ParseBulkFile(the document) = %v; the document:\n%s", err, text)
	}
	if len(file.Updates) != 1 {
		t.Fatalf("the document parsed to %d updates, want 1", len(file.Updates))
	}
	var every []string
	patchType := reflect.TypeFor[storage.UpdateIssueInput]()
	for i := range patchType.NumField() {
		if field := patchType.Field(i); field.Type.Kind() == reflect.Pointer {
			every = append(every, field.Name)
		}
	}
	if got := statedFields(file.Updates[0].Patch); !slices.Equal(got, every) {
		t.Errorf("the document's patch states %v, want every field %v", got, every)
	}
	after, changes, err := storage.ApplyIssueFields(issue, file.Updates[0].Patch)
	if err != nil {
		t.Fatalf("ApplyIssueFields(the issue, its own document) = %v", err)
	}
	if len(changes) != 0 || !reflect.DeepEqual(after, issue) {
		t.Errorf("the issue's own document moved %+v; after = %+v", changes, after)
	}
	if want := []storage.FileReparent{{ID: issue.ID, Parent: epic.ID}}; !slices.Equal(file.Reparents, want) {
		t.Errorf("reparents = %+v, want %+v", file.Reparents, want)
	}
	if len(file.BlockedBy) != 1 || !slices.Equal(file.BlockedBy[0].BlockedBy, []string{blocker.ID}) {
		t.Errorf("blocked_by = %+v, want %s's set [%s]", file.BlockedBy, issue.ID, blocker.ID)
	}
}

// TestABulkUpdateDocumentStatesEveryKeyAnUpdateOwns: the document states every
// key of the bulk file but the four an update cannot carry back — local_id
// names a create, topic is immutable, rank moves an issue rather than saying
// where it stands, and reason annotates an edit the document does not make —
// and states each of the others even when the issue's value is empty, the
// parent whenever there is one, so an agent sees every key it may edit. The keys are read off the type, so a key
// added to the file and left out of the document fails here.
func TestABulkUpdateDocumentStatesEveryKeyAnUpdateOwns(t *testing.T) {
	text, err := yaml.Marshal(storage.BulkUpdateDocument(storage.IssueRelations{Issue: model.Issue{ID: "links-x-1", Title: "bare", IssueType: model.TypeTask}, Parent: &model.Issue{ID: "links-x"}}))
	if err != nil {
		t.Fatalf("marshal the document: %v", err)
	}
	var stated map[string]any
	if err := yaml.Unmarshal(text, &stated); err != nil {
		t.Fatalf("unmarshal the document: %v", err)
	}
	notCarried := map[string]bool{"local_id": true, "topic": true, "rank": true, "reason": true}
	var want []string
	docType := reflect.TypeFor[storage.BulkIssueSpec]()
	for i := range docType.NumField() {
		key, _, _ := strings.Cut(docType.Field(i).Tag.Get("yaml"), ",")
		if !notCarried[key] {
			want = append(want, key)
		}
	}
	got := slices.Sorted(maps.Keys(stated))
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the document states keys %v, want %v; the document:\n%s", got, want, text)
	}
}

// A leaf older data left with no epic reads out as a document stating no
// parent, which the gate admits as stating nothing about where it sits, where
// `parent: ""` would be a clear the gate refuses for a leaf.
func TestTheDocumentOfAnIssueWithNoParentStatesNone(t *testing.T) {
	leaf := model.Issue{ID: "lit-loose", Title: "loose", IssueType: model.TypeTask}
	doc := storage.BulkUpdateDocument(storage.IssueRelations{Issue: leaf})
	if doc.Parent != nil {
		t.Fatalf("the document states parent %q, want none", *doc.Parent)
	}
	if _, err := storage.ParseBulkFile([]storage.BulkIssueSpec{doc}, map[string]model.IssueType{leaf.ID: model.TypeTask}); err != nil {
		t.Errorf("ParseBulkFile(the loose leaf's own document) error = %v", err)
	}
}

// TestEveryFieldRestoresTheValueHistoryRecorded: for every editable field, the
// value a change row records, restored onto a document and imported, lands as
// exactly that value and moves no other field. The fields are read off the
// table, so a field that joins it without a way back fails here.
func TestEveryFieldRestoresTheValueHistoryRecorded(t *testing.T) {
	recorded := model.Issue{
		ID:          "links-x-1",
		Title:       "a title",
		Description: "first line\n\nsecond paragraph",
		Prompt:      "a prompt",
		Plan:        "a plan",
		IssueType:   model.TypeTask,
		Priority:    model.PriorityUrgent,
		Assignee:    "ada",
		Lane:        "a-lane",
		Labels:      []string{"alpha", "beta"},
	}
	base := model.Issue{ID: recorded.ID, Title: "other", IssueType: model.TypeBug, Priority: model.PriorityNormal, Labels: []string{"gamma"}}
	values := storage.IssueFieldValues(recorded)
	for _, name := range storage.IssueFieldNames() {
		t.Run(name, func(t *testing.T) {
			doc := storage.BulkIssueSpec{ID: recorded.ID}
			if err := storage.RestoreIssueField(&doc, name, values[name]); err != nil {
				t.Fatalf("RestoreIssueField(%s, %q) = %v", name, values[name], err)
			}
			text, err := yaml.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal the document: %v", err)
			}
			specs, err := storage.ParseBulkSpecs(text)
			if err != nil {
				t.Fatalf("ParseBulkSpecs = %v; the document:\n%s", err, text)
			}
			file, err := storage.ParseBulkFile(specs, map[string]model.IssueType{recorded.ID: base.IssueType})
			if err != nil {
				t.Fatalf("ParseBulkFile = %v; the document:\n%s", err, text)
			}
			after, changes, err := storage.ApplyIssueFields(base, file.Updates[0].Patch)
			if err != nil {
				t.Fatalf("ApplyIssueFields = %v", err)
			}
			if got := storage.IssueFieldValues(after)[name]; got != values[name] {
				t.Errorf("%s restored as %q, want the recorded %q", name, got, values[name])
			}
			if len(changes) != 1 || changes[0].Field != name {
				t.Errorf("restoring %s moved %+v, want %s alone", name, changes, name)
			}
		})
	}
}

// TestRestoringAFieldNoRowCarriesIsRefused: a change row naming no editable
// field has no way back, and saying so beats stating nothing.
func TestRestoringAFieldNoRowCarriesIsRefused(t *testing.T) {
	var doc storage.BulkIssueSpec
	if err := storage.RestoreIssueField(&doc, "topic", "x"); err == nil {
		t.Fatalf("RestoreIssueField(topic) = nil; want a refusal, topic is not editable")
	}
}

// TestWithIssueFieldsOfCarriesTheEditableFieldsAndNothingElse: what is left
// different after carrying one issue's fields onto another is what differs
// outside the editable set.
func TestWithIssueFieldsOfCarriesTheEditableFieldsAndNothingElse(t *testing.T) {
	src := model.Issue{ID: "links-x-1", Title: "new", IssueType: model.TypeTask, Lane: "l", Labels: []string{"a"}, Rank: "b"}
	dst := model.Issue{ID: "links-x-1", Title: "old", IssueType: model.TypeTask, Labels: []string{}, Rank: "a"}
	got, err := storage.WithIssueFieldsOf(dst, src)
	if err != nil {
		t.Fatalf("WithIssueFieldsOf = %v", err)
	}
	if !maps.Equal(storage.IssueFieldValues(got), storage.IssueFieldValues(src)) {
		t.Errorf("fields = %v, want src's %v", storage.IssueFieldValues(got), storage.IssueFieldValues(src))
	}
	if got.Rank != dst.Rank {
		t.Errorf("rank = %q, want dst's %q: rank is not an editable field", got.Rank, dst.Rank)
	}
}
