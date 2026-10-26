package storage

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// RankPlacement selects where a newly created issue lands in the rank order.
// Its zero value is RankBottom, so the product default — filing work records
// it without promoting it — is also the type default: a CreateIssueInput that
// says nothing about placement gets bottom. That is the whole enforcement
// mechanism behind "one default across every creation surface": every creation
// path (interactive, followup, both import formats) reaches the default by
// saying nothing, so no path can drift from another without someone typing a
// placement it does not mean. [LAW:one-source-of-truth]
//
// A rank that sorts after everything also sorts after every frame-mate, and a
// child's rank is only ever compared against its siblings' (tree order compares
// the containers' keys first), so bottom-of-order is
// bottom-of-frame with no frame-scoped machinery.
//
// The top has no such luck: sorting before everything is not sorting before my
// siblings and nothing else, so RankTop is read within the frame the issue is
// filed into. The asymmetry is the two edges answering the same question —
// "which issues is this one ordered against?" — where only one of them gets
// the right answer by accident.
type RankPlacement int

const (
	RankBottom RankPlacement = iota // sorts after all existing items (default)
	RankTop                         // sorts before every item in the frame it is filed into
)

// IssueIdentity is an issue's id with the topic it was created under — the two
// values its id is rendered from, and what ListIssueIdentities returns.
type IssueIdentity struct {
	ID    string
	Topic string
}

type CreateIssueInput struct {
	Title       string
	Description string
	Prompt      string
	Plan        string
	// IssueType is already-parsed vocabulary, never a raw flag string — trust
	// boundaries route through model.ParseIssueType before constructing the
	// input. The zero value means "unspecified" and defaults to task, mirroring
	// how the Placement zero value is the product default.
	// [LAW:types-are-the-program]
	IssueType model.IssueType
	Topic     string
	ParentID  string
	// Priority is already-parsed domain vocabulary, never a raw flag int —
	// trust boundaries route through model.ParsePriority (or the
	// model.CanonicalPriority salvage coercion) before constructing the input.
	// [LAW:types-are-the-program]
	Priority model.Priority
	Assignee string
	Lane     string
	Labels   []string
	// FollowsUp names the ticket the new issue follows up (`lit followup --on`);
	// empty for an ordinary create. It is the only way the field is ever
	// written: UpdateIssueInput does not carry it, so a follow-up's origin is
	// fixed at birth. [LAW:types-are-the-program]
	FollowsUp string
	// Placement decides where the new issue lands in the rank order. Zero value
	// (RankBottom) appends, so an authored batch keeps its order for free;
	// callers filing a ticket ahead of the issues it will be ordered against —
	// its parent's other children, or the rest of the top level — pass RankTop.
	Placement RankPlacement
	// Prefix is the workspace's cosmetic ID prefix (e.g., "links" → "links-foo-abc1").
	// Sourced from workspace config at the call site. Not persisted as derived state.
	Prefix string
	// Actor is who files the issue: the created event's actor, and the author
	// of the parent edge and labels the create wires.
	Actor string
}

// Actor is who a write records as its author: the name the caller gave,
// trimmed, or "unknown" when it gave none. A write attributed to nobody is a
// real state — an engine cannot invent who did something — and this is the
// one spelling of it. [LAW:one-source-of-truth]
func Actor(name string) string {
	if actor := strings.TrimSpace(name); actor != "" {
		return actor
	}
	return "unknown"
}

// UpdateIssueInput is the field-axis patch of a Change: only columns the field
// axis owns are representable, so a status write through the field path is
// unconstructible rather than guarded at runtime. [LAW:types-are-the-program]
type UpdateIssueInput struct {
	Title       *string
	Description *string
	Prompt      *string
	Plan        *string
	IssueType   *model.IssueType
	Priority    *model.Priority
	Assignee    *string
	Lane        *string
	Labels      *[]string
	// Reason is optional free text recorded on the field-change event.
	Reason string
}

// Change is THE issue-record mutation input: an optional lifecycle action
// paired with a field patch and the transition's provenance. nil Action means
// no transition; empty Fields means no field mutations. The action variant
// carries exactly its payload (Start the assignee, Close the outcome), so loose
// per-action parameters are unrepresentable. Which axis the action drives —
// status machine or retention — is the sum's own structure (the
// StatusAction/RetentionAction partition), never a caller-side mode.
// [LAW:types-are-the-program]
//
// Actor is THE actor for the whole change — one call, one author, recorded on
// both events it may produce. [LAW:one-source-of-truth] Reasons stay
// per-event: Reason belongs to the transition event, Fields.Reason to the
// field-change event, because a combined change records two events whose
// reasons are independently set — `lit update` deliberately synthesizes a
// transition reason while leaving the field reason as typed.
type Change struct {
	Action model.Action
	Fields UpdateIssueInput
	Actor  string
	Reason string
}

// Applied is what one Change did: the issue as the change left it, the claim
// it handed from one holder to another — nil for every change that handed
// none, which is every change but a start taking a ticket somebody had
// already taken — and the change rows its field patch made, none for a patch
// that moved no field. The engine reports both from the comparison that
// decided its write, so nothing above it works them out again.
// [LAW:one-source-of-truth]
type Applied struct {
	Issue    model.Issue
	Handover *claims.Handover
	Fields   []model.FieldChange
}

// PurgeInput names the issue to purge and who is purging it.
type PurgeInput struct {
	IssueID string
	Actor   string
}

// PurgeResult is the record a purge left and how many of the issue's rows went
// with it, so the caller can say what was removed without a read beforehand.
type PurgeResult struct {
	Purge       model.Purge
	Comments    int
	PlanReviews int
	Labels      int
	Relations   int
	Events      int
}

// EventRecord is the purge as event counts: how many of each kind of row went
// with the issue.
func (r PurgeResult) EventRecord() map[string]int {
	return map[string]int{
		"purge.comments":     r.Comments,
		"purge.plan_reviews": r.PlanReviews,
		"purge.labels":       r.Labels,
		"purge.edges":        r.Relations,
		"purge.events":       r.Events,
	}
}

func (c Change) IsEmpty() bool {
	return c.Action == nil && c.Fields.IsEmpty()
}

// ChangePlan decides the Change to apply from the issue as the write finds
// it. A Change is its own plan; a plan that reads the issue — the label verbs,
// which state the set the issue holds with one name added or removed — is
// decided under the write's hold, never from a read taken before it.
// [LAW:no-ambient-temporal-coupling]
type ChangePlan interface {
	PlanChange(current model.Issue) (Change, error)
}

// PlanChange is c, whatever the issue holds.
func (c Change) PlanChange(model.Issue) (Change, error) { return c, nil }

// AddLabel is `lit label add`: the issue's label set with Name in it, written
// as an edit of the labels field by Actor. Adding a label the issue already
// holds restates the set, which records nothing.
type AddLabel struct {
	Name  string
	Actor string
}

func (a AddLabel) PlanChange(current model.Issue) (Change, error) {
	return labelsEdit(slices.Concat(current.Labels, []string{a.Name}), a.Actor), nil
}

// RemoveLabel is `lit label rm`: the issue's label set without Name, written as
// an edit of the labels field by Actor. A label the issue does not hold is
// NotFound — nothing was removed, and an empty edit would say otherwise.
// [LAW:no-silent-failure]
type RemoveLabel struct {
	Name  string
	Actor string
}

func (r RemoveLabel) PlanChange(current model.Issue) (Change, error) {
	name, err := model.NormalizeLabel(r.Name)
	if err != nil {
		return Change{}, err
	}
	kept := slices.DeleteFunc(slices.Clone(current.Labels), func(label string) bool { return label == name })
	if len(kept) == len(current.Labels) {
		return Change{}, NotFoundError{Entity: "label", ID: fmt.Sprintf("%s/%s", current.ID, name)}
	}
	return labelsEdit(kept, r.Actor), nil
}

func labelsEdit(labels []string, actor string) Change {
	return Change{Fields: UpdateIssueInput{Labels: &labels}, Actor: actor}
}

type SortSpec struct {
	Field string
	Desc  bool
}

// ListIssuesFilter is the whole variability of the issue listing surface,
// carried as one value. Every slice is an OR within itself and an AND against
// the other criteria; every zero value means "do not constrain on this axis",
// which is why a listing needs no mode flags and why an engine adding a new
// criterion cannot change what an existing caller's filter means.
// [LAW:dataflow-not-control-flow] The listing runs one path; this value is
// what varies.
//
// IncludeArchived and IncludeDeleted are the two axes whose default is a
// filter rather than an absence: a listing that says nothing about retention
// sees only live issues.
//
// ParentIDs selects the direct children of the named issues — membership read
// off the parent-child edge, never off the id prefix. It is the one axis whose
// criteria must exist: an id naming no issue is a NotFoundError, because "that
// parent has no children" and "there is no such parent" are different facts,
// and an empty listing could only say the first. [LAW:no-silent-failure]
type ListIssuesFilter struct {
	Statuses          []model.State
	Resolutions       []model.Resolution
	IssueTypes        []model.IssueType
	ExcludeIssueTypes []model.IssueType
	Assignees         []string
	SearchTerms       []string
	IDs               []string
	ParentIDs         []string
	HasComments       *bool
	LabelsAll         []string
	UpdatedAfter      *time.Time
	UpdatedBefore     *time.Time
	IncludeArchived   bool
	IncludeDeleted    bool
	SortBy            []SortSpec
	Limit             int
}
