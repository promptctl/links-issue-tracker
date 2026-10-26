package storage

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/model/lifecycle"
)

// Undo reverts writes after the fact, from the history the store already
// keeps: what a write changed is read off the store on both sides of it, and
// its inverse is stated as bulk update documents that land through the one
// bulk-edit path. This file is the pure core: Classify decides what of one
// write an update document can put back, and Revert folds the chosen writes
// onto the store as it stands now. [LAW:effects-at-boundaries]

// SlotKind is what kind of thing a Slot names: exactly the things an update
// document can state.
type SlotKind int

const (
	// FieldSlot is one editable field of an issue; Slot.Name is the field's
	// name in history's vocabulary (IssueFieldNames).
	FieldSlot SlotKind = iota
	// ParentSlot is an issue's set of parents. Stored data can hold two, so
	// the value is the set, though a document states one or none.
	ParentSlot
	// BlockedBySlot is one blocked-by edge; Slot.Name is the blocker.
	BlockedBySlot
)

// Slot is one thing a write moved and a document can put back.
type Slot struct {
	Kind  SlotKind
	Issue string
	Name  string
}

func (s Slot) String() string {
	switch s.Kind {
	case FieldSlot:
		return s.Issue + " " + s.Name
	case ParentSlot:
		return s.Issue + " parent"
	case BlockedBySlot:
		return s.Issue + " blocked-by " + s.Name
	}
	panic(fmt.Sprintf("storage: slot kind %d", s.Kind))
}

// SlotValue is a slot's value in the one spelling every comparison uses: a
// field's history encoding, a parent set, or an edge's presence. Its fields
// are unexported so the constructors below are the only spellings.
// [LAW:types-are-the-program]
type SlotValue struct {
	text string
}

// FieldValue is a field's value as a change row records it.
func FieldValue(recorded string) SlotValue { return SlotValue{text: recorded} }

// ParentsValue is a parent set. Order is not a set fact, so it is sorted.
func ParentsValue(ids []string) SlotValue {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return SlotValue{text: strings.Join(sorted, ",")}
}

// EdgeValue is whether an edge is there.
func EdgeValue(present bool) SlotValue {
	if present {
		return SlotValue{text: "present"}
	}
	return SlotValue{text: "absent"}
}

func (v SlotValue) parents() []string {
	if v.text == "" {
		return nil
	}
	return strings.Split(v.text, ",")
}

// Display is the value as a reader sees it beside its slot.
func (v SlotValue) Display(kind SlotKind) string {
	if kind == ParentSlot && v.text == "" {
		return "none"
	}
	if kind == FieldSlot {
		return fmt.Sprintf("%q", v.text)
	}
	return v.text
}

// Move is one slot a write moved, from the value before it to the value after.
type Move struct {
	Slot     Slot
	From, To SlotValue
}

func (m Move) String() string {
	return fmt.Sprintf("%s: %s → %s", m.Slot, m.From.Display(m.Slot.Kind), m.To.Display(m.Slot.Kind))
}

// Unrevertable is something a write changed that no update document can put
// back, and how to reverse it instead when there is a way.
type Unrevertable struct {
	Write   string
	What    string
	Reverse string
}

func (u Unrevertable) String() string {
	if u.Reverse == "" {
		return fmt.Sprintf("%s: %s", u.Write, u.What)
	}
	return fmt.Sprintf("%s: %s (%s)", u.Write, u.What, u.Reverse)
}

// WriteRows is what the store read of one write: every slot it moved, read off
// the store on both sides of the write, the events the write added, and
// whatever it moved that no slot names.
type WriteRows struct {
	Write string
	// Fields is every editable field whose value differs between the two sides.
	Fields []Move
	// Parents is every issue whose parent set differs between the two sides.
	Parents []Move
	// BlockedBy is every blocked-by edge present on one side only.
	BlockedBy []Move
	// Records is every event the write added.
	Records []model.IssueEvent
	// Outside is what the write moved that no slot names: an issue made or
	// removed, a non-field column, a comment, a related-to edge, a table.
	Outside []Unrevertable
}

// WriteDelta is one write an update document can put back whole: every slot
// it moved, each checked against the write's own record. Only Classify makes
// one, so a write with anything unrevertable in it cannot reach Revert.
// [LAW:parse-dont-validate]
type WriteDelta struct {
	write string
	moves []Move
}

// Write is the write's id.
func (d WriteDelta) Write() string { return d.write }

// Moves is every slot the write moved.
func (d WriteDelta) Moves() []Move { return slices.Clone(d.moves) }

// Classify decides what of one write an update document can put back. A field
// is revertable only when the write recorded exactly the move the store shows:
// a field that moved with no record of this write was written by something
// that is not an edit (a restore, a reconcile, a label verb), and a record the
// store disagrees with is a write a sync replay re-landed over a merged value.
// Putting back the record's value would erase whatever the store's value came
// from, so both are refused. [LAW:no-silent-failure]
func Classify(rows WriteRows) (WriteDelta, []Unrevertable) {
	refused := slices.Clone(rows.Outside)
	refuse := func(what, reverse string) {
		refused = append(refused, Unrevertable{Write: rows.Write, What: what, Reverse: reverse})
	}
	recorded := map[Slot]model.FieldChange{}
	for _, event := range rows.Records {
		if event.Action != "" {
			// Lifecycle and claims are their verbs', and claims are derived
			// from these events, so a revert of the row alone would leave the
			// lane held by an event it never undid.
			refuse(fmt.Sprintf("%s: %s", event.IssueID, actionWrite(event.Action)), "lifecycle moves only through its verbs")
			continue
		}
		for _, change := range event.Changes {
			recorded[Slot{Kind: FieldSlot, Issue: event.IssueID, Name: change.Field}] = change
		}
	}
	moved := map[Slot]Move{}
	for _, move := range rows.Fields {
		moved[move.Slot] = move
	}
	for _, slot := range sortedSlots(slices.Collect(maps.Keys(moved))) {
		move := moved[slot]
		change, ok := recorded[slot]
		switch {
		case !ok:
			refuse(fmt.Sprintf("%s moved with no field-change record of this write", slot), "")
		case FieldValue(change.From) != move.From || FieldValue(change.To) != move.To:
			refuse(fmt.Sprintf("%s: the write recorded %q → %q but the store moved %s → %s; a sync replay re-landed it over a merged value", slot, change.From, change.To, move.From.Display(FieldSlot), move.To.Display(FieldSlot)), "")
		}
	}
	for _, slot := range sortedSlots(slices.Collect(maps.Keys(recorded))) {
		if _, ok := moved[slot]; !ok {
			change := recorded[slot]
			refuse(fmt.Sprintf("%s: the write recorded %q → %q but the store did not move it; a sync replay re-landed it over a merged value", slot, change.From, change.To), "")
		}
	}
	for _, move := range rows.Parents {
		if len(move.From.parents()) > 1 {
			refuse(fmt.Sprintf("%s held %d parents before this write", move.Slot, len(move.From.parents())), "`lit doctor` repairs a child with two parents")
		}
	}
	if len(refused) > 0 {
		return WriteDelta{}, refused
	}
	moves := slices.Concat(rows.Fields, rows.Parents, rows.BlockedBy)
	slices.SortFunc(moves, func(a, b Move) int { return compareSlots(a.Slot, b.Slot) })
	return WriteDelta{write: rows.Write, moves: moves}, nil
}

// actionWrite names the write an event with an action records: a lifecycle
// action by the verb a caller types (lifecycle.ParseAction is the one gate to
// that set), and any other action, a creation among them, as recorded.
func actionWrite(action string) string {
	if parsed, err := lifecycle.ParseAction(action); err == nil {
		return fmt.Sprintf("a `lit %s` write", parsed.Verb())
	}
	return fmt.Sprintf("a %q event", action)
}

// CurrentState is the store as it stands, for exactly the slots the writes
// being reverted moved: each field's value, each child's parent set, and the
// whole blocked-by set of each issue an edge slot names, since a document
// states that set whole.
type CurrentState struct {
	Fields    map[Slot]SlotValue
	Parents   map[string][]string
	BlockedBy map[string][]string
}

func (c CurrentState) value(slot Slot) SlotValue {
	switch slot.Kind {
	case FieldSlot:
		return c.Fields[slot]
	case ParentSlot:
		return ParentsValue(c.Parents[slot.Issue])
	case BlockedBySlot:
		return EdgeValue(slices.Contains(c.BlockedBy[slot.Issue], slot.Name))
	}
	panic(fmt.Sprintf("storage: slot kind %d", slot.Kind))
}

// Conflict is a slot a later write moved again: the write being reverted left
// it at Left, and the store now holds Now.
type Conflict struct {
	Write     string
	Slot      Slot
	Left, Now SlotValue
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s: %s was left %s by this write and is %s now", c.Write, c.Slot, c.Left.Display(c.Slot.Kind), c.Now.Display(c.Slot.Kind))
}

// Reverted is what reverting writes onto the store as it stands comes to: the
// slots it puts back, and the update documents that put them back.
type Reverted struct {
	Moves     []Move
	Documents []BulkIssueSpec
}

// Revert folds writes, newest first, onto the store as it stands. Each slot a
// write moved must still hold the value the write left it at, or a later write
// moved it again and putting the old value back would erase that write; every
// such slot is a Conflict, and any conflict refuses the whole revert. A slot
// that ends where it stands now is left out, so reverting a write whose
// effect is already gone states nothing. [LAW:no-silent-failure]
func Revert(deltas []WriteDelta, now CurrentState) (Reverted, []Conflict) {
	state := map[Slot]SlotValue{}
	current := func(slot Slot) SlotValue {
		if value, ok := state[slot]; ok {
			return value
		}
		return now.value(slot)
	}
	var conflicts []Conflict
	ids := make([]string, len(deltas))
	for i, delta := range deltas {
		ids[i] = delta.write
		for _, move := range delta.moves {
			if held := current(move.Slot); held != move.To {
				conflicts = append(conflicts, Conflict{Write: delta.write, Slot: move.Slot, Left: move.To, Now: held})
			}
			state[move.Slot] = move.From
		}
	}
	if len(conflicts) > 0 {
		return Reverted{}, conflicts
	}
	var reverted Reverted
	for _, slot := range sortedSlots(slices.Collect(maps.Keys(state))) {
		if was := now.value(slot); was != state[slot] {
			reverted.Moves = append(reverted.Moves, Move{Slot: slot, From: was, To: state[slot]})
		}
	}
	reverted.Documents = documentsFor(reverted.Moves, now, "undo "+strings.Join(ids, ", "))
	return reverted, nil
}

// documentsFor states moves as one update document per issue. A reason is
// stated only on a document that moves a field, since it annotates a field
// change and the bulk gate refuses one beside nothing that moves.
func documentsFor(moves []Move, now CurrentState, reason string) []BulkIssueSpec {
	docs := map[string]*BulkIssueSpec{}
	doc := func(issue string) *BulkIssueSpec {
		if docs[issue] == nil {
			docs[issue] = &BulkIssueSpec{ID: issue}
		}
		return docs[issue]
	}
	blockedBy := map[string]map[string]bool{}
	for _, move := range moves {
		switch move.Slot.Kind {
		case FieldSlot:
			d := doc(move.Slot.Issue)
			if err := RestoreIssueField(d, move.Slot.Name, move.To.text); err != nil {
				// A move only reaches here through Classify, which took the
				// field's name and both values from the field's own row.
				panic(fmt.Sprintf("storage: restore a classified field: %v", err))
			}
			d.Reason = reason
		case ParentSlot:
			// Classify refused every write whose from-set held two parents,
			// so a set put back holds one parent or none.
			parent := strings.Join(move.To.parents(), "")
			doc(move.Slot.Issue).Parent = &parent
		case BlockedBySlot:
			if blockedBy[move.Slot.Issue] == nil {
				blockedBy[move.Slot.Issue] = map[string]bool{}
			}
			blockedBy[move.Slot.Issue][move.Slot.Name] = move.To == EdgeValue(true)
		}
	}
	for issue, edges := range blockedBy {
		set := map[string]bool{}
		for _, blocker := range now.BlockedBy[issue] {
			set[blocker] = true
		}
		maps.Copy(set, edges)
		var stated []string
		for blocker, present := range set {
			if present {
				stated = append(stated, blocker)
			}
		}
		slices.Sort(stated)
		if stated == nil {
			stated = []string{}
		}
		doc(issue).BlockedBy = &stated
	}
	out := make([]BulkIssueSpec, 0, len(docs))
	for _, issue := range slices.Sorted(maps.Keys(docs)) {
		out = append(out, *docs[issue])
	}
	return out
}

func compareSlots(a, b Slot) int {
	return cmp.Or(cmp.Compare(a.Issue, b.Issue), cmp.Compare(a.Kind, b.Kind), cmp.Compare(fieldOrder(a), fieldOrder(b)), cmp.Compare(a.Name, b.Name))
}

// fieldOrder places a field slot where the table places its field, so moves
// read in the order history reports them.
func fieldOrder(s Slot) int {
	if s.Kind != FieldSlot {
		return 0
	}
	return slices.Index(IssueFieldNames(), s.Name)
}

func sortedSlots(slots []Slot) []Slot {
	slices.SortFunc(slots, compareSlots)
	return slots
}

// Reverter reads a store's writes back out of its history and reverts chosen
// ones. It is a capability, not a duty every engine owes: an engine with no
// history of commits has no write to name.
type Reverter interface {
	// ListWrites is the newest last writes, newest first, each with what it
	// moved and what of it no document can put back.
	ListWrites(ctx context.Context, last int) ([]WriteSummary, error)
	// RevertWrites puts back what the named writes moved, as one bulk edit,
	// or refuses and writes nothing.
	RevertWrites(ctx context.Context, ids []string, actor string) (RevertResult, error)
}

// WriteSummary is one write as a listing shows it.
type WriteSummary struct {
	ID      string
	At      time.Time
	Message string
	// Moves is every slot the write moved.
	Moves []Move
	// Unrevertable is what of the write no document can put back; a write
	// with none can be reverted.
	Unrevertable []Unrevertable
}

// RevertResult is what an undo did.
type RevertResult struct {
	// Writes is the writes reverted, newest first.
	Writes []string
	// Moves is every slot put back, from the value it held to the value it
	// holds now.
	Moves []Move
	// Applied is what the bulk edit that put them back wrote.
	Applied BulkApplyResult
	// Landed is the commits that bulk edit landed, oldest first: the writes
	// that undo this undo.
	Landed []string
}

// RevertRefusedError refuses an undo because a write it names moved something
// no update document can put back. Nothing is written. It refuses the command
// as issued: naming other writes, or reversing the rest with the verbs it
// names, is what clears it. [LAW:no-silent-failure]
type RevertRefusedError struct {
	Unrevertable []Unrevertable
}

func (e RevertRefusedError) Error() string {
	lines := make([]string, len(e.Unrevertable))
	for i, u := range e.Unrevertable {
		lines[i] = "  " + u.String()
	}
	return "undo refused, nothing written; no update document can put these back:\n" + strings.Join(lines, "\n")
}

// RefusesCommand marks the refusal as one of the command as issued.
func (RevertRefusedError) RefusesCommand() {}

// RevertConflictError refuses an undo because a later write moved again
// something the undo would put back, so putting it back would erase that
// write. Nothing is written.
type RevertConflictError struct {
	Conflicts []Conflict
}

func (e RevertConflictError) Error() string {
	lines := make([]string, len(e.Conflicts))
	for i, c := range e.Conflicts {
		lines[i] = "  " + c.String()
	}
	return "undo refused, nothing written; a later write moved these again:\n" + strings.Join(lines, "\n")
}

// RevertHeldError answers an undo whose writes' effects are already gone:
// every slot they moved stands where it was before them, so there is nothing
// to put back and nothing was written. The state the undo asked for holds.
type RevertHeldError struct {
	Writes []string
}

func (e RevertHeldError) Error() string {
	return fmt.Sprintf("nothing to put back: what %s moved already stands where it was before", strings.Join(e.Writes, ", "))
}

// Satisfied reports that the state the undo asked for already holds.
func (RevertHeldError) Satisfied() bool { return true }

var _ model.StateRequestError = RevertHeldError{}
