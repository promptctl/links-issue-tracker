package storage

import (
	"fmt"
	"maps"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// A file read out with `lit ls --format yaml` states every slot of each issue
// as it stood at the read, and an import writes what a file states. Other
// writers move those slots in between; writing back every value the file was
// read with would erase their writes: the lost update undo's Revert refuses for
// the writes it puts back. So each read-out document carries its Basis, and
// MergeReadOut makes a three-way merge of every slot the basis covers: the
// base it was read with, the file's value, and the value now, read under the
// write's lock. Only a basis proves a read, so a slot with none is never read
// here: it passes through as stated, for the write to judge.
// [LAW:one-source-of-truth] the slots, their values and the conflict are
// undo's own (revert.go).

// MergedFile is a bulk file whose read-out documents are merged onto the store
// as it stands, which is what the gate judges (ParseBulkFile). Only
// MergeReadOut makes one, so no engine can write a read-out file over the
// writes made since its read. [LAW:parse-dont-validate]
type MergedFile struct {
	specs []BulkIssueSpec
	// Kept is every slot a read-out document left as it was read and another
	// writer moved since, in document order: the merge kept that writer's value.
	Kept []Slot
}

// ReadOutIDs is the issues a bulk file's read-out documents name, which an
// engine reads as CurrentState under its write's lock for MergeReadOut.
func ReadOutIDs(specs []BulkIssueSpec) []string {
	var ids []string
	for _, spec := range specs {
		if spec.Basis != nil {
			ids = append(ids, spec.ID)
		}
	}
	return ids
}

// MergeReadOut merges each read-out document of a bulk file onto now, the
// store as it stands for ReadOutIDs. For each slot a document states and its
// basis covers, where base is the value it was read with, file the document's
// and now the store's:
//
//   - file is base: the file left it alone, so the document states now, and a
//     now that moved from base is Kept;
//   - file is not base, and now is base or file: the file's edit, stated;
//   - otherwise, or when the store no longer holds the issue: a conflict.
//
// Every conflict in the file is gathered into one ConflictError, which writes
// nothing. A merged document still states every key the file stated, so one
// the file left alone lands as held, as an unedited read-out always has.
//
// The documents as stated pass the gate's document rules first, so the merge
// never makes a document the gate would refuse as written into one it admits,
// and a file the gate refuses is refused before any conflict is weighed: the
// same rules a document with no basis meets. [LAW:single-enforcer]
func MergeReadOut(specs []BulkIssueSpec, now CurrentState) (MergedFile, error) {
	if _, _, err := parseBulkSpecs(specs); err != nil {
		return MergedFile{}, err
	}
	merged := MergedFile{specs: slices.Clone(specs)}
	var conflicts []Conflict
	for i := range merged.specs {
		m := documentMerge{now: now}
		if err := m.merge(i, &merged.specs[i]); err != nil {
			return MergedFile{}, err
		}
		merged.Kept = append(merged.Kept, m.kept...)
		conflicts = append(conflicts, m.conflicts...)
	}
	if len(conflicts) > 0 {
		return MergedFile{}, ConflictError{Conflicts: conflicts}
	}
	return merged, nil
}

// documentMerge is the merge of one document: what it kept and what conflicted.
type documentMerge struct {
	now       CurrentState
	kept      []Slot
	conflicts []Conflict
}

// merge merges doc in place. The basis is consumed: the gate sees the merged
// document, which states what lands.
func (m *documentMerge) merge(i int, doc *BulkIssueSpec) error {
	basis := doc.Basis
	if basis == nil {
		return nil
	}
	doc.Basis = nil
	if doc.ID == "" {
		return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d has a basis but no id; a basis records the issue a document was read out of", i)}
	}
	if err := m.fields(i, doc, basis.Fields); err != nil {
		return err
	}
	if doc.Parent != nil {
		m.parent(doc, basis.Parent)
	}
	if doc.BlockedBy != nil {
		m.blockedBy(doc, basis.BlockedBy)
	}
	return nil
}

// fields merges each stated field the basis covers. The file's value of a
// field is the one its write would store: the document's patch applied to the
// issue as it stands, through the rows every write applies, so a value the
// write canonicalizes (labels sorted, text trimmed) compares as it would land.
// [LAW:single-enforcer]
func (m *documentMerge) fields(i int, doc *BulkIssueSpec, basis map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(basis)) {
		if !slices.ContainsFunc(issueFields, func(field issueField) bool { return field.name == name }) {
			return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) has a basis for %q, which is not an editable field", i, doc.ID, name)}
		}
	}
	patch, err := bulkUpdatePatch(*doc)
	if err != nil {
		return fmt.Errorf("bulk: doc %d (id %q): %w", i, doc.ID, err)
	}
	issue, standing := m.now.Issues[doc.ID]
	var file map[string]string
	if standing {
		applied, _, err := ApplyIssueFields(issue, patch)
		if err != nil {
			return fmt.Errorf("bulk: doc %d (id %q): %w", i, doc.ID, err)
		}
		file = IssueFieldValues(applied)
	}
	for _, field := range issueFields {
		base, covered := basis[field.name]
		if !covered || !field.stated(patch) {
			continue
		}
		slot := Slot{Kind: FieldSlot, Issue: doc.ID, Name: field.name}
		if merged, ok := m.slot(slot, base, FieldValue(file[field.name])); ok {
			if err := field.restore(doc, merged.text); err != nil {
				return fmt.Errorf("bulk: doc %d (id %q): %w", i, doc.ID, err)
			}
		}
	}
	return nil
}

// parent merges a stated parent and states the result as a move from now: a
// parent as itself, none as a clear only when now holds a parent to clear, so
// the gate judges a clear exactly when one is written, and nothing when what
// results is what stands, as the two parents of a child the file left alone
// do: no document can state them, and `lit doctor` repairs them.
func (m *documentMerge) parent(doc *BulkIssueSpec, base string) {
	slot := Slot{Kind: ParentSlot, Issue: doc.ID}
	merged, ok := m.slot(slot, ParentsValue(parentSet(base)).digest(), ParentsValue(parentSet(*doc.Parent)))
	if !ok {
		return
	}
	switch parents := merged.parents(); {
	case len(parents) == 1:
		doc.Parent = &parents[0]
	case len(parents) == 0 && len(m.now.value(slot).parents()) > 0:
		clear := ""
		doc.Parent = &clear
	default:
		doc.Parent = nil
	}
}

// blockedBy merges a stated blocked-by set edge by edge, every edge the base,
// the file or now holds, and states the set that results whole.
func (m *documentMerge) blockedBy(doc *BulkIssueSpec, base []string) {
	edges := slices.Concat(base, *doc.BlockedBy, m.now.BlockedBy[doc.ID])
	slices.Sort(edges)
	merged := []string{}
	for _, blocker := range slices.Compact(edges) {
		slot := Slot{Kind: BlockedBySlot, Issue: doc.ID, Name: blocker}
		value, ok := m.slot(slot, EdgeValue(slices.Contains(base, blocker)).digest(), EdgeValue(slices.Contains(*doc.BlockedBy, blocker)))
		if ok && value == EdgeValue(true) {
			merged = append(merged, blocker)
		}
	}
	doc.BlockedBy = &merged
}

// slot is one slot's three-way merge, given the digest of the value it was
// read with and the file's value: the value the merged document states, or
// false when it conflicts.
func (m *documentMerge) slot(slot Slot, base string, file SlotValue) (SlotValue, bool) {
	now := m.now.value(slot)
	switch {
	case now.removed:
	case file.digest() == base:
		if now.digest() != base {
			m.kept = append(m.kept, slot)
		}
		return now, true
	case now.digest() == base, now == file:
		return file, true
	}
	m.conflicts = append(m.conflicts, Conflict{Slot: slot, Now: now, Since: ReadOut{}})
	return SlotValue{}, false
}

// parentSet is a document's parent as a set: none for the empty string.
func parentSet(parent string) []string {
	if parent == "" {
		return nil
	}
	return []string{parent}
}
