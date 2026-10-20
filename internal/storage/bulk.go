package storage

import (
	"errors"
	"fmt"
	"slices"
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
	Parent    *string  `yaml:"parent,omitempty"`
	BlockedBy []string `yaml:"blocked_by,omitempty"`
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
// which a landed file leaves empty; Updated lists the real IDs of
// every updated issue, in the order they were applied; Ranked is the RankSet
// the file's `rank: top` documents made, with no resolutions when the file
// states no rank. A refused BulkApply returns its AfterRollback beside the
// error, so the caller reads one account of the batch whether it landed or
// not. [LAW:nothing-unseen]
type BulkApplyResult struct {
	Created         []IDMapping
	Reparented      []string
	ParentHeld      []string
	ReparentsUndone []string
	Updated         []string
	Ranked          RankSetResult
}

// AfterRollback is what stays written once a refused batch's creates are
// rolled back, given leaked, the creates the rollback could not delete: those
// creates, the reparents its undo could not put back, the updates applied
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
	return fmt.Sprintf("kept %d reparents [%s], %d updates [%s] and the order of %d issues [%s]", len(kept.Reparented), strings.Join(kept.Reparented, ","), len(kept.Updated), strings.Join(kept.Updated, ","), len(ranked), strings.Join(ranked, ","))
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
// Reparents are the update documents that state a parent, in document order.
// An engine writes them as one step, whole or not at all, through the relation
// write `lit parent set` and `lit parent clear` use, so a reparent obeys the
// one hierarchy rule. [LAW:single-enforcer] It writes them after the creates,
// which a reparent may name as its parent, and before the creates' blocked_by
// edges, so each edge is judged on the hierarchy the file ends with.
type BulkFile struct {
	Creates   []FileCreate
	Reparents []FileReparent
	Updates   []BulkUpdate
	Ranked    []FileRef
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

// ParentWriter is the hierarchy write an engine runs a file's reparents
// through, inside the one step that makes them whole or not at all. Clear and
// Attach are the writes `lit parent clear` and `lit parent set` make, each
// with the rules it carries; Detach drops a child's parent edges with no rule,
// because an attach follows it in the same step. Parents and Restore read and
// write a child's parent edges as stored, judging nothing, so a refused batch
// can put back the hierarchy its reparents replaced.
type ParentWriter interface {
	// Holds reports whether childID's parents are exactly parentID.
	Holds(childID, parentID string) (bool, error)
	// Clear detaches childID as `lit parent clear` does, answering a child
	// with no parent with RelationAbsentError.
	Clear(childID string) error
	Detach(childID string) error
	Attach(childID, parentID string) error
	// Parents lists the parent edges childID holds.
	Parents(childID string) ([]model.Relation, error)
	// Restore makes childID's parent edges exactly edges.
	Restore(childID string, edges []model.Relation) error
}

// ReparentStep is a file's written reparents, sorted by what each found:
// Moved, the issues whose parent changed, and Held, those whose parent already
// was the one the file states, each in document order. It keeps the parent
// edges each moved issue held before, which Undo writes back.
type ReparentStep struct {
	Moved, Held []string
	prior       map[string][]model.Relation
}

// Undo puts every moved issue back under the parents it held before the step,
// as stored. A batch refused after its reparents runs it before it rolls back
// its creates, so no issue is left under a create the rollback deletes, and
// none loses the parent it had. [LAW:no-silent-failure]
func (s ReparentStep) Undo(w ParentWriter) error {
	for _, id := range s.Moved {
		if err := w.Restore(id, s.prior[id]); err != nil {
			return fmt.Errorf("bulk: restore the parents of %q: %w", id, err)
		}
	}
	return nil
}

// WriteReparents writes the file's reparents through w and returns the step
// it wrote. Each parent is resolved first; an empty one clears.
//
// The file is judged on the hierarchy it ends with, never on its document
// order: every moved child is detached before any is attached, so each
// hierarchy in between is part of the final one, and Attach's cycle rule
// refuses a file exactly when the hierarchy it ends with holds a cycle.
// Swapping an epic with the epic above it applies in either order.
// [LAW:no-ambient-temporal-coupling] [LAW:single-enforcer]
//
// The first refusal ends the step, naming the document it refused.
func (f BulkFile) WriteReparents(resolve func(ref string) string, w ParentWriter) (ReparentStep, error) {
	refuse := func(r FileReparent, err error) (ReparentStep, error) {
		return ReparentStep{}, fmt.Errorf("bulk: parent of %q -> %q: %w", r.ID, r.Parent, err)
	}
	isHeld := make([]bool, len(f.Reparents))
	for i, r := range f.Reparents {
		if parent := resolve(r.Parent); parent != "" {
			held, err := w.Holds(r.ID, parent)
			if err != nil {
				return refuse(r, err)
			}
			isHeld[i] = held
		}
	}
	step := ReparentStep{prior: make(map[string][]model.Relation, len(f.Reparents))}
	for i, r := range f.Reparents {
		if isHeld[i] {
			continue
		}
		prior, err := w.Parents(r.ID)
		if err != nil {
			return refuse(r, err)
		}
		step.prior[r.ID] = prior
		if r.Parent != "" {
			if err := w.Detach(r.ID); err != nil {
				return refuse(r, err)
			}
			continue
		}
		err = w.Clear(r.ID)
		if absent := (RelationAbsentError{}); errors.As(err, &absent) {
			isHeld[i] = true
		} else if err != nil {
			return refuse(r, err)
		}
	}
	for i, r := range f.Reparents {
		if r.Parent != "" && !isHeld[i] {
			if err := w.Attach(r.ID, resolve(r.Parent)); err != nil {
				return refuse(r, err)
			}
		}
	}
	for i, r := range f.Reparents {
		if isHeld[i] {
			step.Held = append(step.Held, r.ID)
		} else {
			step.Moved = append(step.Moved, r.ID)
		}
	}
	return step, nil
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
