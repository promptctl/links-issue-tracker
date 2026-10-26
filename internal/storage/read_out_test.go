package storage_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// readOut is the document `lit ls --format yaml` prints for issue under
// parent, blocked by blockers.
func readOut(issue model.Issue, parent string, blockers ...string) storage.BulkIssueSpec {
	rel := storage.IssueRelations{Issue: issue}
	if parent != "" {
		rel.Parent = &model.Issue{ID: parent, IssueType: model.TypeEpic}
	}
	for _, id := range blockers {
		rel.BlockedBy = append(rel.BlockedBy, model.Issue{ID: id})
	}
	return storage.BulkUpdateDocument(rel)
}

// standing is the store holding issue with the given parents and blockers.
func standing(issue model.Issue, parents []string, blockers ...string) storage.CurrentState {
	return storage.CurrentState{
		Issues:    map[string]model.Issue{issue.ID: issue},
		Parents:   map[string][]string{issue.ID: parents},
		BlockedBy: map[string][]string{issue.ID: blockers},
	}
}

func readIssue() model.Issue {
	return model.Issue{ID: "lit-x-1", Title: "a title", IssueType: model.TypeTask, Lane: "a-lane", Labels: []string{"alpha", "beta"}}
}

func mergeOK(t *testing.T, doc storage.BulkIssueSpec, now storage.CurrentState) (storage.BulkIssueSpec, []string) {
	t.Helper()
	merged, err := storage.MergeReadOut([]storage.BulkIssueSpec{doc}, now)
	if err != nil {
		t.Fatalf("MergeReadOut error = %v", err)
	}
	file, err := storage.ParseBulkFile(merged, map[string]model.IssueType{doc.ID: now.Issues[doc.ID].IssueType, "lit-e": model.TypeEpic, "lit-f": model.TypeEpic})
	if err != nil {
		t.Fatalf("ParseBulkFile(the merged file) error = %v", err)
	}
	var out storage.BulkIssueSpec
	out.ID = doc.ID
	for _, update := range file.Updates {
		out.Lane, out.Labels, out.Title = update.Patch.Lane, update.Patch.Labels, update.Patch.Title
	}
	for _, r := range file.Reparents {
		out.Parent = &r.Parent
	}
	for _, b := range file.BlockedBy {
		out.BlockedBy = &b.BlockedBy
	}
	kept := make([]string, len(merged.Kept))
	for i, slot := range merged.Kept {
		kept[i] = slot.String()
	}
	return out, kept
}

func mergeConflicts(t *testing.T, doc storage.BulkIssueSpec, now storage.CurrentState) []storage.Conflict {
	t.Helper()
	_, err := storage.MergeReadOut([]storage.BulkIssueSpec{doc}, now)
	var conflict storage.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("MergeReadOut error = %v, want a ConflictError", err)
	}
	return conflict.Conflicts
}

// A slot the file left as read takes the value another writer moved it to,
// and is kept; an edit beside it lands.
func TestAReadOutFileLeftAloneTakesTheValueNow(t *testing.T) {
	read := readIssue()
	doc := readOut(read, "lit-e", "lit-b")
	retitled := "retitled"
	doc.Title = &retitled
	now := read
	now.Lane = "their-lane"
	got, kept := mergeOK(t, doc, standing(now, []string{"lit-e"}, "lit-b", "lit-y"))
	if *got.Title != retitled || *got.Lane != "their-lane" {
		t.Errorf("title %q, lane %q; want the file's %q and the other writer's lane", *got.Title, *got.Lane, retitled)
	}
	if !slices.Equal(*got.BlockedBy, []string{"lit-b", "lit-y"}) {
		t.Errorf("blocked_by = %v, want the read edge and the other writer's", *got.BlockedBy)
	}
	if want := []string{"lit-x-1 lane", "lit-x-1 blocked-by lit-y"}; !slices.Equal(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
}

// An edit on a slot another writer moved elsewhere is a conflict that names
// the slot, what it holds now, and the way on.
func TestAReadOutEditOverAnotherWritersIsAConflict(t *testing.T) {
	read := readIssue()
	doc := readOut(read, "lit-e")
	ours := "our-lane"
	doc.Lane = &ours
	now := read
	now.Lane = "their-lane"
	conflicts := mergeConflicts(t, doc, standing(now, []string{"lit-e"}))
	if len(conflicts) != 1 || conflicts[0].Slot != (storage.Slot{Kind: storage.FieldSlot, Issue: read.ID, Name: "lane"}) || conflicts[0].Since != (storage.ReadOut{}) {
		t.Fatalf("conflicts = %v, want one on the lane, against the read", conflicts)
	}
	text := storage.ConflictError{Conflicts: conflicts}.Error()
	if !strings.Contains(text, `"their-lane" now`) || !strings.Contains(text, "lit ls --ids") {
		t.Errorf("message = %s, want the value now and the way on", text)
	}
}

// Both writers making one edit is no conflict: the value lands as held.
func TestAReadOutEditAnotherWriterAlsoMadeLands(t *testing.T) {
	read := readIssue()
	doc := readOut(read, "lit-e")
	same := "same-lane"
	doc.Lane = &same
	now := read
	now.Lane = same
	got, kept := mergeOK(t, doc, standing(now, []string{"lit-e"}))
	if *got.Lane != same || len(kept) != 0 {
		t.Errorf("lane %q, kept %v; want %q and nothing kept", *got.Lane, kept, same)
	}
}

// What the write would store decides whether the file edited a field, so
// reordered or recased labels and a title with a trailing space are left as
// read, and take the value another writer moved them to.
func TestAReadOutValueIsComparedAsTheWriteWouldStoreIt(t *testing.T) {
	read := readIssue()
	doc := readOut(read, "lit-e")
	reordered, spaced := []string{"Beta", "alpha"}, "a title  "
	doc.Labels, doc.Title = &reordered, &spaced
	now := read
	now.Labels, now.Title = []string{"gamma"}, "their title"
	got, kept := mergeOK(t, doc, standing(now, []string{"lit-e"}))
	if !slices.Equal(*got.Labels, []string{"gamma"}) || *got.Title != "their title" {
		t.Errorf("labels %v, title %q; want the other writer's", *got.Labels, *got.Title)
	}
	if want := []string{"lit-x-1 title", "lit-x-1 labels"}; !slices.Equal(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
}

// Blocked-by merges edge by edge: an edge the file adds and one another
// writer adds both stand, and an edge the file drops goes.
func TestAReadOutBlockedByMergesEdgeByEdge(t *testing.T) {
	read := readIssue()
	doc := readOut(read, "lit-e", "lit-b")
	edges := []string{"lit-z"}
	doc.BlockedBy = &edges
	got, kept := mergeOK(t, doc, standing(read, []string{"lit-e"}, "lit-b", "lit-y"))
	if !slices.Equal(*got.BlockedBy, []string{"lit-y", "lit-z"}) {
		t.Errorf("blocked_by = %v, want [lit-y lit-z]", *got.BlockedBy)
	}
	if want := []string{"lit-x-1 blocked-by lit-y"}; !slices.Equal(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
}

// A parent the file clears is stated as a clear while the issue holds one, so
// the gate judges it: a sub-epic ends top-level, and a leaf's clear is refused
// as it always is. A parent another writer cleared since the read, left alone
// by the file, states nothing.
func TestAReadOutParentResultIsStatedAsAMoveFromNow(t *testing.T) {
	none := ""
	subEpic := model.Issue{ID: "lit-x-2", Title: "sub-epic", IssueType: model.TypeEpic}
	clearedEpic := readOut(subEpic, "lit-e")
	clearedEpic.Parent = &none
	if got, _ := mergeOK(t, clearedEpic, standing(subEpic, []string{"lit-e"})); got.Parent == nil || *got.Parent != "" {
		t.Errorf("a sub-epic's cleared parent merged to %v, want a clear", got.Parent)
	}
	read := readIssue()
	cleared := readOut(read, "lit-e")
	cleared.Parent = &none
	merged, err := storage.MergeReadOut([]storage.BulkIssueSpec{cleared}, standing(read, []string{"lit-e"}))
	if err != nil {
		t.Fatalf("MergeReadOut error = %v", err)
	}
	if _, err := storage.ParseBulkFile(merged, map[string]model.IssueType{read.ID: model.TypeTask}); err == nil {
		t.Error("the gate admitted a leaf's clear; want the detach refusal")
	}
	left := readOut(read, "lit-e")
	got, kept := mergeOK(t, left, standing(read, nil))
	if got.Parent != nil || !slices.Equal(kept, []string{"lit-x-1 parent"}) {
		t.Errorf("parent %v, kept %v; want none stated and the parent kept", got.Parent, kept)
	}
}

// A key the file deletes is not merged: the slot stands as it is.
func TestAReadOutKeyTheFileDeletesIsLeftAlone(t *testing.T) {
	read := readIssue()
	doc := readOut(read, "lit-e", "lit-b")
	doc.Parent, doc.BlockedBy = nil, nil
	got, kept := mergeOK(t, doc, standing(read, []string{"lit-f"}))
	if got.Parent != nil || got.BlockedBy != nil || len(kept) != 0 {
		t.Errorf("parent %v, blocked_by %v, kept %v; want nothing stated or kept", got.Parent, got.BlockedBy, kept)
	}
}

// An issue the store no longer holds is a conflict whatever the file says, and
// the refusal names the issues to read out again.
func TestAReadOutOfARemovedIssueIsAConflict(t *testing.T) {
	removed := mergeConflicts(t, readOut(readIssue(), "lit-e"), storage.CurrentState{})
	if len(removed) == 0 || !strings.Contains(removed[0].String(), "is removed now") {
		t.Errorf("conflicts = %v, want each slot of the removed issue", removed)
	}
	if way := (storage.ConflictError{Conflicts: removed}).Error(); !strings.Contains(way, "`lit ls --ids lit-x-1 --format yaml`") {
		t.Errorf("refusal = %q, want the read-out command for lit-x-1", way)
	}
}

// A child holding two parents now, which no document can state: a file that
// left its parent alone states none, so the two stand and the rest of the file
// lands; a file that edited it conflicts, and the refusal names the repair.
func TestAReadOutOfAChildWithTwoParentsNow(t *testing.T) {
	read := readIssue()
	twoParents := standing(read, []string{"lit-e", "lit-f"})
	left := readOut(read, "lit-e")
	title := "a new title"
	left.Title = &title
	got, kept := mergeOK(t, left, twoParents)
	if got.Parent != nil || got.Title == nil || *got.Title != title || !slices.Equal(kept, []string{"lit-x-1 parent"}) {
		t.Errorf("parent %v, title %v, kept %v; want no parent stated, the title, and the parent kept", got.Parent, got.Title, kept)
	}
	edited := readOut(read, "lit-e")
	none := ""
	edited.Parent = &none
	conflicts := mergeConflicts(t, edited, twoParents)
	if len(conflicts) != 1 || !strings.Contains(conflicts[0].String(), "lit doctor") {
		t.Errorf("conflicts = %v, want the parent, naming lit doctor", conflicts)
	}
}

// A read-out document meets the gate's document rules as stated, as one with
// no basis does: the merge does not make a blocked_by naming an edge twice
// into a set, and a file the gate refuses is refused before any conflict.
func TestAReadOutDocumentIsGatedAsStated(t *testing.T) {
	read := readIssue()
	doc := readOut(read, "", "lit-b")
	twice := []string{"lit-b", "lit-b"}
	doc.BlockedBy = &twice
	_, err := storage.MergeReadOut([]storage.BulkIssueSpec{doc}, storage.CurrentState{})
	var invalid model.ValidationError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "twice") {
		t.Errorf("error = %v, want the gate's refusal of an edge named twice, before the removed issue's conflict", err)
	}
}

// A document with no basis is never read: it passes through as stated, so
// the write judges it, even for an issue the store does not hold.
func TestADocumentWithNoBasisPassesThrough(t *testing.T) {
	lane := "a-lane"
	doc := storage.BulkIssueSpec{ID: "lit-missing", Lane: &lane}
	merged, err := storage.MergeReadOut([]storage.BulkIssueSpec{doc}, storage.CurrentState{})
	if err != nil || len(merged.Kept) != 0 {
		t.Fatalf("MergeReadOut = %v, kept %v; want it through untouched", err, merged.Kept)
	}
	if ids := storage.ReadOutIDs([]storage.BulkIssueSpec{doc}); len(ids) != 0 {
		t.Errorf("ReadOutIDs = %v, want none to read", ids)
	}
}

// A basis the merge cannot read refuses the file as invalid input.
func TestAMalformedBasisIsRefused(t *testing.T) {
	read := readIssue()
	unknown := readOut(read, "lit-e")
	unknown.Basis.Fields["colour"] = "0000"
	noID := readOut(read, "lit-e")
	noID.ID = ""
	for name, doc := range map[string]storage.BulkIssueSpec{"an unknown field": unknown, "no id": noID} {
		_, err := storage.MergeReadOut([]storage.BulkIssueSpec{doc}, standing(read, []string{"lit-e"}))
		var invalid model.ValidationError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: error = %v, want a ValidationError", name, err)
		}
	}
}
