package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	now := e.clock.Now()
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
	// Placement runs before the record is committed to e.issues. place became
	// fallible when it started dispatching through orderEdgeFor, and a failure
	// after the map write would strand the record in e.issues while absent from
	// e.order — findable by GetIssue, hydrated through a missing pos key, and so
	// reported at a fabricated rank. place reads only e.order, so ordering the
	// two this way removes that state rather than unwinding it.
	// [LAW:polishing-by-subtraction]
	if err := e.place(id, in.Placement); err != nil {
		return model.Issue{}, err
	}
	e.issues[id] = rec
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
//
// Filing is scoped to the whole order, not to the issue's frame the way the
// rank verbs are: landing after everything that exists is also landing after
// every frame-mate, so the default satisfies the frame-local reading for free,
// and scoping it would drop a first child into the middle of the order
// instead. The remaining unscoped edge is RankTop, tracked on its own
// (links-rank-t2vl) because narrowing it changes what filing order
// means.
func (e *Engine) place(id string, placement storage.RankPlacement) error {
	if len(e.order) == 0 {
		e.order = append(e.order, id)
		return nil
	}
	// The population is every position in the order — the same edge dispatch the
	// rank verbs use, asked about the workspace instead of one frame.
	// [LAW:dataflow-not-control-flow]
	population := make([]int, len(e.order))
	for index := range e.order {
		population[index] = index
	}
	edge, err := orderEdgeFor(population, placement)
	if err != nil {
		return err
	}
	e.insertAt(edge.insertAt, id)
	return nil
}

// mintID names a new issue. Top-level and child ids differ only in the
// namespace they hang under and the population that sets their hash length;
// the minting rule itself is one function in issueid, reached the same way
// from both, so this engine and the Dolt store cannot drift apart on what an
// id is. [LAW:one-type-per-behavior]
func (e *Engine) mintID(prefix, topic, title, description string, createdAt time.Time, parentID string) (string, error) {
	content := issueid.Content{
		Topic:       topic,
		Title:       title,
		Description: description,
		Creator:     createdBy,
		CreatedAt:   createdAt,
	}
	namespace, population := e.idSpace(prefix, topic, parentID)
	return issueid.Mint(namespace, content, population, func(candidate string) (bool, error) {
		_, taken := e.issues[candidate]
		return taken, nil
	})
}

// idSpace resolves which id-space a new issue is minted into and how populated
// that space already is. The population sets a starting hash length and never a
// position: an id derived from a count over the LOCAL rows is a claim about
// every row that exists anywhere, and two disconnected stores holding the same
// rows make that claim identically. [LAW:one-source-of-truth]
func (e *Engine) idSpace(prefix, topic, parentID string) (issueid.Namespace, int) {
	if parentID == "" {
		return issueid.TopLevelNamespace(prefix, topic), e.topLevelCount()
	}
	return issueid.ChildNamespace(parentID), e.childCount(parentID)
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

// childCount counts the direct children already recorded under parentID.
// Parentage is read from the relation edges, the only place it lives — an id
// prefix scan would also sweep up grandchildren and would take the id shape as
// evidence of structure it does not own. [LAW:one-source-of-truth]
func (e *Engine) childCount(parentID string) int {
	count := 0
	for _, rel := range e.relations {
		if rel.Type == model.RelParentChild && rel.DstID == parentID {
			count++
		}
	}
	return count
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

// ListAllEvents returns the whole history, oldest first, ties broken by event
// id — the contract's ordering, on storage.IssueReader.ListAllEvents.
//
// The append-only slice already holds true recording order, which is a BETTER
// answer, and it is deliberately not the one given. A same-tick tie is exactly
// where the two engines would otherwise part company, and the differential
// oracle compares whole event lists: an engine that is right where the other
// is arbitrary reads as divergence, and the campaign would spend the signal it
// needs for real faults on a known, catalogued one. Happens-before ordering
// arrives with the event store's Lamport positions, for both engines at once.
func (e *Engine) ListAllEvents(ctx context.Context) ([]model.IssueEvent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return sortEvents(cloneEvents(e.events)), nil
}

// ListEvents is eventsFor behind the lock, for callers outside the engine.
// Callers already holding e.mu — the apply path — use eventsFor directly.
func (e *Engine) ListEvents(ctx context.Context, issueID string) ([]model.IssueEvent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.eventsFor(issueID), nil
}

// sortEvents imposes the contract's total order. It is the one place this
// engine orders history, so ListAllEvents and a single issue's history cannot
// drift, and the order it imposes is [storage.EventOrdering] rather than a
// second spelling of the same rule. [LAW:single-enforcer]
func sortEvents(events []model.IssueEvent) []model.IssueEvent {
	slices.SortStableFunc(events, storage.EventOrdering)
	return events
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
	return sortEvents(cloneEvents(out))
}
