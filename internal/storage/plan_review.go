package storage

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// PlanReviewStore records law reviews of a ticket's plan. A review is never
// edited or removed: a reviewer who changes their mind records another.
type PlanReviewStore interface {
	// AddPlanReview records a review of one version of an issue's plan,
	// decided by PlanReviewOf, and returns it as stored.
	AddPlanReview(ctx context.Context, in AddPlanReviewInput) (model.PlanReview, error)
}

// AddPlanReviewInput is a reviewer's verdict on the plan version they read.
type AddPlanReviewInput struct {
	IssueID string
	// Version is the plan version the review judged, numbered as lit history
	// numbers it. It is named by the reviewer rather than taken to be the
	// current one, so a plan revised while the review ran is not passed on
	// the strength of the text before it.
	Version   int
	Verdict   model.PlanVerdict
	Laws      []model.Law
	CreatedBy string
}

// PlanReviewOf decides the review a request records against issue, whose
// history is events: the version it names resolved to the text it held and
// that text's digest, the laws in their canonical order, the reviewer. A
// version the history does not hold is VersionNotFoundError, and a fail that
// cites no law is refused — a failing law review names the law the plan
// breaks. Pure, so every engine records the same review for the same request
// and stamps it with its own id and time.
// [LAW:single-enforcer] [LAW:effects-at-boundaries]
func PlanReviewOf(issue model.Issue, events []model.IssueEvent, in AddPlanReviewInput) (model.PlanReview, error) {
	field := PlanField()
	versions := TextVersions(issue, events, field)
	if in.Version < 1 || in.Version > len(versions) {
		return model.PlanReview{}, VersionNotFoundError{IssueID: issue.ID, Field: field.Name, Number: in.Version, Held: len(versions)}
	}
	laws := model.CanonicalLaws(in.Laws)
	if in.Verdict == model.PlanFail && len(laws) == 0 {
		return model.PlanReview{}, model.ValidationError{Message: "a failing verdict cites at least one law the plan breaks"}
	}
	reviewer := strings.TrimSpace(in.CreatedBy)
	if reviewer == "" {
		reviewer = "unknown"
	}
	return model.PlanReview{
		IssueID:    issue.ID,
		PlanDigest: model.DigestPlan(versions[in.Version-1]),
		Verdict:    in.Verdict,
		Laws:       laws,
		CreatedBy:  reviewer,
	}, nil
}

// PlanField is the plan's row of the editable-field table. The table is
// static, so a missing row is a defect in this package, not a condition a
// caller can meet.
func PlanField() LongTextField {
	fields := LongTextFields()
	at := slices.IndexFunc(fields, func(field LongTextField) bool { return field.Name == planFieldName })
	if at < 0 {
		panic(fmt.Sprintf("editable-field table has no long text row %q", planFieldName))
	}
	return fields[at]
}

// TextVersions is every text a long text field is recorded holding, each
// once, numbered from 1 in the order its record first shows it: the texts
// the trail's changes move between, then the current text if the trail never
// reaches it. Each text has one number, so two edits made at once on two
// machines and merged by sync both step from the number of the text they
// started from, and a change back to an earlier text returns to its number.
// The numbers follow the record's order, so a sync that merges in an edit
// older than ones already here renumbers the texts after it — which is why a
// PlanReview names its text by digest, not by number. The empty text is no
// version. A field never changed has one version, its current text.
func TextVersions(issue model.Issue, events []model.IssueEvent, field LongTextField) []string {
	var texts []string
	record := func(text string) {
		if text != "" && !slices.Contains(texts, text) {
			texts = append(texts, text)
		}
	}
	for _, event := range events {
		for _, change := range event.Changes {
			if change.Field == field.Name {
				record(change.From)
				record(change.To)
			}
		}
	}
	record(field.Current(issue))
	return texts
}

// VersionNotFoundError is a reference to a version of a long text field past
// the versions an issue's record holds. It is not NotFoundError: that one's
// remediation sends the caller to check the issue id, and here the id
// resolved — what is missing is the version, and the message says how many
// there are and where the current text is. [LAW:one-type-per-behavior]
type VersionNotFoundError struct {
	IssueID string
	Field   string
	Number  int
	Held    int
}

func (e VersionNotFoundError) Error() string {
	return fmt.Sprintf("%s has no %s@%d: its record holds %s of %s, numbered from 1; `lit show %s --field %s` prints the current text",
		e.IssueID, e.Field, e.Number, DescribeVersions(e.Held), e.Field, e.IssueID, e.Field)
}

// DescribeVersions is how a count of versions reads: "1 version", "3 versions".
func DescribeVersions(count int) string {
	if count == 1 {
		return "1 version"
	}
	return fmt.Sprintf("%d versions", count)
}
