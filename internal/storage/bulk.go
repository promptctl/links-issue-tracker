package storage

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// BulkIssueSpec is a single YAML document in a bulk-input file. ID is the
// selector: present, it names an existing issue and the document is an
// update patch of only the fields it sets (the same fields UpdateIssueInput
// exposes, one YAML key per field). Absent, the document creates a new
// issue and behaves like ImportTreeSpec's flat form — LocalID, Parent, and
// BlockedBy wire the new issue against siblings created in the same file (by
// LocalID) or against pre-existing issues (by real ID); title/topic/type are
// required as they are for any create. [LAW:types-are-the-program] Pointer
// fields are exactly the update-patch fields: nil means "leave unchanged,"
// a set pointer means "write this value" — the same distinction
// UpdateIssueInput's pointers carry, so a create-doc's omitted optional
// field and an update-doc's omitted field defer to the same "unspecified"
// representation instead of each needing its own convention.
//
// The struct tags are contract, not engine detail: the file format is what a
// user authored and what any engine must keep reading identically.
type BulkIssueSpec struct {
	LocalID     string    `yaml:"local_id,omitempty"`
	ID          string    `yaml:"id,omitempty"`
	Title       *string   `yaml:"title,omitempty"`
	Description *string   `yaml:"description,omitempty"`
	Prompt      *string   `yaml:"prompt,omitempty"`
	Plan        *string   `yaml:"plan,omitempty"`
	IssueType   *string   `yaml:"type,omitempty"`
	Topic       *string   `yaml:"topic,omitempty"`
	Priority    *int      `yaml:"priority,omitempty"`
	Assignee    *string   `yaml:"assignee,omitempty"`
	Labels      *[]string `yaml:"labels,omitempty"`
	Lane        *string   `yaml:"lane,omitempty"`
	// Parent on a create names the new issue's parent. On an update it
	// reparents the issue, and the empty string clears its parent: nil is
	// "not stated", so the two cannot be confused.
	Parent *string `yaml:"parent,omitempty"`
	// BlockedBy on a create names the issues the new one waits on. On an
	// update it is the issue's whole blocked-by set, replacing the one it
	// holds as labels replace, and `blocked_by: []` empties it: nil is "not
	// stated", so the two cannot be confused.
	BlockedBy *[]string `yaml:"blocked_by,omitempty"`
	// Rank places the document's issue in the file's ordering. Its one
	// value is "top"; the gate refuses any other.
	Rank *string `yaml:"rank,omitempty"`
	// Reason is optional free text recorded on an update's field-change
	// event; it does not apply to a create (there is no prior state to
	// annotate a change against).
	Reason string `yaml:"reason,omitempty"`
}

// IDMapping records one created issue: Ref is the name the input file used
// for it, ID is the real issue ID it was created under.
//
// [LAW:types-are-the-program] A result carries these in the engine's creation
// order — an ordered slice, not a map — so the same input file reports the
// same sequence on every run instead of Go's randomized map iteration.
type IDMapping struct {
	Ref string
	ID  string
}

// NewIDMapping applies the one ref rule: a create is nameable by the LocalID
// it chose, or by its own new real ID when the file gave it none — so every
// create is in the report either way. [LAW:one-source-of-truth]
func NewIDMapping(localID, realID string) IDMapping {
	if localID == "" {
		return IDMapping{Ref: realID, ID: realID}
	}
	return IDMapping{Ref: localID, ID: realID}
}

// BulkApplyResult reports what a BulkApply left written. Created lists
// every created issue in the order it was created; Reparented lists the issues
// whose parent the file changed, and ParentHeld those whose parent already
// was what the file states, each in file order; ReparentsUndone lists the
// issues a refusal put back under the parents they held before the file,
// which a landed file leaves empty; BlockedBySet, BlockedByHeld and
// BlockedByUndone say the same of the update documents' blocked_by sets;
// EdgesAdded and EdgesRemoved count every edge the file wrote, a create's
// blocked_by included; Updated lists the real IDs of the issues whose fields an
// update document moved, and FieldsHeld those whose fields already were what
// the document states, each in the order they were applied; Ranked is the RankSet
// the file's `rank: top` documents made, with no resolutions when the file
// states no rank. A refused BulkApply returns its AfterRollback beside the
// error, so the caller reads one account of the batch whether it landed or
// not. [LAW:nothing-unseen]
type BulkApplyResult struct {
	Created         []IDMapping
	Reparented      []string
	ParentHeld      []string
	ReparentsUndone []string
	BlockedBySet    []string
	BlockedByHeld   []string
	BlockedByUndone []string
	EdgesAdded      int
	EdgesRemoved    int
	Updated         []string
	FieldsHeld      []string
	Ranked          RankSetResult
}

// RecordUpdate is the account of one applied update document: updated when its
// patch moved a field, held when the issue already held every field it states.
// [LAW:one-source-of-truth] both engines sort an update by the change rows
// their own write made, here.
func (r *BulkApplyResult) RecordUpdate(applied Applied) {
	if len(applied.Fields) == 0 {
		r.FieldsHeld = append(r.FieldsHeld, applied.Issue.ID)
		return
	}
	r.Updated = append(r.Updated, applied.Issue.ID)
}

// RecordEdges is the account of a written edge step: the update documents'
// sets, by type, and every edge it wrote. [LAW:one-source-of-truth]
func (r *BulkApplyResult) RecordEdges(step EdgeStep) {
	r.Reparented, r.ParentHeld = updatedIDs(step.Changed, model.RelParentChild), updatedIDs(step.Held, model.RelParentChild)
	r.BlockedBySet, r.BlockedByHeld = updatedIDs(step.Changed, model.RelBlockedBy), updatedIDs(step.Held, model.RelBlockedBy)
	r.EdgesAdded, r.EdgesRemoved = step.Added, step.Removed
}

// EdgesUndone is the account once a refusal's Undo put the step's sets back:
// none stays changed, each is reported undone, and no edge it wrote stays
// written.
func (r *BulkApplyResult) EdgesUndone() {
	r.ReparentsUndone, r.Reparented = r.Reparented, nil
	r.BlockedByUndone, r.BlockedBySet = r.BlockedBySet, nil
	r.EdgesAdded, r.EdgesRemoved = 0, 0
}

// AfterRollback is what stays written once a refused batch's creates are
// rolled back, given leaked, the creates the rollback could not delete: those
// creates, the reparents and blocked_by sets its undo could not put back, the
// updates applied
// before the refusal, and the order the file's `rank: top` documents wrote,
// among the issues that outlive the rollback. [LAW:one-source-of-truth]
func (r BulkApplyResult) AfterRollback(leaked []string) BulkApplyResult {
	rolledBack := make(map[string]bool, len(r.Created))
	for _, m := range r.Created {
		rolledBack[m.ID] = !slices.Contains(leaked, m.ID)
	}
	kept := r
	kept.Created = nil
	for _, m := range r.Created {
		if !rolledBack[m.ID] {
			kept.Created = append(kept.Created, m)
		}
	}
	kept.Ranked.Resolutions = nil
	for _, res := range r.Ranked.Resolutions {
		if !rolledBack[res.RankedID] {
			kept.Ranked.Resolutions = append(kept.Ranked.Resolutions, res)
		}
	}
	return kept
}

// Kept is AfterRollback in words, the account a refused batch's error owes its
// caller beside the creates the rollback leaked, which the error names. Zero
// of any is said, not omitted. [LAW:no-silent-failure]
func (r BulkApplyResult) Kept(leaked []string) string {
	kept := r.AfterRollback(leaked)
	ranked := make([]string, len(kept.Ranked.Resolutions))
	for i, res := range kept.Ranked.Resolutions {
		ranked[i] = res.RankedID
	}
	return fmt.Sprintf("kept %d reparents [%s], %d blocked_by sets [%s], %d updates [%s] and the order of %d issues [%s]", len(kept.Reparented), strings.Join(kept.Reparented, ","), len(kept.BlockedBySet), strings.Join(kept.BlockedBySet, ","), len(kept.Updated), strings.Join(kept.Updated, ","), len(ranked), strings.Join(ranked, ","))
}

// BulkFile is a bulk file the gate admitted, parsed: the issues it creates,
// in the order they are written, and the patch each update document applies.
// An engine writes exactly this, so a document means the same write whichever
// engine opens the file, and no engine parses a document itself.
// [LAW:parse-dont-validate] [LAW:one-source-of-truth]
//
// Ranked is the file's ordering: every document that states `rank: top`, in
// document order. An engine hands it to its RankSet whole, so a file orders
// issues exactly as `lit rank set` does — one transaction, keys permuted and
// none minted, and an order that has already landed writes nothing.
// [LAW:single-enforcer]
//
// Reparents are the update documents that state a parent, and BlockedBy those
// that state blocked_by, each in document order. An engine writes them, with
// the creates' blocked_by, as one step after the creates, which any of them
// may name: WriteEdges, whole or not at all, through the relation write every
// edge goes through, so an edge a file writes obeys the one rule its type
// carries. [LAW:single-enforcer]
type BulkFile struct {
	Creates   []FileCreate
	Reparents []FileReparent
	BlockedBy []FileBlockedBy
	Updates   []BulkUpdate
	Ranked    []FileRef
}

// FileBlockedBy is one update document's blocked_by, parsed: the issue whose
// blocked-by set it states and that whole set, each entry a local_id in the
// file or a real id, which only the write can resolve. An empty set is no
// blockers.
type FileBlockedBy struct {
	ID        string
	BlockedBy []string
}

// FileReparent is one update document's parent, parsed: the issue it moves and
// the parent it moves it under, a local_id in the file or a real id, which only
// the write can resolve. An empty Parent is no parent, as a create's is.
type FileReparent struct {
	ID     string
	Parent string
}

// FileRef names a document's issue before the file is written: an update
// document's id, or a create document's position in the file, whose id only
// the write mints.
type FileRef struct {
	id    string
	index int
}

// String is the name a refusal calls the document by: an update by its id, a
// create by its position.
func (r FileRef) String() string {
	if r.id != "" {
		return strconv.Quote(r.id)
	}
	return fmt.Sprintf("doc %d", r.index)
}

// EdgeSet states one issue's whole set of outgoing edges of one relation
// type: a parent is a set of zero or one, a blocked_by list a set of any size.
// Doc is the document that states the set.
type EdgeSet struct {
	Doc     FileRef
	ID      string
	Type    model.RelationType
	Targets []EdgeTarget
}

// EdgeTarget is one target an edge set states: Ref as the file wrote it, a
// local_id in the file or a real id, and ID, the real id it resolves to. A
// refusal names the Ref, the one the author can find in the file.
type EdgeTarget struct {
	Ref, ID string
}

// ResolveRef resolves a reference a file states against minted, the ids its
// creates were minted under by local_id: a reference that matches no create
// passes through unchanged as a real, pre-existing id, for the write that
// receives it to judge. [LAW:dataflow-not-control-flow] one lookup with a
// fallback, not a branch on "is this local".
func ResolveRef(ref string, minted map[string]string) string {
	if id, ok := minted[ref]; ok {
		return id
	}
	return ref
}

// EdgeWriter is the relation write an engine runs a file's edge sets through,
// inside the one step that makes them whole or not at all. Add is the write
// every edge of its type goes through, `lit dep add` and `lit parent set`
// alike, with the rules it carries. Edges, Remove and Restore read and write
// edges as stored, judging nothing: a move removes a leaf's parent before it
// attaches the new one, so a rule judged at the removal would refuse a file
// whose final hierarchy obeys it. A refused batch restores what a step
// replaced through Restore. [LAW:single-enforcer]
type EdgeWriter interface {
	// Edges lists the edges of relType id holds, and refuses an id that names
	// no issue, so a set stated on a missing issue is never read as held.
	Edges(id string, relType model.RelationType) ([]model.Relation, error)
	Add(id, target string, relType model.RelationType) error
	Remove(edge model.Relation) error
	// Restore makes id's edges of relType exactly edges.
	Restore(id string, relType model.RelationType, edges []model.Relation) error
}

// EdgeStep is a file's written edge sets, sorted by what each found: Changed,
// the sets the file changed, and Held, those already exactly as stated, each in
// write order. Added and Removed count the edges it wrote. It keeps the edges
// each changed set held before, which Undo writes back.
type EdgeStep struct {
	Changed, Held  []EdgeSet
	Added, Removed int
	prior          [][]model.Relation
}

// Undo gives every changed set back the edges it held before the step, as
// stored. A batch refused after the step runs it before it rolls back its
// creates, so no issue is left under or behind a create the rollback deletes,
// and none loses an edge it had. [LAW:no-silent-failure]
func (s EdgeStep) Undo(w EdgeWriter) error {
	for i, set := range s.Changed {
		if err := w.Restore(set.ID, set.Type, s.prior[i]); err != nil {
			return fmt.Errorf("bulk: restore the %s of %s: %w", fileKey(set.Type), set.Doc, err)
		}
	}
	return nil
}

// WriteEdges writes every edge set the file states through w and returns the
// step it wrote, given created: the id the write minted for each of f.Creates,
// in that order.
//
// The file is judged on the graph it ends with, never on its document order:
// every edge any set drops is removed before any set's edge is added, so the
// cycle rules refuse a file exactly when the graph it ends with holds a
// cycle, and swapping an epic with the epic above it, or one blocker for
// another, applies in either order. The sets are added hierarchy first, so the
// rule that keeps blocked_by out of one epic reads the hierarchy the file ends
// with. An edge an issue already holds is neither removed nor added.
// [LAW:no-ambient-temporal-coupling] [LAW:single-enforcer]
//
// The first refusal ends the step, naming the document it refused.
func (f BulkFile) WriteEdges(created []string, w EdgeWriter) (EdgeStep, error) {
	refuse := func(set EdgeSet, ref string, err error) (EdgeStep, error) {
		return EdgeStep{}, fmt.Errorf("bulk: %s of %s -> %q: %w", fileKey(set.Type), set.Doc, ref, err)
	}
	var step EdgeStep
	sets := f.edgeSets(created)
	adds := make([][]EdgeTarget, len(sets))
	for i, set := range sets {
		prior, err := w.Edges(set.ID, set.Type)
		if err != nil {
			return EdgeStep{}, fmt.Errorf("bulk: %s of %s: %w", fileKey(set.Type), set.Doc, err)
		}
		held := make(map[string]bool, len(prior))
		removed := 0
		for _, edge := range prior {
			held[edge.DstID] = true
			if !slices.ContainsFunc(set.Targets, func(t EdgeTarget) bool { return t.ID == edge.DstID }) {
				if err := w.Remove(edge); err != nil {
					return refuse(set, edge.DstID, err)
				}
				removed++
			}
		}
		for _, target := range set.Targets {
			if !held[target.ID] {
				adds[i] = append(adds[i], target)
			}
		}
		if removed+len(adds[i]) == 0 {
			step.Held = append(step.Held, set)
			continue
		}
		step.Changed = append(step.Changed, set)
		step.prior = append(step.prior, prior)
		step.Removed += removed
	}
	for i, set := range sets {
		for _, target := range adds[i] {
			if err := w.Add(set.ID, target.ID, set.Type); err != nil {
				return refuse(set, target.Ref, err)
			}
			step.Added++
		}
	}
	return step, nil
}

// edgeSets lists every edge set the file states, given the ids its creates
// were minted under, in the order they are written: the update documents'
// parents, then the creates' blocked_by, then the update documents'
// blocked_by. A create's parent is not among them; the create writes it.
func (f BulkFile) edgeSets(created []string) []EdgeSet {
	minted := make(map[string]string, len(f.Creates))
	for pos, c := range f.Creates {
		if c.LocalID != "" {
			minted[c.LocalID] = created[pos]
		}
	}
	targets := func(refs []string) []EdgeTarget {
		resolved := make([]EdgeTarget, len(refs))
		for i, ref := range refs {
			resolved[i] = EdgeTarget{Ref: ref, ID: ResolveRef(ref, minted)}
		}
		return resolved
	}
	sets := make([]EdgeSet, 0, len(f.Reparents)+len(f.Creates)+len(f.BlockedBy))
	for _, r := range f.Reparents {
		set := EdgeSet{Doc: FileRef{id: r.ID}, ID: r.ID, Type: model.RelParentChild}
		if r.Parent != "" {
			set.Targets = targets([]string{r.Parent})
		}
		sets = append(sets, set)
	}
	for pos, c := range f.Creates {
		if len(c.BlockedBy) > 0 {
			sets = append(sets, EdgeSet{Doc: FileRef{index: c.Index}, ID: created[pos], Type: model.RelBlockedBy, Targets: targets(c.BlockedBy)})
		}
	}
	for _, b := range f.BlockedBy {
		sets = append(sets, EdgeSet{Doc: FileRef{id: b.ID}, ID: b.ID, Type: model.RelBlockedBy, Targets: targets(b.BlockedBy)})
	}
	return sets
}

// fileKey is the bulk file key that states edges of relType.
func fileKey(relType model.RelationType) string {
	if relType == model.RelParentChild {
		return "parent"
	}
	return "blocked_by"
}

// updatedIDs lists the ids of the sets of relType that update documents
// state. The set a create states is part of its create, reported with it.
func updatedIDs(sets []EdgeSet, relType model.RelationType) []string {
	var ids []string
	for _, set := range sets {
		if set.Type == relType && set.Doc.id != "" {
			ids = append(ids, set.ID)
		}
	}
	return ids
}

// RankedIDs is the file's ordering as the issue ids an engine hands its
// RankSet, given created: the id the write minted for each of f.Creates, in
// that order. It is empty for a file that states no rank.
func (f BulkFile) RankedIDs(created []string) []string {
	byIndex := make(map[int]string, len(f.Creates))
	for pos, create := range f.Creates {
		byIndex[create.Index] = created[pos]
	}
	ids := make([]string, len(f.Ranked))
	for i, ref := range f.Ranked {
		ids[i] = ref.id
		if ref.id == "" {
			ids[i] = byIndex[ref.index]
		}
	}
	return ids
}

// BulkUpdate is one update document, parsed: the issue it names and the patch
// of the fields it states.
type BulkUpdate struct {
	ID    string
	Patch UpdateIssueInput
}

// FileCreate is one create document of an import file, parsed. Index is its
// position in the file, which is how a bulk refusal names it; Parent and
// BlockedBy are references, each a local_id in the file or a real id, which
// only the write can resolve.
type FileCreate struct {
	Index     int
	LocalID   string
	Parent    string
	BlockedBy []string
	issue     CreateIssueInput
}

// Input is the create this document writes, given the three facts only the
// write knows: the real id its parent reference resolved to, the prefix of the
// workspace it lands in, and who is importing it. Taking them as arguments is
// what keeps a write from landing with its parent unresolved or its author
// unnamed. [LAW:types-are-the-program]
func (c FileCreate) Input(parentID, prefix, actor string) CreateIssueInput {
	in := c.issue
	in.ParentID = parentID
	in.Prefix = prefix
	in.Actor = actor
	return in
}

// ImportTreeSpec is a single record in a declarative tree-import file. LocalID
// is opaque — it's used inside the spec to wire Parent and BlockedBy refs and
// is replaced with the generated lit issue ID at import time.
type ImportTreeSpec struct {
	LocalID     string   `json:"local_id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
	Plan        string   `json:"plan,omitempty"`
	IssueType   string   `json:"type"`
	Topic       string   `json:"topic"`
	Priority    int      `json:"priority"`
	Assignee    string   `json:"assignee,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	BlockedBy   []string `json:"blocked_by,omitempty"`
}

// ImportTreeResult reports the local-ID → real-issue-ID mapping produced by a
// successful import, in the order the issues were created.
type ImportTreeResult struct {
	Created []IDMapping
}
