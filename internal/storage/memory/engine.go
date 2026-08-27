package memory

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Engine satisfies the whole storage contract and nothing beside it. The
// assertion is the claim: a contract method added or a signature moved stops
// this engine compiling, rather than surfacing as a failure in whichever test
// run happened to exercise it. [LAW:types-are-the-program]
var _ storage.Store = (*Engine)(nil)

// Engine is one in-memory store. It owns every byte of its state and hands
// none of it out: each read composes a fresh model value, so a caller holding
// an issue it read earlier cannot reach back through it and mutate the
// engine. [LAW:no-shared-mutable-globals]
//
// The mutex makes the engine safe to share the way the Dolt store is safe to
// share, and the discipline that keeps it honest is structural: every exported
// method locks and immediately delegates to an unexported one, and no
// unexported method ever locks. That is what lets BulkApply drive CreateIssue
// and Apply for a whole batch under one hold without deadlocking on itself.
type Engine struct {
	mu sync.Mutex

	// workspaceID scopes the attribution stamp. It is taken at construction
	// because a stream token with no workspace to scope it is a half-fact
	// model.NewAttribution refuses to carry.
	workspaceID string
	attribution model.Attribution

	issues map[string]*record

	// order is THE total rank order, top first. Issue.Rank is rendered from a
	// position in this slice at read time and stored nowhere, so a rank value
	// that contradicts the order it claims to encode is unrepresentable —
	// which is also why nothing here needs the inversion repair the Dolt
	// engine offers as a capability. [LAW:one-source-of-truth]
	order []string

	// The three edge tables and the history, each in the order it was
	// written. Insertion order is oldest-first by construction, so no read
	// re-derives "when did this happen" from a timestamp.
	relations []model.Relation
	comments  []model.Comment
	events    []model.IssueEvent

	// labels are keyed by issue and kept sorted by name, because the sorted
	// set is what every label read returns and re-sorting on each read would
	// be deriving the same fact twice.
	labels map[string][]model.Label
}

// record is one stored issue: what an engine must keep to answer with a
// model.Issue, and nothing more.
//
// Rank is absent on purpose. Position lives in Engine.order, and a rank string
// beside it would be a second representation of one fact — the exact
// divergence the Dolt engine's FixRankInversions exists to repair.
// [LAW:one-source-of-truth]
type record struct {
	id          string
	title       string
	description string
	prompt      string
	issueType   model.IssueType
	topic       string
	assignee    string
	lane        string
	priority    model.Priority
	createdAt   time.Time
	updatedAt   time.Time

	// status is the leaf status view. A container's state derives from its
	// children, so its view stays zero and hydration never reads it — the same
	// place Dolt writes a NULL status column.
	status    model.StatusView
	retention model.Retention
}

// createdBy is the author every structural row this engine writes on a
// caller's behalf carries: the parent edge and initial labels a create wires,
// and the create event itself. It matches the Dolt engine's spelling because
// it lands in exported history a user reads.
const createdBy = "links"

// New mints an empty engine whose work is attributed under workspaceID.
//
// The workspace id is required rather than defaulted: attribution is a
// complete pair or nothing, so an engine built without one could only ever
// record unattributed events, and it would do so silently — a store that
// looks like it is stamping and is not. [LAW:parse-dont-validate] The check is
// here, at the one place an Engine comes into existence, so no method below
// asks again.
func New(workspaceID string) (*Engine, error) {
	id := strings.TrimSpace(workspaceID)
	if id == "" {
		return nil, errors.New("workspace id is required")
	}
	return &Engine{
		workspaceID: id,
		issues:      map[string]*record{},
		labels:      map[string][]model.Label{},
	}, nil
}

// AttributeTo names the checkout whose work this engine records from here on.
// An empty token leaves it unattributed rather than half-attributed, which is
// model.NewAttribution's contract and why this takes no presence flag.
func (e *Engine) AttributeTo(streamToken string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attribution = model.NewAttribution(streamToken, e.workspaceID)
}

// Close releases what the engine holds, which is nothing: its state is Go
// memory the garbage collector owns. It is in the contract because a caller
// must be able to hand any engine back without knowing what it was.
func (e *Engine) Close() error { return nil }

// --- state access ---------------------------------------------------------

// mustRecord returns the stored issue or the contract's absence error. It is
// the one place "you named an issue that isn't here" is decided, so no caller
// below re-checks the map. [LAW:single-enforcer]
func (e *Engine) mustRecord(id string) (*record, error) {
	rec, ok := e.issues[id]
	if !ok {
		return nil, storage.NotFoundError{Entity: "issue", ID: id}
	}
	return rec, nil
}

// positions renders the rank order as an id → index lookup.
//
// It is derived on every read rather than maintained beside the slice: a
// cached index is a second representation of the order that every mutation
// site would have to remember to keep true, and the engines this contract is
// carving a seam for exist because that kind of bookkeeping went wrong.
// [LAW:one-source-of-truth]
func (e *Engine) positions() map[string]int {
	pos := make(map[string]int, len(e.order))
	for i, id := range e.order {
		pos[id] = i
	}
	return pos
}

// rankAt renders a position as the issue's Rank. Any encoding whose ascending
// string order matches the engine's order satisfies the contract — the suite
// observes rank only through the order of an unsorted listing — and a
// zero-padded position is the one encoding that cannot drift from the order it
// describes. The width is fixed so the comparison stays lexicographic.
func rankAt(index int) string { return fmt.Sprintf("%09d", index) }

// --- hydration ------------------------------------------------------------

// hydrate composes the model.Issue a caller sees from a stored record. Every
// derived fact — rank, labels, a container's state — is computed here and
// stored nowhere, so a read is the record plus its derivations and never a
// snapshot that could have gone stale.
func (e *Engine) hydrate(rec *record, pos map[string]int) (model.Issue, error) {
	issue := model.Issue{
		ID:          rec.id,
		Title:       rec.title,
		Description: rec.description,
		Prompt:      rec.prompt,
		Priority:    rec.priority,
		IssueType:   rec.issueType,
		Topic:       rec.topic,
		Assignee:    rec.assignee,
		Rank:        rankAt(pos[rec.id]),
		Lane:        rec.lane,
		Labels:      e.labelNames(rec.id),
		CreatedAt:   rec.createdAt,
		UpdatedAt:   rec.updatedAt,
	}
	issue.SetRetention(rec.retention)
	children, err := e.lifecycleChildren(rec, pos)
	if err != nil {
		return model.Issue{}, err
	}
	return model.HydrateRow(issue, rec.status, children)
}

// hydrateAll composes the issues for a list of records, preserving input
// order.
func (e *Engine) hydrateAll(recs []*record, pos map[string]int) ([]model.Issue, error) {
	out := make([]model.Issue, 0, len(recs))
	for _, rec := range recs {
		issue, err := e.hydrate(rec, pos)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	return out, nil
}

// lifecycleChildren returns the children whose lifecycles compose a
// container's state, rank-ordered. A leaf has none — its state is its own
// primitive — so it always returns an empty set rather than a case that skips
// the call. [LAW:dataflow-not-control-flow]
func (e *Engine) lifecycleChildren(rec *record, pos map[string]int) ([]model.Issue, error) {
	if !rec.issueType.IsContainer() {
		return nil, nil
	}
	kids := make([]*record, 0)
	for _, child := range e.childRecords(rec.id, pos) {
		if visibleUnder(rec.retention, child.retention) {
			kids = append(kids, child)
		}
	}
	return e.hydrateAll(kids, pos)
}

// visibleUnder decides whether a child counts toward its container's derived
// state. A live container shows only live children, so archiving a child
// removes it from the epic's progress; a container that is itself out of the
// flow keeps its whole child set, so an archived epic's state stays the state
// it had when it left rather than collapsing to an empty container's open.
func visibleUnder(parent, child model.Retention) bool {
	return model.Frozen(parent) || !model.Frozen(child)
}

// childRecords returns the parent-child children of parentID in rank order.
func (e *Engine) childRecords(parentID string, pos map[string]int) []*record {
	out := make([]*record, 0)
	for _, rel := range e.relations {
		if rel.Type != model.RelParentChild || rel.DstID != parentID {
			continue
		}
		if child, ok := e.issues[rel.SrcID]; ok {
			out = append(out, child)
		}
	}
	slices.SortStableFunc(out, func(a, b *record) int { return pos[a.id] - pos[b.id] })
	return out
}

// labelNames returns an issue's label set, sorted by name — the whole set,
// which is what every mutating label verb hands back so that "what does it
// have now" is never a follow-up read.
func (e *Engine) labelNames(issueID string) []string {
	rows := e.labels[issueID]
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names
}

// --- history --------------------------------------------------------------

// eventSpec is a mutation's account of itself: what verb was applied, why, by
// whom, and which fields actually moved. It carries no id, timestamp, or
// attribution, because those belong to the recording rather than to the
// mutation — recordEvent stamps them at the single insertion point below.
type eventSpec struct {
	action  string
	reason  string
	actor   string
	changes []model.FieldChange
}

// recordEvent is the one place history is written.
//
// Attribution is read off the engine here rather than passed in, so "every
// work mutation carries its checkout's attribution pair" is a property of this
// write path and not an obligation on every call site that could be forgotten
// at one of them. [LAW:single-enforcer]
func (e *Engine) recordEvent(issueID string, spec eventSpec, now time.Time) {
	actor := strings.TrimSpace(spec.actor)
	if actor == "" {
		actor = "unknown"
	}
	e.events = append(e.events, model.IssueEvent{
		ID:          "evt-" + uuid.NewString(),
		IssueID:     issueID,
		Action:      strings.TrimSpace(spec.action),
		Reason:      strings.TrimSpace(spec.reason),
		Actor:       actor,
		CreatedAt:   now,
		Attribution: e.attribution,
		Changes:     spec.changes,
	})
}

// recordEvents writes every event a mutation owed, in the order it owed them.
// A mutation that moved nothing owes none, which is how a pure no-op leaves
// the history untouched without anybody testing for it.
func (e *Engine) recordEvents(issueID string, specs []eventSpec, now time.Time) {
	for _, spec := range specs {
		e.recordEvent(issueID, spec, now)
	}
}

// cloneEvents copies a run of history for a caller to keep.
//
// The copy goes one level deeper than the slice, and that depth is the whole
// point: model.IssueEvent carries its field changes in a slice, so handing
// back the stored value would hand back a window into this engine's history
// that a caller could write through. Every other model value crossing this
// boundary is composed fresh or is a plain value; this is the one that would
// have aliased. [LAW:no-shared-mutable-globals]
func cloneEvents(events []model.IssueEvent) []model.IssueEvent {
	out := make([]model.IssueEvent, 0, len(events))
	for _, event := range events {
		event.Changes = slices.Clone(event.Changes)
		out = append(out, event)
	}
	return out
}

// now is the engine's clock, read at the write boundary so nothing that plans
// a mutation has to reach for one. [LAW:effects-at-boundaries]
func (e *Engine) now() time.Time { return time.Now().UTC() }
