package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func (e *Engine) CreateIssue(ctx context.Context, in storage.CreateIssueInput) (model.Issue, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.createIssue(in)
}

// createIssue records a new issue and returns it as stored, id minted.
//
// The order of the checks below is contract, not housekeeping: the parent must
// be resolved before the cosmetic prefix is, so that naming a parent that is
// not there reports the missing issue rather than whatever the caller left out
// of a call it was never going to complete.
func (e *Engine) createIssue(in storage.CreateIssueInput) (model.Issue, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return model.Issue{}, errors.New("title is required")
	}
	labels, err := canonicalLabels(in.Labels)
	if err != nil {
		return model.Issue{}, err
	}
	topic, err := issueid.NormalizeTopicForCreate(in.Topic)
	if err != nil {
		return model.Issue{}, err
	}
	// [LAW:dataflow-not-control-flow] The zero value is data meaning
	// "unspecified"; resolving it to the product default here keeps every
	// caller's input flowing through the one construction below.
	issueType := in.IssueType
	if issueType == "" {
		issueType = model.TypeTask
	}
	parentID := strings.TrimSpace(in.ParentID)
	if parentID != "" {
		if _, err := e.mustRecord(parentID); err != nil {
			return model.Issue{}, err
		}
	}
	prefix, err := issueid.NormalizeConfiguredPrefix(in.Prefix)
	if err != nil {
		return model.Issue{}, fmt.Errorf("normalize issue prefix: %w", err)
	}
	now := e.now()
	id, err := e.mintID(prefix, topic, title, strings.TrimSpace(in.Description), now, parentID)
	if err != nil {
		return model.Issue{}, err
	}

	rec := &record{
		id:          id,
		title:       title,
		description: strings.TrimSpace(in.Description),
		prompt:      strings.TrimSpace(in.Prompt),
		issueType:   issueType,
		topic:       topic,
		assignee:    strings.TrimSpace(in.Assignee),
		lane:        strings.TrimSpace(in.Lane),
		priority:    in.Priority,
		createdAt:   now,
		updatedAt:   now,
		status:      model.StatusView{Value: model.StateOpen},
		retention:   model.Live{},
	}
	e.issues[id] = rec
	e.place(id, in.Placement)
	e.setLabels(id, labels, now, createdBy)
	if parentID != "" {
		e.relations = append(e.relations, model.Relation{
			SrcID: id, DstID: parentID, Type: model.RelParentChild, CreatedAt: now, CreatedBy: createdBy,
		})
	}
	// The create event records the initial status as one field-change row. A
	// container has no status of its own to record, so it records none.
	changes := []model.FieldChange{}
	if !issueType.IsContainer() {
		changes = append(changes, model.FieldChange{Field: "status", From: "", To: string(model.StateOpen)})
	}
	e.recordEvent(id, eventSpec{action: "created", reason: "issue created", actor: createdBy, changes: changes}, now)

	return e.hydrate(rec, e.positions())
}

// place files a newly created issue in the rank order. RankBottom is the zero
// value, so a create that says nothing about placement appends — which is what
// keeps an authored batch in the order its file states it.
func (e *Engine) place(id string, placement storage.RankPlacement) {
	if placement == storage.RankTop {
		e.order = append([]string{id}, e.order...)
		return
	}
	e.order = append(e.order, id)
}

// mintID names a new issue. A child is numbered under its parent so the id
// carries the structure a reader reads it for; a top-level issue gets a
// content hash long enough that a collision at this store's size is unlikely,
// retried at increasing length until one is free.
func (e *Engine) mintID(prefix, topic, title, description string, createdAt time.Time, parentID string) (string, error) {
	if parentID != "" {
		return e.nextChildID(parentID), nil
	}
	baseLength := min(issueid.ComputeAdaptiveLength(e.topLevelCount()), issueid.MaxHashLength)
	for length := baseLength; length <= issueid.MaxHashLength; length++ {
		for nonce := 0; nonce < issueid.NonceAttempts; nonce++ {
			candidate := issueid.GenerateHashID(prefix, topic, title, description, createdBy, createdAt, length, nonce)
			if _, taken := e.issues[candidate]; !taken {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("generate unique issue id: exhausted lengths %d-%d", baseLength, issueid.MaxHashLength)
}

func (e *Engine) topLevelCount() int {
	count := 0
	for id := range e.issues {
		if !strings.Contains(id, ".") {
			count++
		}
	}
	return count
}

// nextChildID numbers a child one past the highest direct child of parentID.
// Only direct children count: a grandchild's id carries a further dot, and
// counting it would collide the next sibling with an existing branch.
func (e *Engine) nextChildID(parentID string) string {
	highest := 0
	for id := range e.issues {
		suffix, ok := strings.CutPrefix(id, parentID+".")
		if !ok || suffix == "" || strings.Contains(suffix, ".") {
			continue
		}
		number, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		highest = max(highest, number)
	}
	return fmt.Sprintf("%s.%d", parentID, highest+1)
}

func (e *Engine) GetIssue(ctx context.Context, id string) (model.Issue, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.getIssue(id)
}

func (e *Engine) getIssue(id string) (model.Issue, error) {
	rec, err := e.mustRecord(id)
	if err != nil {
		return model.Issue{}, err
	}
	return e.hydrate(rec, e.positions())
}

func (e *Engine) GetIssueDetail(ctx context.Context, id string) (model.IssueDetail, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	issue, err := e.getIssue(id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	pos := e.positions()
	relations := e.incidentRelations(id)
	structural, err := e.bucketRelations(id, relations, pos)
	if err != nil {
		return model.IssueDetail{}, err
	}
	related, err := e.relatedIssues(id, relations, pos)
	if err != nil {
		return model.IssueDetail{}, err
	}
	// Siblings are the parent's other children — derived from the same
	// rank-ordered child set every other consumer reads, minus self, so an
	// only child yields the empty group rather than a special case.
	siblings := []model.Issue{}
	if structural.Parent != nil {
		children, err := e.hydrateAll(e.childRecords(structural.Parent.ID, pos), pos)
		if err != nil {
			return model.IssueDetail{}, err
		}
		for _, child := range children {
			if child.ID != id {
				siblings = append(siblings, child)
			}
		}
	}
	// The redirect target hydrates from the issue's own close payload, never
	// from the relations graph: a related-to edge means exactly one thing
	// (a manual peer link), so the two render as two facts.
	var redirectTarget *model.Issue
	if target := issue.RedirectTargetValue(); target != nil {
		if rec, ok := e.issues[*target]; ok {
			hydrated, err := e.hydrate(rec, pos)
			if err != nil {
				return model.IssueDetail{}, err
			}
			redirectTarget = &hydrated
		}
	}
	return model.IssueDetail{
		Issue:          issue,
		Relations:      relations,
		Comments:       e.commentsFor(id),
		Events:         e.eventsFor(id),
		Children:       structural.Children,
		Siblings:       siblings,
		DependsOn:      structural.DependsOn,
		Blocks:         structural.Blocks,
		Parent:         structural.Parent,
		Related:        related,
		RedirectTarget: redirectTarget,
	}, nil
}

func (e *Engine) ListChildren(ctx context.Context, parentID string) ([]model.Issue, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.mustRecord(parentID); err != nil {
		return nil, err
	}
	pos := e.positions()
	return e.hydrateAll(e.childRecords(parentID, pos), pos)
}

func (e *Engine) ListTopics(ctx context.Context) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	seen := map[string]struct{}{}
	topics := []string{}
	for _, rec := range e.issues {
		// Deletion takes an issue's topic out of the vocabulary with it;
		// archival does not, because archived work is still work that was
		// filed under that topic.
		if _, gone := rec.retention.(model.Deleted); gone || rec.topic == "" {
			continue
		}
		if _, dup := seen[rec.topic]; dup {
			continue
		}
		seen[rec.topic] = struct{}{}
		topics = append(topics, rec.topic)
	}
	slices.Sort(topics)
	return topics, nil
}

// ListAllEvents returns the whole history, oldest first.
//
// It is the recording order rather than a timestamp sort: the events slice is
// append-only, so the order it holds IS the order things happened, and no
// clock coarse enough to stamp two mutations identically can scramble it.
func (e *Engine) ListAllEvents(ctx context.Context) ([]model.IssueEvent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneEvents(e.events), nil
}

// LocalIssueCount reports how many issues this store holds — the adopt-safety
// signal, so it counts what would be lost rather than what is in the flow.
func (e *Engine) LocalIssueCount(ctx context.Context) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return int64(len(e.issues)), nil
}

func (e *Engine) eventsFor(issueID string) []model.IssueEvent {
	out := []model.IssueEvent{}
	for _, event := range e.events {
		if event.IssueID == issueID {
			out = append(out, event)
		}
	}
	return cloneEvents(out)
}
