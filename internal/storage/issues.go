package storage

import (
	"time"

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
// child's rank is only ever compared against its siblings' (composite rank is
// keyed on the containing epic's rank first), so bottom-of-order is
// bottom-of-frame with no frame-scoped machinery.
type RankPlacement int

const (
	RankBottom RankPlacement = iota // sorts after all existing items (default)
	RankTop                         // sorts before all existing items
)

type CreateIssueInput struct {
	Title       string
	Description string
	Prompt      string
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
	// Placement decides where the new issue lands in the rank order. Zero value
	// (RankBottom) appends, so an authored batch keeps its order for free;
	// callers promoting a ticket to the front of the agenda pass RankTop.
	Placement RankPlacement
	// Prefix is the workspace's cosmetic ID prefix (e.g., "links" → "links-foo-abc1").
	// Sourced from workspace config at the call site. Not persisted as derived state.
	Prefix string
}

// UpdateIssueInput is the field-axis patch of a Change: only columns the field
// axis owns are representable, so a status write through the field path is
// unconstructible rather than guarded at runtime. [LAW:types-are-the-program]
type UpdateIssueInput struct {
	Title       *string
	Description *string
	Prompt      *string
	IssueType   *model.IssueType
	Priority    *model.Priority
	Assignee    *string
	Lane        *string
	Labels      *[]string
	// Reason is optional free text recorded on the field-change event.
	Reason string
}

func (u UpdateIssueInput) IsEmpty() bool {
	return u.Title == nil && u.Description == nil && u.Prompt == nil && u.IssueType == nil &&
		u.Priority == nil && u.Assignee == nil && u.Lane == nil && u.Labels == nil
}

// Change is THE issue-record mutation input: an optional lifecycle action
// paired with a field patch and the transition's provenance. nil Action means
// no transition; empty Fields means no field mutations. The action variant
// carries exactly its payload (Start the assignee, Close the outcome), so the
// loose per-action parameters this seam used to thread are unrepresentable.
// Which axis the action drives — status machine or retention — is the sum's
// own structure (the StatusAction/RetentionAction partition), never a
// caller-side mode.
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

func (c Change) IsEmpty() bool {
	return c.Action == nil && c.Fields.IsEmpty()
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
type ListIssuesFilter struct {
	Statuses          []model.State
	Resolutions       []model.Resolution
	IssueTypes        []model.IssueType
	ExcludeIssueTypes []model.IssueType
	Assignees         []string
	SearchTerms       []string
	IDs               []string
	HasComments       *bool
	LabelsAll         []string
	UpdatedAfter      *time.Time
	UpdatedBefore     *time.Time
	IncludeArchived   bool
	IncludeDeleted    bool
	SortBy            []SortSpec
	Limit             int
}
