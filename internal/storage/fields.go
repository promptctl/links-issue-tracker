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
// One row carries a field's whole story: where a patch states it, where a bulk
// update document states it — read in and written back out — how it lands on
// the issue, and how its value encodes into a change row and is restored from
// one. Apply, diff, the patch's emptiness, the bulk translation in both
// directions, and undo walk the same rows, so a field cannot be written without
// being recorded, stated without being seen as stated, read in without being
// written back out, nor recorded without being restorable — the omission is
// unrepresentable rather than guarded against.
// [LAW:one-source-of-truth] [LAW:dataflow-not-control-flow]
type issueField struct {
	// name is the field's history vocabulary, which is the domain's own — the
	// name the CLI accepts and prints, never a storage column's spelling.
	name     string
	text     FieldText
	stated   func(UpdateIssueInput) bool
	fromBulk func(BulkIssueSpec, *UpdateIssueInput) error
	toBulk   func(model.Issue, *BulkIssueSpec)
	apply    func(*model.Issue, UpdateIssueInput) error
	encode   func(model.Issue) string
	// restore states on a bulk update document the value a change row
	// recorded for this field: encode's inverse, landing where fromBulk reads.
	restore func(*BulkIssueSpec, string) error
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
// its text, which pointer on the patch states it, which key of a bulk update
// document states it, how a stated value lands on the issue, the issue's
// current value, and how that value encodes for history.
//
// A nil pointer means "leave this alone", and this is the one place that
// sentence is written. Restating it per field in each engine would be eight
// chances per engine to write a mutation with no matching diff.
// [LAW:single-enforcer]
//
// current is the row's one reading of the issue: history encodes it, and a
// bulk document states it, so what a document says the issue holds is what a
// diff compares against. [LAW:one-source-of-truth]
func patchField[T any](
	name string,
	text FieldText,
	slot func(*UpdateIssueInput) **T,
	bulk bulkKey[T],
	write func(*model.Issue, T) error,
	current func(model.Issue) T,
	history historyCodec[T],
) issueField {
	return issueField{
		name:   name,
		text:   text,
		stated: func(in UpdateIssueInput) bool { return *slot(&in) != nil },
		fromBulk: func(doc BulkIssueSpec, in *UpdateIssueInput) error {
			value, err := bulk.read(doc)
			if err != nil {
				return err
			}
			*slot(in) = value
			return nil
		},
		toBulk: func(issue model.Issue, doc *BulkIssueSpec) { bulk.state(doc, current(issue)) },
		apply: func(issue *model.Issue, in UpdateIssueInput) error {
			value := *slot(&in)
			if value == nil {
				return nil
			}
			return write(issue, *value)
		},
		encode: func(issue model.Issue) string { return history.encode(current(issue)) },
		restore: func(doc *BulkIssueSpec, recorded string) error {
			value, err := history.decode(recorded)
			if err != nil {
				return fmt.Errorf("restore %s from recorded %q: %w", name, recorded, err)
			}
			bulk.state(doc, value)
			return nil
		},
	}
}

// historyCodec is how a field's value is written into a change row and read
// back out of one. The two are one value so a field cannot record a text it
// has no way to restore.
type historyCodec[T any] struct {
	encode func(T) string
	decode func(string) (T, error)
}

// bulkKey is the key of a bulk update document that states one field, both
// ways: read is what a document states for it, nil when unstated, and state
// writes a value of the field into a document. Each is the other's inverse.
type bulkKey[T any] struct {
	read  func(BulkIssueSpec) (*T, error)
	state func(*BulkIssueSpec, T)
}

// bulkAsStated is the key of a field whose document key carries the patch's
// own type, so the document's pointer is the patch's.
func bulkAsStated[T any](key func(*BulkIssueSpec) **T) bulkKey[T] {
	return bulkKey[T]{
		read:  func(doc BulkIssueSpec) (*T, error) { return *key(&doc), nil },
		state: func(doc *BulkIssueSpec, value T) { *key(doc) = &value },
	}
}

// bulkParsed is the key of a field whose document key carries raw input: a
// stated value goes through the field's one parse gate, an unstated one stays
// unstated, and a value is stated back as the raw input that parses to it.
func bulkParsed[R, T any](name string, key func(*BulkIssueSpec) **R, parse func(R) (T, error), raw func(T) R) bulkKey[T] {
	return bulkKey[T]{
		read: func(doc BulkIssueSpec) (*T, error) {
			stated := *key(&doc)
			if stated == nil {
				return nil, nil
			}
			value, err := parse(*stated)
			if err != nil {
				return nil, invalidValue(name, *stated, err)
			}
			return &value, nil
		},
		state: func(doc *BulkIssueSpec, value T) {
			stated := raw(value)
			*key(doc) = &stated
		},
	}
}

// asText is the history codec of a field whose value is already its text.
var asText = historyCodec[string]{
	encode: func(v string) string { return v },
	decode: func(v string) (string, error) { return v, nil },
}

// invalidValue is the one refusal of a document value its parse refused,
// whichever document shape states it. It names the document key and the value
// it held, so the author can find the line to fix, and wraps the parse's own
// refusal, which names what is allowed. [LAW:one-source-of-truth]
func invalidValue(key string, raw any, err error) error {
	return fmt.Errorf("has invalid %s %#v: %w", key, raw, err)
}

// IsEmpty reports whether the patch states no editable field. Reason is not a
// field: it annotates the change a stated field makes.
func (u UpdateIssueInput) IsEmpty() bool {
	for _, field := range issueFields {
		if field.stated(u) {
			return false
		}
	}
	return true
}

// bulkUpdatePatch is the field patch a bulk update document states, one
// pointer per field the document sets. The bulk gate hands each engine exactly
// this patch, so a document means the same edit whichever engine opens the
// file. [LAW:one-source-of-truth]
func bulkUpdatePatch(doc BulkIssueSpec) (UpdateIssueInput, error) {
	patch := UpdateIssueInput{Reason: strings.TrimSpace(doc.Reason)}
	for _, field := range issueFields {
		if err := field.fromBulk(doc, &patch); err != nil {
			return UpdateIssueInput{}, err
		}
	}
	return patch, nil
}

// BulkUpdateDocument is the update document that states everything about an
// issue a bulk file can edit, as it stands: every editable field, its parent
// when it has one, and its whole blocked-by set ([] for none). An issue with
// no parent states none rather than `parent: ""`, which is a clear, a move the
// gate judges and refuses for a leaf older data left with no epic. It is
// bulkUpdatePatch's inverse, so a file of these imported unedited moves
// nothing, and editing one value in it edits exactly that value.
//
// It states no topic, which an update refuses, no rank, since `rank: top`
// moves an issue rather than saying where it stands, and no reason, which
// annotates an edit and so belongs to whoever makes one.
func BulkUpdateDocument(rel IssueRelations) BulkIssueSpec {
	doc := BulkIssueSpec{ID: rel.Issue.ID}
	for _, field := range issueFields {
		field.toBulk(rel.Issue, &doc)
	}
	if rel.Parent != nil {
		doc.Parent = &rel.Parent.ID
	}
	blockedBy := make([]string, len(rel.BlockedBy))
	for i, blocker := range rel.BlockedBy {
		blockedBy[i] = blocker.ID
	}
	doc.BlockedBy = &blockedBy
	return doc
}

// planFieldName is the plan's name in history's vocabulary, which a review
// resolves its version against.
const planFieldName = "plan"

// issueFields is the editable set, in the order history reports a change to
// them — fixed, so two reads of one mutation describe it identically.
var issueFields = []issueField{
	patchField("title", ShortText,
		func(in *UpdateIssueInput) **string { return &in.Title },
		bulkAsStated(func(doc *BulkIssueSpec) **string { return &doc.Title }),
		func(issue *model.Issue, v string) error {
			issue.Title = strings.TrimSpace(v)
			if issue.Title == "" {
				return model.ValidationError{Message: "title cannot be empty"}
			}
			return nil
		},
		func(issue model.Issue) string { return issue.Title }, asText),
	patchField("description", LongText,
		func(in *UpdateIssueInput) **string { return &in.Description },
		bulkAsStated(func(doc *BulkIssueSpec) **string { return &doc.Description }),
		func(issue *model.Issue, v string) error { issue.Description = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Description }, asText),
	patchField("prompt", LongText,
		func(in *UpdateIssueInput) **string { return &in.Prompt },
		bulkAsStated(func(doc *BulkIssueSpec) **string { return &doc.Prompt }),
		func(issue *model.Issue, v string) error { issue.Prompt = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Prompt }, asText),
	patchField(planFieldName, LongText,
		func(in *UpdateIssueInput) **string { return &in.Plan },
		bulkAsStated(func(doc *BulkIssueSpec) **string { return &doc.Plan }),
		func(issue *model.Issue, v string) error { issue.Plan = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Plan }, asText),
	patchField("issue_type", ShortText,
		func(in *UpdateIssueInput) **model.IssueType { return &in.IssueType },
		bulkParsed("type", func(doc *BulkIssueSpec) **string { return &doc.IssueType }, model.ParseIssueType,
			func(v model.IssueType) string { return string(v) }),
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
		func(issue model.Issue) model.IssueType { return issue.IssueType },
		historyCodec[model.IssueType]{encode: func(v model.IssueType) string { return string(v) }, decode: model.ParseIssueType}),
	patchField("priority", ShortText,
		func(in *UpdateIssueInput) **model.Priority { return &in.Priority },
		bulkParsed("priority", func(doc *BulkIssueSpec) **int { return &doc.Priority }, model.ParsePriority,
			func(v model.Priority) int { return int(v) }),
		func(issue *model.Issue, v model.Priority) error { issue.Priority = v; return nil },
		func(issue model.Issue) model.Priority { return issue.Priority },
		// The numeric wire encoding, not the display name: history keeps what
		// the field stores, so a replay reads back the value that was written.
		historyCodec[model.Priority]{
			encode: func(v model.Priority) string { return strconv.Itoa(int(v)) },
			decode: func(v string) (model.Priority, error) {
				n, err := strconv.Atoi(v)
				if err != nil {
					return 0, err
				}
				return model.ParsePriority(n)
			},
		}),
	patchField("assignee", ShortText,
		func(in *UpdateIssueInput) **string { return &in.Assignee },
		bulkAsStated(func(doc *BulkIssueSpec) **string { return &doc.Assignee }),
		// [LAW:decomposition] Assignee is an issue-level field independent of
		// the lifecycle; reassigning is a plain field write, not a status
		// mutation.
		func(issue *model.Issue, v string) error {
			assignee, err := model.NormalizeAssignee(v)
			if err != nil {
				return err
			}
			issue.Assignee = assignee
			return nil
		},
		func(issue model.Issue) string { return issue.AssigneeValue() }, asText),
	patchField("lane", ShortText,
		func(in *UpdateIssueInput) **string { return &in.Lane },
		bulkAsStated(func(doc *BulkIssueSpec) **string { return &doc.Lane }),
		func(issue *model.Issue, v string) error { issue.Lane = strings.TrimSpace(v); return nil },
		func(issue model.Issue) string { return issue.Lane }, asText),
	patchField("labels", ShortText,
		func(in *UpdateIssueInput) **[]string { return &in.Labels },
		bulkAsStated(func(doc *BulkIssueSpec) **[]string { return &doc.Labels }),
		func(issue *model.Issue, v []string) error {
			labels, err := model.CanonicalizeLabels(v)
			if err != nil {
				return err
			}
			issue.Labels = labels
			return nil
		},
		func(issue model.Issue) []string { return issue.Labels },
		// A label cannot hold a comma (model.NormalizeLabel), so the joined set
		// splits back into exactly the labels it was joined from; the empty
		// text is the empty set.
		historyCodec[[]string]{
			encode: func(v []string) string { return strings.Join(v, ",") },
			decode: func(v string) ([]string, error) {
				if v == "" {
					return []string{}, nil
				}
				return strings.Split(v, ","), nil
			},
		}),
}

// IssueFieldValues is every editable field of issue as a change row records
// it, by field name: the values a write's history is checked against.
func IssueFieldValues(issue model.Issue) map[string]string {
	values := make(map[string]string, len(issueFields))
	for _, field := range issueFields {
		values[field.name] = field.encode(issue)
	}
	return values
}

// IssueFieldNames is the editable fields' names in the table's order, the
// order history reports a change to them in.
func IssueFieldNames() []string {
	names := make([]string, len(issueFields))
	for i, field := range issueFields {
		names[i] = field.name
	}
	return names
}

// RestoreIssueField states on doc the value a change row recorded for field,
// through that field's own row, so a document restores exactly the value
// history holds. A name no row carries is refused.
func RestoreIssueField(doc *BulkIssueSpec, field, recorded string) error {
	for _, row := range issueFields {
		if row.name == field {
			return row.restore(doc, recorded)
		}
	}
	return fmt.Errorf("restore: %q is not an editable field", field)
}

// WithIssueFieldsOf is dst carrying src's editable fields and nothing else of
// src: every field src holds, stated on a document and applied to dst through
// the rows. What still differs between the result and src is what differs
// outside the editable set.
func WithIssueFieldsOf(dst, src model.Issue) (model.Issue, error) {
	var doc BulkIssueSpec
	for _, field := range issueFields {
		field.toBulk(src, &doc)
	}
	patch, err := bulkUpdatePatch(doc)
	if err != nil {
		return model.Issue{}, err
	}
	issue, _, err := ApplyIssueFields(dst, patch)
	return issue, err
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
		case merge.ProsePlan:
			in.Plan = &text
		default:
			return model.Issue{}, nil, fmt.Errorf("issue %s: no editable field for merged prose field %q", provisional.ID, field)
		}
	}
	return ApplyIssueFields(provisional, in)
}
