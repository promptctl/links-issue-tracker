package memory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Apply is the single execution path for issue-record changes. The Change
// value carries all the variability — a nil Action is no transition, an empty
// patch is no field write — so the body below is one straight pipe with no
// verb-shaped branches in it. [LAW:dataflow-not-control-flow]
func (e *Engine) Apply(ctx context.Context, id string, c storage.Change) (model.Issue, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.apply(id, c)
}

func (e *Engine) apply(id string, c storage.Change) (model.Issue, error) {
	rec, err := e.mustRecord(id)
	if err != nil {
		return model.Issue{}, err
	}
	pos := e.positions()
	current, err := e.hydrate(rec, pos)
	if err != nil {
		return model.Issue{}, err
	}
	// [LAW:one-source-of-truth] One change has one author, normalized once and
	// recorded on every event the change produces.
	actor := strings.TrimSpace(c.Actor)
	if actor == "" {
		actor = "unknown"
	}
	now := e.now()

	// The field write baselines on the POST-action issue, so a start's new
	// assignee is what the patch diffs against rather than a second assignee
	// change row restating what the transition already recorded.
	afterAction, actionEvents, err := e.planLifecycle(current, actor, strings.TrimSpace(c.Reason), c.Action, now)
	if err != nil {
		return model.Issue{}, err
	}
	patch, err := planFields(afterAction, c.Fields, actor, now)
	if err != nil {
		return model.Issue{}, err
	}

	e.writeIssue(rec, patch.issue)
	e.writeLabels(rec.id, patch, now)
	e.recordEvents(rec.id, actionEvents, now)
	e.recordEvents(rec.id, patch.events, now)
	return e.hydrate(rec, e.positions())
}

// planLifecycle plans whichever axis the action drives. The two subsets
// partition the sealed action sum, so a retention action reaching the status
// machine — or the reverse — is unrepresentable rather than guarded against.
// A nil action is no transition at all, which the type carries as data.
// [LAW:types-are-the-program]
func (e *Engine) planLifecycle(current model.Issue, actor, reason string, action model.Action, now time.Time) (model.Issue, []eventSpec, error) {
	switch a := action.(type) {
	case nil:
		return current, nil, nil
	case model.StatusAction:
		return e.planStatus(current, actor, reason, a, now)
	case model.RetentionAction:
		return planRetention(current, actor, reason, a, now)
	default:
		// [LAW:no-silent-failure] The sealed sum has no third axis; only an
		// impostor Action reaches here.
		panic(fmt.Sprintf("illegal Action value %T", action))
	}
}

// planStatus walks the status state machine. Every rejection it can produce is
// the machine's own — frozen work is out of the flow, a container's state is
// its children's — and the engine adds only the assignee rule and the
// integrity floor under a redirecting close.
func (e *Engine) planStatus(current model.Issue, actor, reason string, action model.StatusAction, now time.Time) (model.Issue, []eventSpec, error) {
	if model.Frozen(current.Retention()) {
		return model.Issue{}, nil, fmt.Errorf("cannot %s archived or deleted issue", action.Name())
	}
	updated, err := current.Apply(action)
	if err != nil {
		return model.Issue{}, nil, err
	}
	// Start is the one variant that carries a new owner, so it is the one
	// transition that rewrites the assignee; every other one preserves
	// ownership, which is an issue-level field the status machine never
	// touches. [LAW:types-are-the-program] The payload comes from the variant,
	// not a loose parameter every other action would have to ignore.
	priorAssignee := current.Assignee
	postAssignee := priorAssignee
	if start, ok := action.(model.Start); ok {
		postAssignee = strings.TrimSpace(start.Assignee)
	}
	updated.Assignee = postAssignee
	// A call whose target state and resulting assignee both already hold is
	// the documented no-op: history records mutations that happened, not calls
	// that were made. A same-state start with a NEW assignee is the agent
	// reclaim path and falls through, recording the ownership change.
	//
	// The no-op is decided BEFORE the redirect target is checked, because the
	// check guards a write and there is no write here to guard. Checking first
	// would make a call that changes nothing fail on the state of an issue it
	// was never going to touch.
	if updated.StatusValue() == current.StatusValue() && postAssignee == priorAssignee {
		return current, nil, nil
	}
	if err := e.validateRedirectTarget(updated); err != nil {
		return model.Issue{}, nil, err
	}
	updated.UpdatedAt = now
	return updated, []eventSpec{{
		action:  string(action.Name()),
		reason:  reason,
		actor:   actor,
		changes: statusChanges(current, updated),
	}}, nil
}

// planRetention moves the retention axis. The legal moves and every rejection
// reason are the model's Retain transition table; this plan supplies the clock
// and the change rows. There is no same-state success cell — re-archiving an
// archived issue is a rejection, not a quiet no-op — so every planned
// retention move owes a write. [LAW:single-enforcer]
func planRetention(current model.Issue, actor, reason string, action model.RetentionAction, now time.Time) (model.Issue, []eventSpec, error) {
	next, err := model.Retain(current.Retention(), action, now)
	if err != nil {
		return model.Issue{}, nil, err
	}
	post := current
	post.SetRetention(next)
	post.UpdatedAt = now
	return post, []eventSpec{{
		action:  string(action.Name()),
		reason:  reason,
		actor:   actor,
		changes: retentionChanges(current.Retention(), next),
	}}, nil
}

// validateRedirectTarget is the integrity floor under a redirecting close: the
// canonical it points at must be named, must not be the closing issue, and
// must not itself be deleted — a redirect into the trash is a dangling pointer
// by design. Archived stays legal, because "duplicate of something already
// done" is the most common real redirect. A non-redirecting transition carries
// no target, so this is nothing there. [LAW:single-enforcer]
func (e *Engine) validateRedirectTarget(closing model.Issue) error {
	resolution := closing.ResolutionValue()
	target := closing.RedirectTargetValue()
	if target == nil {
		if resolution != nil && resolution.RedirectsToCanonical() {
			return fmt.Errorf("closing as %s requires a canonical target issue to redirect to", *resolution)
		}
		return nil
	}
	if *target == closing.ID {
		return fmt.Errorf("cannot redirect %s to itself", closing.ID)
	}
	canonical, err := e.mustRecord(*target)
	if err != nil {
		return err
	}
	if _, gone := canonical.retention.(model.Deleted); gone {
		return fmt.Errorf("cannot redirect %s to %s: the canonical issue is deleted", closing.ID, *target)
	}
	return nil
}

// fieldPatch is a planned field write: the post-patch issue, whether the patch
// stated the whole label set, and the event it owes.
//
// statesLabels is not the same question as "did the labels change". A patch
// that restates the set it already had rewrites the label rows — authorship
// and timestamps included — while a patch that never mentions labels leaves
// them exactly as some earlier writer left them.
type fieldPatch struct {
	issue        model.Issue
	statesLabels bool
	// actor authors the label rows a stated set writes, so the patch's one
	// author is the one this write records.
	actor  string
	events []eventSpec
}

// planFields computes the post-patch issue and the field-change rows it owes,
// as a pure function of (baseline, patch, actor): no clock beyond the stamp it
// is handed, no store, no writes. A nil pointer means "leave this alone",
// which is the whole reason the patch is pointers — and an empty patch plans
// no change at all without anyone testing for one.
func planFields(baseline model.Issue, in storage.UpdateIssueInput, actor string, now time.Time) (fieldPatch, error) {
	issue := baseline
	if in.Title != nil {
		issue.Title = strings.TrimSpace(*in.Title)
		if issue.Title == "" {
			return fieldPatch{}, errors.New("title cannot be empty")
		}
	}
	if in.Description != nil {
		issue.Description = strings.TrimSpace(*in.Description)
	}
	if in.Prompt != nil {
		issue.Prompt = strings.TrimSpace(*in.Prompt)
	}
	if in.IssueType != nil {
		// Container-vs-leaf is what decides which lifecycle expression backs
		// the issue, so switching across that line would orphan the one it
		// has: an epic turned leaf keeps a state derived from children it no
		// longer claims, and a leaf turned epic drops its status. Refuse it
		// here rather than inventing a default downstream.
		if issue.IssueType.IsContainer() != in.IssueType.IsContainer() {
			return fieldPatch{}, fmt.Errorf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerTypes())
		}
		issue.IssueType = *in.IssueType
	}
	if in.Priority != nil {
		issue.Priority = *in.Priority
	}
	if in.Assignee != nil {
		issue.Assignee = strings.TrimSpace(*in.Assignee)
	}
	if in.Lane != nil {
		issue.Lane = strings.TrimSpace(*in.Lane)
	}
	if in.Labels != nil {
		labels, err := canonicalLabels(*in.Labels)
		if err != nil {
			return fieldPatch{}, err
		}
		issue.Labels = labels
	}
	patch := fieldPatch{issue: issue, statesLabels: in.Labels != nil, actor: actor}
	changes := fieldChanges(baseline, issue)
	if len(changes) == 0 {
		return patch, nil
	}
	patch.issue.UpdatedAt = now
	patch.events = []eventSpec{{reason: in.Reason, actor: actor, changes: changes}}
	return patch, nil
}

// fieldChanges reports the field axis's change rows: one per field that
// actually moved, in a fixed order so two reads of one mutation describe it
// identically.
func fieldChanges(before, after model.Issue) []model.FieldChange {
	var changes []model.FieldChange
	appendIfMoved := func(field, from, to string) {
		if from != to {
			changes = append(changes, model.FieldChange{Field: field, From: from, To: to})
		}
	}
	appendIfMoved("title", before.Title, after.Title)
	appendIfMoved("description", before.Description, after.Description)
	appendIfMoved("issue_type", string(before.IssueType), string(after.IssueType))
	// The numeric wire encoding, not the display name: history keeps what the
	// field stores so a replay reads back the value that was written.
	appendIfMoved("priority", strconv.Itoa(int(before.Priority)), strconv.Itoa(int(after.Priority)))
	appendIfMoved("assignee", before.Assignee, after.Assignee)
	appendIfMoved("lane", before.Lane, after.Lane)
	appendIfMoved("labels", strings.Join(before.Labels, ","), strings.Join(after.Labels, ","))
	return changes
}

// statusChanges reports the status axis's change rows — the whole close
// payload, so that a reopen's clearing of it is as legible in history as the
// close that set it.
func statusChanges(before, after model.Issue) []model.FieldChange {
	var changes []model.FieldChange
	if before.StatusValue() != after.StatusValue() {
		changes = append(changes, model.FieldChange{Field: "status", From: before.StatusValue(), To: after.StatusValue()})
	}
	if !timesEqual(before.ClosedAtValue(), after.ClosedAtValue()) {
		changes = append(changes, model.FieldChange{
			Field: "closed_at", From: formatTime(before.ClosedAtValue()), To: formatTime(after.ClosedAtValue()),
		})
	}
	if !resolutionsEqual(before.ResolutionValue(), after.ResolutionValue()) {
		changes = append(changes, model.FieldChange{
			Field: "resolution", From: formatResolution(before.ResolutionValue()), To: formatResolution(after.ResolutionValue()),
		})
	}
	if !stringsEqual(before.RedirectTargetValue(), after.RedirectTargetValue()) {
		changes = append(changes, model.FieldChange{
			Field: "redirect_target", From: formatString(before.RedirectTargetValue()), To: formatString(after.RedirectTargetValue()),
		})
	}
	if before.Assignee != after.Assignee {
		changes = append(changes, model.FieldChange{Field: "assignee", From: before.Assignee, To: after.Assignee})
	}
	return changes
}

// retentionChanges reports the retention axis's change rows against the
// timestamp pair the axis projects to, so the history format is the same
// encoding an export carries.
func retentionChanges(before, after model.Retention) []model.FieldChange {
	priorArchived, priorDeleted := model.RetentionTimestamps(before)
	nextArchived, nextDeleted := model.RetentionTimestamps(after)
	var changes []model.FieldChange
	if !timesEqual(priorArchived, nextArchived) {
		changes = append(changes, model.FieldChange{Field: "archived_at", From: formatTime(priorArchived), To: formatTime(nextArchived)})
	}
	if !timesEqual(priorDeleted, nextDeleted) {
		changes = append(changes, model.FieldChange{Field: "deleted_at", From: formatTime(priorDeleted), To: formatTime(nextDeleted)})
	}
	return changes
}

// writeIssue lands a planned issue back on its record. It is the one place a
// mutation becomes stored state, which is what keeps every axis's plan from
// having to know how the record spells itself. [LAW:single-enforcer]
func (e *Engine) writeIssue(rec *record, issue model.Issue) {
	rec.title = issue.Title
	rec.description = issue.Description
	rec.prompt = issue.Prompt
	rec.issueType = issue.IssueType
	rec.priority = issue.Priority
	rec.assignee = issue.Assignee
	rec.lane = issue.Lane
	rec.updatedAt = issue.UpdatedAt
	rec.retention = issue.Retention()
	rec.status = statusViewOf(issue)
}

// writeLabels replaces the label set when — and only when — the patch stated
// it. An unstated set keeps its rows, so a title edit does not silently
// reassign authorship of every label on the issue.
func (e *Engine) writeLabels(issueID string, patch fieldPatch, now time.Time) {
	if !patch.statesLabels {
		return
	}
	e.setLabels(issueID, patch.issue.Labels, now, patch.actor)
}

// statusViewOf projects an issue's leaf status back to the stored view. A
// container has no status of its own, so it projects to the zero view — the
// same nothing the Dolt engine stores as a NULL column.
func statusViewOf(issue model.Issue) model.StatusView {
	if issue.IsContainer() {
		return model.StatusView{}
	}
	return model.StatusView{
		Value:          issue.State(),
		ClosedAt:       issue.ClosedAtValue(),
		Resolution:     issue.ResolutionValue(),
		RedirectTarget: issue.RedirectTargetValue(),
	}
}
