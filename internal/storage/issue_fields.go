package storage

import (
	"time"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/selector"
)

// IssueTable is every field a selection can read of an issue, and the one
// statement of which fields exist: the parser refuses a name it lacks, the
// help text lists it, and the conformance suite asks both engines to answer
// every field in it. [LAW:one-source-of-truth]
var IssueTable = selector.NewTable("issue")

// WorkspaceTable is what a path that starts with / reads from.
var WorkspaceTable = selector.NewTable("workspace")

// IssueSchema is what a selection over issues is parsed against.
var IssueSchema = selector.Schema{Root: IssueTable, Anchor: WorkspaceTable}

// The fields of IssueTable. Each value an = or != compares with is parsed by
// its field's own domain parser, so a value is compared in the one spelling
// the store holds whichever surface it was typed at. [LAW:single-enforcer]
var (
	IssueID       = FieldOf[model.ID]{selector.TextField("id", "The issue's id.", nil)}
	IssueTitle    = selector.TextField("title", "The title.", nil)
	IssueTopic    = selector.TextField("topic", "The topic the id was minted under.", issueid.NormalizeTopicForCreate)
	IssueType     = FieldOf[model.IssueType]{selector.EnumField("type", "The issue type.", names(model.IssueTypes()), canonical(model.ParseIssueType))}
	IssuePriority = selector.EnumField("priority", "The priority.", priorityNames(), func(s string) (string, error) {
		p, err := model.ParsePriorityName(s)
		return p.String(), err
	})
	IssueAssignee = selector.TextField("assignee", "Who holds the issue; absent when nobody does.", model.NormalizeAssignee)
	IssueLane     = selector.TextField("lane", "The lane among its siblings; absent in the shared lane.", func(s string) (string, error) {
		lane, err := ParseLane(s)
		if err != nil {
			return "", err
		}
		return lane.Text()
	})
	IssueLabels      = LabelsOf{selector.TextsField("labels", "The labels.", model.NormalizeLabel)}
	IssueStatus      = FieldOf[model.State]{selector.EnumField("status", "The state. An epic's is its leaves': closed once every one is, in_progress once any has started or closed, else open; a live epic counts only live leaves.", names(model.States()), canonical(model.ParseState))}
	IssueResolution  = selector.EnumField("resolution", "How a closed issue ended; absent for an open one and for one closed by done.", names(model.Resolutions()), canonical(model.ParseResolution))
	IssueCreatedAt   = selector.StampField("created_at", "When the issue was created.")
	IssueUpdatedAt   = selector.StampField("updated_at", "When the issue last changed.")
	IssueClosedAt    = selector.StampField("closed_at", "When a closed leaf closed; absent otherwise.")
	IssueDescription = selector.TextField("description", "The description.", nil)
	IssuePrompt      = selector.TextField("prompt", "The agent prompt.", nil)
	IssuePlan        = selector.TextField("plan", "The plan.", nil)
	IssueText        = selector.SearchField("text", "The title, description, prompt, plan and topic.")

	IssueParent         = FieldOf[model.ID]{selector.RefField("parent", "The epic the issue sits in.", IssueTable)}
	IssueChildren       = selector.RowsField("children", "The issues sitting in this one.", IssueTable)
	IssueSiblings       = selector.RowsField("siblings", "The other issues in the same epic.", IssueTable)
	IssueBlockedBy      = selector.RowsField("blocked_by", "The issues this one waits on.", IssueTable)
	IssueBlocks         = selector.RowsField("blocks", "The issues waiting on this one.", IssueTable)
	IssueRelated        = selector.RowsField("related", "The issues related to this one, either way round.", IssueTable)
	IssueFollowsUp      = selector.RefField("follows_up", "The issue this one was filed to follow up.", IssueTable)
	IssueFollowUps      = selector.RowsField("follow_ups", "The issues filed to follow this one up.", IssueTable)
	IssueRedirectTarget = selector.RefField("redirect_target", "The issue a duplicate or superseded issue points to.", IssueTable)

	IssueComments    = selector.RecordsField("comments", "The comments.")
	IssuePhase       = selector.EnumField("phase", "The phase of an in-progress leaf: its newest phase move, or plan since it started; absent for every other issue.", names(model.Phases()), canonical(model.ParsePhase))
	IssuePlanReviews = selector.RecordsField("plan_reviews", "The verdicts recorded on the plan.")

	WorkspaceIssues = selector.RowsField("issues", "Every issue.", IssueTable)
)

func init() {
	IssueTable.Define(IssueID.Field,
		IssueID.Field, IssueTitle, IssueTopic, IssueType.Field, IssuePriority, IssueAssignee, IssueLane, IssueLabels.Field, IssueStatus.Field, IssueResolution,
		IssueCreatedAt, IssueUpdatedAt, IssueClosedAt, IssueDescription, IssuePrompt, IssuePlan, IssueText,
		IssueParent.Field, IssueChildren, IssueSiblings, IssueBlockedBy, IssueBlocks, IssueRelated, IssueFollowsUp, IssueFollowUps, IssueRedirectTarget,
		IssueComments, IssuePhase, IssuePlanReviews)
	WorkspaceTable.Define(nil, WorkspaceIssues)
}

// SelectorExamples are the selectors the reference shows, each parsed by a
// test against the table, so an example cannot name a field that is gone.
var SelectorExamples = []selector.Example{
	{Text: "status=open type=bug,task", Says: "open bugs and tasks"},
	{Text: "!assignee status!=closed", Says: "unheld work that is not closed"},
	{Text: `text~"rank order"`, Says: "issues whose texts mention rank order"},
	{Text: "labels=needs-design !comments", Says: "needs-design issues nobody has commented on"},
	{Text: "updated_at<now-7d status=in_progress", Says: "work started and untouched for a week"},
	{Text: "parent/status=closed", Says: "issues whose epic has closed"},
	{Text: "type=epic children(status!=closed)/count=0", Says: "epics with nothing left open"},
	{Text: "blocked_by(status!=closed)/count>0", Says: "issues waiting on unfinished work"},
	{Text: "phase=review", Says: "work in review"},
	{Text: "/issues(type=bug status=open)/count>20 type=bug", Says: "every bug, when more than 20 are open"},
}

// SelectorReference is the whole reference for a selection over issues.
func SelectorReference() string { return selector.Describe(IssueSchema, SelectorExamples) }

// FieldOf is a field of IssueTable whose values are T, so a program builds
// its atoms from values the compiler checks rather than from text: a field
// renamed or retyped fails the build at every such atom.
// [LAW:types-are-the-program]
type FieldOf[T ~string] struct{ *selector.Field }

// Is is the selector holding where the field is one of values.
func (f FieldOf[T]) Is(values ...T) Selector {
	return Selector{sel: selector.Is(f.Field, names(values)...)}
}

// LabelsOf is the labels field, typed for a program that names a label.
type LabelsOf struct{ *selector.Field }

// Has is the selector holding where the issue carries label. label is a name
// the program states, so one the label rule refuses is the program's mistake
// and panics. [LAW:no-silent-failure]
func (f LabelsOf) Has(label string) Selector {
	canonical, err := model.NormalizeLabel(label)
	if err != nil {
		panic(err)
	}
	return Selector{sel: selector.Is(f.Field, canonical)}
}

// Selector is a selection over IssueTable. It is minted only by parsing
// against IssueSchema or from the typed fields above, so the port is never
// handed atoms over a table it does not hold. The zero Selector selects every
// issue. [LAW:types-are-the-program]
type Selector struct {
	sel selector.Selector
}

// ParseSelector reads text as a selection over issues, now being the instant
// a stamp written from now is read against.
func ParseSelector(text string, now time.Time) (Selector, error) {
	sel, err := selector.Parse(IssueSchema, text, now)
	return Selector{sel: sel}, err
}

// ParseAtom is one atom over issues given as its parts, read as the same atom
// in a selector's text would be.
func ParseAtom(now time.Time, path string, op selector.Op, values ...string) (Selector, error) {
	sel, err := selector.ParseAtom(IssueSchema, now, path, op, values...)
	return Selector{sel: sel}, err
}

// And is the selection every part selects.
func And(parts ...Selector) Selector {
	sels := make([]selector.Selector, 0, len(parts))
	for _, part := range parts {
		sels = append(sels, part.sel)
	}
	return Selector{sel: selector.And(sels...)}
}

// Atoms are the selector's atoms, every one of which must hold.
func (s Selector) Atoms() []selector.Atom { return s.sel.Atoms() }

// Starts reports whether an atom reads f from the issue itself.
func (s Selector) Starts(f *selector.Field) bool { return s.sel.Starts(f) }

// Reads reports whether the selector reads f anywhere, through any path.
func (s Selector) Reads(f *selector.Field) bool { return s.sel.Reads(f) }

// Selection is which issues a read is about: those Selector selects, among the
// issues that exist for the read. Archived and deleted issues exist for it
// only when it includes them, and that holds at every step of every path —
// a deleted child is no child, a deleted parent no parent — so a selector
// means the same thing whichever issues the read admits. [LAW:one-source-of-truth]
type Selection struct {
	Selector        Selector
	IncludeArchived bool
	IncludeDeleted  bool
}

// Admits reports whether an issue retained as r exists for the selection.
func (s Selection) Admits(r model.Retention) bool {
	switch r.(type) {
	case model.Archived:
		return s.IncludeArchived
	case model.Deleted:
		return s.IncludeDeleted
	default:
		return true
	}
}

// Listing is a selection read as issues: in Sort's order, then cut to the
// first Limit. A Limit of zero is no limit.
type Listing struct {
	Selection
	Sort  []SortSpec
	Limit int
}

func names[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

func canonical[T ~string](parse func(string) (T, error)) func(string) (string, error) {
	return func(s string) (string, error) {
		v, err := parse(s)
		return string(v), err
	}
}

func priorityNames() []string {
	out := make([]string, 0, len(model.Priorities()))
	for _, p := range model.Priorities() {
		out = append(out, p.String())
	}
	return out
}
