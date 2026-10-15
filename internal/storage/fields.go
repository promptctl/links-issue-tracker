package storage

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The editable fields of the issue record, as one table.
//
// "Which fields may a patch write" and "which fields does history record" are
// the same question. Answered separately — an apply block and a diff block
// inside each engine — it has four answers and nothing holds any of them to the
// others; a field written but never recorded changes the row and leaves no
// history at all. A hand-maintained list is a map of the editable set that
// only stays true while somebody remembers to redraw it.
//
// One row carries a field's whole story: where a patch states it, how it
// lands on the issue, and how its value encodes into a change row. Apply and
// diff walk the same rows, so a field cannot be written without being
// recorded — the omission is unrepresentable rather than guarded against.
// [LAW:one-source-of-truth] [LAW:dataflow-not-control-flow]
type issueField struct {
	// name is the field's history vocabulary, which is the domain's own — the
	// name the CLI accepts and prints, never a storage column's spelling.
	name   string
	text   FieldText
	apply  func(*model.Issue, UpdateIssueInput) error
	encode func(model.Issue) string
}

// FieldText is the shape a field's value takes for the person reading it.
// History records every value in full whatever the shape; the shape decides
// only how a change to it is shown. It is not merge.ProseField: that set is
// the fields only a reader can merge, title among them, and a title is short
// enough to show whole.
type FieldText int

const (
	// ShortText reads whole on one line — a name, a number, a label set — so
	// a change shows as its old value and its new one.
	ShortText FieldText = iota
	// LongText is free text an author writes and rewrites. A change to it
	// shows as a step between numbered versions, so a reader of the trail is
	// not handed every superseded text alongside the current one.
	LongText
)

// LongTextField is one editable field whose value is long text: its name in
// history's vocabulary, and how to read its current text off an issue.
type LongTextField struct {
	Name    string
	Current func(model.Issue) string
}

// LongTextFields is the editable fields whose values are long text, in table
// order. The set is read off the table, so a field joins it by declaring its
// shape on its row and nowhere else. [LAW:one-source-of-truth]
func LongTextFields() []LongTextField {
	var fields []LongTextField
	for _, field := range issueFields {
		if field.text == LongText {
			fields = append(fields, LongTextField{Name: field.name, Current: field.encode})
		}
	}
	return fields
}

// patchField builds a row from the parts that differ per field: the shape of
// its text, which pointer on the patch states it, how a stated value lands on
// the issue, and how the issue's value encodes for history.
//
// A nil pointer means "leave this alone", and this is the one place that
// sentence is written. Restating it per field in each engine would be eight
// chances per engine to write a mutation with no matching diff.
// [LAW:single-enforcer]
func patchField[T any](
	name string,
	text FieldText,
	stated func(UpdateIssueInput) *T,
	write func(*model.Issue, T) error,
	encode func(model.Issue) string,
) issueField {
	return issueField{
		name: name,
		text: text,
		apply: func(issue *model.Issue, in UpdateIssueInput) error {
			value := stated(in)
			if value == nil {
				return nil
			}
			return write(issue, *value)
		},
		encode: encode,
	}
}

// issueFields is the editable set, in the order history reports a change to
// them — fixed, so two reads of one mutation describe it identically.
var issueFields = []issueField{
	patchField("title", ShortText,
		func(in UpdateIssueInput) *string { return in.Title },
		func(issue *model.Issue, v string) error {
			issue.Title = strings.TrimSpace(v)
			if issue.Title == "" {
				return model.ValidationError{Message: "title cannot be empty"}
			}
			return nil
		},
		func(issue model.Issue) string { return issue.Title }),
	patchField("description", LongText,
		func(in UpdateIssueInput) *string { return in.Description },
		func(issue *model.Issue, v string) error { issue.Description = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Description }),
	patchField("prompt", LongText,
		func(in UpdateIssueInput) *string { return in.Prompt },
		func(issue *model.Issue, v string) error { issue.Prompt = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Prompt }),
	patchField("issue_type", ShortText,
		func(in UpdateIssueInput) *model.IssueType { return in.IssueType },
		func(issue *model.Issue, v model.IssueType) error {
			// [LAW:single-enforcer] Container vs leaf decides which lifecycle
			// expression backs the issue, so crossing that line would orphan
			// the one it has: an epic turned leaf keeps a state derived from
			// children it no longer claims, and a leaf turned epic drops its
			// status. Refuse it here rather than inventing a default
			// downstream.
			if issue.IssueType.IsContainer() != v.IsContainer() {
				return model.ValidationError{Message: fmt.Sprintf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerTypes())}
			}
			issue.IssueType = v
			return nil
		},
		func(issue model.Issue) string { return string(issue.IssueType) }),
	patchField("priority", ShortText,
		func(in UpdateIssueInput) *model.Priority { return in.Priority },
		func(issue *model.Issue, v model.Priority) error { issue.Priority = v; return nil },
		// The numeric wire encoding, not the display name: history keeps what
		// the field stores, so a replay reads back the value that was written.
		func(issue model.Issue) string { return strconv.Itoa(int(issue.Priority)) }),
	patchField("assignee", ShortText,
		func(in UpdateIssueInput) *string { return in.Assignee },
		// [LAW:decomposition] Assignee is an issue-level field independent of
		// the lifecycle; reassigning is a plain field write, not a status
		// mutation.
		func(issue *model.Issue, v string) error {
			assignee, err := model.NormalizeAssignee(v)
			issue.Assignee = assignee
			return err
		},
		func(issue model.Issue) string { return issue.AssigneeValue() }),
	patchField("lane", ShortText,
		func(in UpdateIssueInput) *string { return in.Lane },
		func(issue *model.Issue, v string) error { issue.Lane = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Lane }),
	patchField("labels", ShortText,
		func(in UpdateIssueInput) *[]string { return in.Labels },
		func(issue *model.Issue, v []string) error {
			labels, err := model.CanonicalizeLabels(v)
			if err != nil {
				return err
			}
			issue.Labels = labels
			return nil
		},
		func(issue model.Issue) string { return strings.Join(issue.Labels, ",") }),
}

// ApplyIssueFields is the field axis of a patch, decided: the post-patch issue
// and one change row per field that actually moved. Pure — no clock, no store,
// no writes — so an engine can plan a patch before it opens a transaction and
// stamp the result with its own write time.
//
// Every engine owes lit this exact answer, which is why it is computed here
// rather than per engine. An engine that planned its own would be free to
// disagree about what a patch means, and the differential oracle would read
// that disagreement as engine divergence rather than as the duplicated logic
// it is. [LAW:effects-at-boundaries] [LAW:one-source-of-truth]
func ApplyIssueFields(baseline model.Issue, in UpdateIssueInput) (model.Issue, []model.FieldChange, error) {
	// Encoded eagerly, so a prior is a snapshot of the value rather than a
	// view onto data the apply pass is about to replace.
	priors := make([]string, len(issueFields))
	for i, field := range issueFields {
		priors[i] = field.encode(baseline)
	}
	issue := baseline
	for _, field := range issueFields {
		if err := field.apply(&issue, in); err != nil {
			return model.Issue{}, nil, err
		}
	}
	var changes []model.FieldChange
	for i, field := range issueFields {
		current := field.encode(issue)
		if current == priors[i] {
			continue
		}
		changes = append(changes, model.FieldChange{Field: field.name, From: priors[i], To: current})
	}
	return issue, changes, nil
}

// ApplyProseFields lands a reconcile's merged prose on the provisional issue
// the way any edit lands: stated on a patch and decided by ApplyIssueFields,
// so each field the agent's text moves comes back as its change row, from the
// provisional value to the merged text. The merged text is the agent's edit,
// and an edit cannot be written without being recorded. [LAW:single-enforcer]
//
// The switch is the one mapping from a merge.ProseField to the patch pointer
// that states it; a field it does not name is refused rather than dropped,
// since dropping it would publish the provisional value as if it were merged.
// [LAW:no-silent-failure]
func ApplyProseFields(provisional model.Issue, texts merge.ProseTexts) (model.Issue, []model.FieldChange, error) {
	var in UpdateIssueInput
	for field, text := range texts {
		switch field {
		case merge.ProseTitle:
			in.Title = &text
		case merge.ProseDescription:
			in.Description = &text
		case merge.ProsePrompt:
			in.Prompt = &text
		default:
			return model.Issue{}, nil, fmt.Errorf("issue %s: no editable field for merged prose field %q", provisional.ID, field)
		}
	}
	return ApplyIssueFields(provisional, in)
}
