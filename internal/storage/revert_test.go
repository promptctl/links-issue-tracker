package storage_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func titleSlot(issue string) storage.Slot {
	return storage.Slot{Kind: storage.FieldSlot, Issue: issue, Name: "title"}
}

// fieldWrite is a write that moved one field and recorded exactly that move.
func fieldWrite(t *testing.T, write string, slot storage.Slot, from, to string) storage.WriteDelta {
	t.Helper()
	delta, refused := storage.Classify(storage.WriteRows{
		Write:   write,
		Fields:  []storage.Move{{Slot: slot, From: storage.FieldValue(from), To: storage.FieldValue(to)}},
		Records: []model.IssueEvent{{IssueID: slot.Issue, Changes: []model.FieldChange{{Field: slot.Name, From: from, To: to}}}},
	})
	if len(refused) > 0 {
		t.Fatalf("Classify(%s) refused %v", write, refused)
	}
	return delta
}

func revertOK(t *testing.T, deltas []storage.WriteDelta, now storage.CurrentState) storage.Reverted {
	t.Helper()
	reverted, conflicts := storage.Revert(deltas, now)
	if len(conflicts) > 0 {
		t.Fatalf("Revert conflicts = %v", conflicts)
	}
	return reverted
}

// A field a write moved, still where the write left it, goes back, and the
// document carries the undo's reason beside it.
func TestRevertPutsAFieldBack(t *testing.T) {
	delta := fieldWrite(t, "w1", titleSlot("a"), "old", "new")
	reverted := revertOK(t, []storage.WriteDelta{delta}, storage.CurrentState{Fields: map[storage.Slot]storage.SlotValue{titleSlot("a"): storage.FieldValue("new")}})
	if len(reverted.Documents) != 1 || reverted.Documents[0].Title == nil || *reverted.Documents[0].Title != "old" {
		t.Fatalf("documents = %+v, want a's title stated as old", reverted.Documents)
	}
	if reverted.Documents[0].Reason != "undo w1" {
		t.Errorf("reason = %q, want %q", reverted.Documents[0].Reason, "undo w1")
	}
}

// A slot a later write moved again is a conflict: putting the old value back
// would erase that write. Nothing is stated.
func TestRevertRefusesASlotALaterWriteMoved(t *testing.T) {
	delta := fieldWrite(t, "w1", titleSlot("a"), "old", "new")
	reverted, conflicts := storage.Revert([]storage.WriteDelta{delta}, storage.CurrentState{Fields: map[storage.Slot]storage.SlotValue{titleSlot("a"): storage.FieldValue("later")}})
	if len(conflicts) != 1 || conflicts[0].Now != storage.FieldValue("later") || conflicts[0].Left != storage.FieldValue("new") {
		t.Fatalf("conflicts = %v, want one naming new (left) and later (now)", conflicts)
	}
	if len(reverted.Documents) != 0 {
		t.Errorf("a conflicted revert stated %+v", reverted.Documents)
	}
}

// Writes fold newest first: two writes in a row on one field revert to the
// value before the older one.
func TestRevertFoldsWritesNewestFirst(t *testing.T) {
	older := fieldWrite(t, "w1", titleSlot("a"), "one", "two")
	newer := fieldWrite(t, "w2", titleSlot("a"), "two", "three")
	now := storage.CurrentState{Fields: map[storage.Slot]storage.SlotValue{titleSlot("a"): storage.FieldValue("three")}}
	reverted := revertOK(t, []storage.WriteDelta{newer, older}, now)
	if got := *reverted.Documents[0].Title; got != "one" {
		t.Errorf("title = %q, want one", got)
	}
	if _, conflicts := storage.Revert([]storage.WriteDelta{older, newer}, now); len(conflicts) == 0 {
		t.Errorf("oldest-first folded without conflict; the older write's value was moved by the newer")
	}
}

// A write and the later write that undid it, reverted together, leave the
// store where it stands, so nothing is stated.
func TestRevertOfAWriteAndItsUndoStatesNothing(t *testing.T) {
	write := fieldWrite(t, "w1", titleSlot("a"), "old", "new")
	undo := fieldWrite(t, "w2", titleSlot("a"), "new", "old")
	reverted := revertOK(t, []storage.WriteDelta{undo, write}, storage.CurrentState{Fields: map[storage.Slot]storage.SlotValue{titleSlot("a"): storage.FieldValue("old")}})
	if len(reverted.Documents) != 0 || len(reverted.Moves) != 0 {
		t.Errorf("documents = %+v, moves = %v; want nothing", reverted.Documents, reverted.Moves)
	}
}

// A reparent goes back to the parent before it, or to none, with no reason:
// a parent is not a field and the gate refuses a reason beside no field.
func TestRevertPutsAParentBack(t *testing.T) {
	for _, before := range [][]string{{"epic-1"}, nil} {
		slot := storage.Slot{Kind: storage.ParentSlot, Issue: "a"}
		delta, refused := storage.Classify(storage.WriteRows{Write: "w1", Parents: []storage.Move{{Slot: slot, From: storage.ParentsValue(before), To: storage.ParentsValue([]string{"epic-2"})}}})
		if len(refused) > 0 {
			t.Fatalf("Classify refused %v", refused)
		}
		reverted := revertOK(t, []storage.WriteDelta{delta}, storage.CurrentState{Parents: map[string][]string{"a": {"epic-2"}}})
		want := strings.Join(before, "")
		if doc := reverted.Documents[0]; doc.Parent == nil || *doc.Parent != want || doc.Reason != "" {
			t.Errorf("before %v: document = %+v, want parent %q and no reason", before, doc, want)
		}
	}
}

// A child that holds two parents now does not hold the one the write left, so
// putting its old parent back is a conflict.
func TestRevertRefusesAChildWithTwoParentsNow(t *testing.T) {
	slot := storage.Slot{Kind: storage.ParentSlot, Issue: "a"}
	delta, _ := storage.Classify(storage.WriteRows{Write: "w1", Parents: []storage.Move{{Slot: slot, From: storage.ParentsValue([]string{"e1"}), To: storage.ParentsValue([]string{"e2"})}}})
	if _, conflicts := storage.Revert([]storage.WriteDelta{delta}, storage.CurrentState{Parents: map[string][]string{"a": {"e2", "e3"}}}); len(conflicts) != 1 {
		t.Errorf("conflicts = %v, want one", conflicts)
	}
}

// A write that started from two parents cannot be put back by a document,
// which states one parent or none.
func TestClassifyRefusesAWriteThatStartedFromTwoParents(t *testing.T) {
	slot := storage.Slot{Kind: storage.ParentSlot, Issue: "a"}
	_, refused := storage.Classify(storage.WriteRows{Write: "w1", Parents: []storage.Move{{Slot: slot, From: storage.ParentsValue([]string{"e1", "e2"}), To: storage.ParentsValue([]string{"e3"})}}})
	if len(refused) != 1 || !strings.Contains(refused[0].Reverse, "lit doctor") {
		t.Errorf("refused = %v, want one naming lit doctor", refused)
	}
}

// A blocked-by edge a write added goes; an edge added since stays.
func TestRevertKeepsAnEdgeALaterWriteAdded(t *testing.T) {
	slot := storage.Slot{Kind: storage.BlockedBySlot, Issue: "a", Name: "x"}
	delta, _ := storage.Classify(storage.WriteRows{Write: "w1", BlockedBy: []storage.Move{{Slot: slot, From: storage.EdgeValue(false), To: storage.EdgeValue(true)}}})
	reverted := revertOK(t, []storage.WriteDelta{delta}, storage.CurrentState{BlockedBy: map[string][]string{"a": {"x", "y"}}})
	if doc := reverted.Documents[0]; doc.BlockedBy == nil || !reflect.DeepEqual(*doc.BlockedBy, []string{"y"}) {
		t.Errorf("document = %+v, want blocked_by [y]", doc)
	}
}

// What a write moved outside the slots, an event with an action, a field the
// write recorded no move of, and a record the store disagrees with are each
// refused, by name.
func TestClassifyRefusesWhatNoDocumentCanPutBack(t *testing.T) {
	cases := []struct {
		name string
		rows storage.WriteRows
		want string
	}{
		{"a lifecycle write", storage.WriteRows{Records: []model.IssueEvent{{IssueID: "a", Action: "reopen"}}}, "`lit open`"},
		{"a creation", storage.WriteRows{Records: []model.IssueEvent{{IssueID: "a", Action: model.ActionCreated}}}, `"created"`},
		{"a field moved with no record", storage.WriteRows{Fields: []storage.Move{{Slot: titleSlot("a"), From: storage.FieldValue("x"), To: storage.FieldValue("y")}}}, "no field-change record"},
		{"a record the store did not move", storage.WriteRows{Records: []model.IssueEvent{{IssueID: "a", Changes: []model.FieldChange{{Field: "lane", From: "", To: "x"}}}}}, "sync replay"},
		{"a record the store moved otherwise", storage.WriteRows{
			Fields:  []storage.Move{{Slot: storage.Slot{Kind: storage.FieldSlot, Issue: "a", Name: "priority"}, From: storage.FieldValue("3"), To: storage.FieldValue("1")}},
			Records: []model.IssueEvent{{IssueID: "a", Changes: []model.FieldChange{{Field: "priority", From: "2", To: "1"}}}}}, "sync replay"},
		{"something outside the slots", storage.WriteRows{Outside: []storage.Unrevertable{{What: "a rank moved", Reverse: "lit rank"}}}, "lit rank"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.rows.Write = "w1"
			delta, refused := storage.Classify(tc.rows)
			if len(refused) != 1 || !strings.Contains(refused[0].String(), tc.want) {
				t.Fatalf("refused = %v, want one naming %s", refused, tc.want)
			}
			if len(delta.Moves()) != 0 {
				t.Errorf("a refused write classified moves %v", delta.Moves())
			}
		})
	}
}
