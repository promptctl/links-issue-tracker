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

// AddPlanReviewInput is a reviewer's verdict on the plan text they read, as
// they gave it; PlanReviewOf parses it.
type AddPlanReviewInput struct {
	IssueID string
	// Plan is the digest of the text the review judged, as `lit show` prints
	// it. It names the text rather than its version number because a sync
	// that merges in an older edit renumbers the versions, and a number read
	// before that merge would name a text the reviewer never read. It is
	// named by the reviewer rather than taken to be the current text, so a
	// plan revised while the review ran is not passed on the strength of the
	// text before it.
	Plan      string
	Verdict   string
	Laws      []string
	CreatedBy string
}

// PlanReviewOf decides the review a request records against issue, whose
// history is events: the verdict, the laws in their canonical order and the
// digest each parsed, the reviewer. A digest of no text the history holds is
// PlanDigestNotFoundError, and a fail that cites no law is refused — a failing
// law review names the law the plan breaks. Pure, so every engine records the
// same review for the same request and stamps it with its own id and time.
// [LAW:single-enforcer] [LAW:effects-at-boundaries] [LAW:parse-dont-validate]
func PlanReviewOf(issue model.Issue, events []model.IssueEvent, in AddPlanReviewInput) (model.PlanReview, error) {
	verdict, err := model.ParsePlanVerdict(in.Verdict)
	if err != nil {
		return model.PlanReview{}, err
	}
	cited := make([]model.Law, len(in.Laws))
	for i, raw := range in.Laws {
		if cited[i], err = model.ParseLaw(raw); err != nil {
			return model.PlanReview{}, err
		}
	}
	laws := model.CanonicalLaws(cited)
	if verdict == model.PlanFail && len(laws) == 0 {
		return model.PlanReview{}, model.ValidationError{Message: "a failing verdict cites at least one law the plan breaks"}
	}
	digest, err := model.ParsePlanDigest(in.Plan)
	if err != nil {
		return model.PlanReview{}, err
	}
	field := PlanField()
	versions := TextVersions(issue, events, field)
	if !slices.ContainsFunc(versions, func(text string) bool { return model.DigestPlan(text) == digest }) {
		return model.PlanReview{}, PlanDigestNotFoundError{IssueID: issue.ID, Digest: digest}
	}
	reviewer := Actor(in.CreatedBy)
	return model.PlanReview{
		IssueID:    issue.ID,
		PlanDigest: digest,
		Verdict:    verdict,
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

// PlanDigestNotFoundError is a review naming a plan text by a digest no text
// the issue's plan history holds. It misses the way VersionNotFoundError
// does, and the message differs: a digest names no number, and the current
// plan's digest is in lit show, not in lit show --field.
type PlanDigestNotFoundError struct {
	IssueID string
	Digest  model.PlanDigest
}

func (e PlanDigestNotFoundError) Error() string {
	return fmt.Sprintf("%s has no plan text with digest %s; `lit show %s` prints the current plan beside its digest", e.IssueID, e.Digest, e.IssueID)
}

// DescribeVersions is how a count of versions reads: "1 version", "3 versions".
func DescribeVersions(count int) string {
	if count == 1 {
		return "1 version"
	}
	return fmt.Sprintf("%d versions", count)
}

// PlanApproval decides whether issue's current plan has passed its law review,
// given its history events and every review of its plan, oldest first. It
// returns the passing review the approval rests on, or PlanUnapprovedError
// naming what is missing. A ticket is finished only on an approved plan.
//
// Each reviewer's latest verdict on the current text stands. The plan is
// approved when no standing verdict is a fail and at least one standing pass
// is by someone other than the text's author. A fail holds until the reviewer
// who gave it passes the same text, or the text changes: /lit:review-plan
// starts a reviewer with a new identity on every run, so running an unchanged
// plan past fresh reviewers until one passes it leaves the first fail
// standing. Pure, so the gate decides the same on every engine.
// [LAW:single-enforcer] [LAW:effects-at-boundaries] [LAW:parse-dont-validate]
func PlanApproval(issue model.Issue, events []model.IssueEvent, reviews []model.PlanReview) (model.PlanReview, error) {
	field := PlanField()
	plan := field.Current(issue)
	if plan == "" {
		return model.PlanReview{}, PlanUnapprovedError{IssueID: issue.ID, Gap: PlanMissing{}}
	}
	version := PlanVersion{
		Number: slices.Index(TextVersions(issue, events, field), plan) + 1,
		Digest: model.DigestPlan(plan),
	}
	author := planAuthor(events, field, plan)
	// Newest first, so the first review seen from each reviewer is the
	// verdict of theirs that stands.
	var standing []model.PlanReview
	for _, review := range slices.Backward(reviews) {
		if review.Judges(plan) && !slices.ContainsFunc(standing, func(seen model.PlanReview) bool { return seen.CreatedBy == review.CreatedBy }) {
			standing = append(standing, review)
		}
	}
	fails := slices.DeleteFunc(slices.Clone(standing), func(review model.PlanReview) bool { return review.Verdict != model.PlanFail })
	if len(fails) > 0 {
		return model.PlanReview{}, PlanUnapprovedError{IssueID: issue.ID, Gap: PlanFailed{Plan: version, Fails: fails}}
	}
	at := slices.IndexFunc(standing, func(review model.PlanReview) bool { return review.CreatedBy != author })
	if at >= 0 {
		return standing[at], nil
	}
	if len(standing) > 0 {
		return model.PlanReview{}, PlanUnapprovedError{IssueID: issue.ID, Gap: PlanSelfReviewed{Plan: version, Author: author}}
	}
	return model.PlanReview{}, PlanUnapprovedError{IssueID: issue.ID, Gap: PlanUnreviewed{Plan: version, EarlierReviewed: len(reviews) > 0}}
}

// planAuthor is who wrote the plan text plan: the actor of the latest event
// that changed the plan to it, else the issue's creator, since a plan given at
// creation is recorded by no change.
func planAuthor(events []model.IssueEvent, field LongTextField, plan string) string {
	author := ""
	for _, event := range events {
		wrote := slices.ContainsFunc(event.Changes, func(change model.FieldChange) bool { return change.Field == field.Name && change.To == plan })
		if wrote || event.Action == model.ActionCreated {
			author = event.Actor
		}
	}
	return author
}

// PlanVersion is one text of a plan as `lit show` names it: its number in the
// history and its digest.
type PlanVersion struct {
	Number int
	Digest model.PlanDigest
}

func (v PlanVersion) String() string {
	return fmt.Sprintf("v%d, digest %s", v.Number, v.Digest)
}

// PlanUnapprovedError is `lit done` refused because the ticket's current plan
// has not passed its law review. Gap says what is missing.
type PlanUnapprovedError struct {
	IssueID string
	Gap     PlanGap
}

func (e PlanUnapprovedError) Error() string {
	return e.Gap.describe(e.IssueID)
}

// PlanGap is what keeps a plan from approval. The variants are sealed, and
// only those with a plan carry its version. [LAW:types-are-the-program]
type PlanGap interface {
	// Kind is the gap's token on the refused command's event.
	Kind() string
	describe(issueID string) string
}

// PlanMissing is a ticket with no plan.
type PlanMissing struct{}

// PlanUnreviewed is a current plan no review has judged. EarlierReviewed is
// set when the reviews on record judged earlier texts.
type PlanUnreviewed struct {
	Plan            PlanVersion
	EarlierReviewed bool
}

// PlanFailed is a current plan with standing fails, newest first.
type PlanFailed struct {
	Plan  PlanVersion
	Fails []model.PlanReview
}

// PlanSelfReviewed is a current plan whose only standing verdicts are its
// author's own passes.
type PlanSelfReviewed struct {
	Plan   PlanVersion
	Author string
}

func (PlanMissing) Kind() string      { return "no_plan" }
func (PlanUnreviewed) Kind() string   { return "unreviewed" }
func (PlanFailed) Kind() string       { return "failed" }
func (PlanSelfReviewed) Kind() string { return "author_only" }

func (PlanMissing) describe(issueID string) string {
	return fmt.Sprintf("%s has no plan; a ticket is finished once a reviewer other than its plan's author has passed the current plan", issueID)
}

func (g PlanUnreviewed) describe(issueID string) string {
	earlier := ""
	if g.EarlierReviewed {
		earlier = "; the reviews on record judged an earlier text"
	}
	return fmt.Sprintf("%s's current plan (%s) has no review%s", issueID, g.Plan, earlier)
}

func (g PlanFailed) describe(issueID string) string {
	fails := make([]string, len(g.Fails))
	for i, review := range g.Fails {
		fails[i] = fmt.Sprintf("%s failed it citing %s", review.CreatedBy, model.JoinLaws(review.Laws))
	}
	return fmt.Sprintf("%s's current plan (%s) failed its law review: %s; a fail stands until its reviewer passes the same text, so revise the plan and have the new text reviewed", issueID, g.Plan, strings.Join(fails, "; "))
}

func (g PlanSelfReviewed) describe(issueID string) string {
	return fmt.Sprintf("%s's current plan (%s) has passed only its author's own review (%s); it needs a pass from a reviewer other than its author", issueID, g.Plan, g.Author)
}
