package storage

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The editable fields of the issue record, as one table.
//
// "Which fields may a patch write" and "which fields does history record" are
// the same question. They used to have four answers — an apply block and a
// diff block inside each engine — and nothing held any of them to the others.
// They drifted by exactly one field: both engines wrote prompt and neither
// recorded it, so editing an agent prompt changed the row and left no history
// at all (links-store-seam-q35v.8). A hand-maintained list is a map of the
// editable set that only stays true while somebody remembers to redraw it.
//
// One row carries a field's whole story: where a patch states it, how it
// lands on the issue, and how its value encodes into a change row. Apply and
// diff walk the same rows, so a field cannot be written without being
// recorded — the omission is unrepresentable now rather than merely fixed.
// [LAW:one-source-of-truth] [LAW:dataflow-not-control-flow]
type issueField struct {
	// name is the field's history vocabulary, which is the domain's own — the
	// name the CLI accepts and prints, never a storage column's spelling.
	name   string
	apply  func(*model.Issue, UpdateIssueInput) error
	encode func(model.Issue) string
}

// patchField builds a row from the three parts that differ per field: which
// pointer on the patch states it, how a stated value lands on the issue, and
// how the issue's value encodes for history.
//
// A nil pointer means "leave this alone", and this is the one place that
// sentence is written. Each engine used to restate it per field, which is
// eight chances per engine to write a mutation with no matching diff.
// [LAW:single-enforcer]
func patchField[T any](
	name string,
	stated func(UpdateIssueInput) *T,
	write func(*model.Issue, T) error,
	encode func(model.Issue) string,
) issueField {
	return issueField{
		name: name,
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
	patchField("title",
		func(in UpdateIssueInput) *string { return in.Title },
		func(issue *model.Issue, v string) error {
			issue.Title = strings.TrimSpace(v)
			if issue.Title == "" {
				return errors.New("title cannot be empty")
			}
			return nil
		},
		func(issue model.Issue) string { return issue.Title }),
	patchField("description",
		func(in UpdateIssueInput) *string { return in.Description },
		func(issue *model.Issue, v string) error { issue.Description = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Description }),
	patchField("prompt",
		func(in UpdateIssueInput) *string { return in.Prompt },
		func(issue *model.Issue, v string) error { issue.Prompt = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Prompt }),
	patchField("issue_type",
		func(in UpdateIssueInput) *model.IssueType { return in.IssueType },
		func(issue *model.Issue, v model.IssueType) error {
			// [LAW:single-enforcer] Container vs leaf decides which lifecycle
			// expression backs the issue, so crossing that line would orphan
			// the one it has: an epic turned leaf keeps a state derived from
			// children it no longer claims, and a leaf turned epic drops its
			// status. Refuse it here rather than inventing a default
			// downstream.
			if issue.IssueType.IsContainer() != v.IsContainer() {
				return fmt.Errorf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerTypes())
			}
			issue.IssueType = v
			return nil
		},
		func(issue model.Issue) string { return string(issue.IssueType) }),
	patchField("priority",
		func(in UpdateIssueInput) *model.Priority { return in.Priority },
		func(issue *model.Issue, v model.Priority) error { issue.Priority = v; return nil },
		// The numeric wire encoding, not the display name: history keeps what
		// the field stores, so a replay reads back the value that was written.
		func(issue model.Issue) string { return strconv.Itoa(int(issue.Priority)) }),
	patchField("assignee",
		func(in UpdateIssueInput) *string { return in.Assignee },
		// [LAW:decomposition] Assignee is an issue-level field independent of
		// the lifecycle; reassigning is a plain field write, not a status
		// mutation.
		func(issue *model.Issue, v string) error { issue.Assignee = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.AssigneeValue() }),
	patchField("lane",
		func(in UpdateIssueInput) *string { return in.Lane },
		func(issue *model.Issue, v string) error { issue.Lane = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Lane }),
	patchField("labels",
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
