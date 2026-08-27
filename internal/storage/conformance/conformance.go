// Package conformance is the executable meaning of lit's storage contract.
//
// [storage.Store] declares shapes; shapes are not a contract. What an engine
// actually owes lit is behavior — that a missing id is NotFoundError and not a
// zero value, that an unsorted listing comes back rank-ordered, that "above Y"
// puts the issue above Y. Those facts live here, once, parameterized over "give
// me a fresh engine," so every implementation is held to the same statements
// rather than to its own test suite's idea of them.
//
// # How to use it
//
// An engine's package writes one test:
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, func(t *testing.T) storage.Store { return openMyEngine(t) })
//	}
//
// The engine package supplies the factory, so this package imports no engine
// and an engine's construction — migrations, temp dirs, cleanup — stays that
// engine's business. [LAW:one-way-deps]
//
// # What it may and may not assert
//
// Only what a caller can observe through the contract. No case may reach past
// the interface into an engine's internals, because a case that did would be
// testing one implementation while claiming to define the contract, and the
// next engine would have to grow the internal to pass. [LAW:behavior-not-structure]
//
// While the Dolt engine is lit's only implementation, its behavior IS the
// specification: where a behavior was ambiguous, these cases were written to
// record what Dolt does rather than what would be tidier, because the S0
// migration state's whole gate is that nothing observable changes.
package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// NewEngine mints a fresh, empty engine for one case, already registered for
// cleanup with t. Every case gets its own: a suite whose cases shared a store
// would be pinning the order they run in as much as the contract.
type NewEngine func(t *testing.T) storage.Store

// Run executes the whole suite against engines newEngine produces.
func Run(t *testing.T, newEngine NewEngine) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, context.Background(), newEngine(t))
		})
	}
}

// engineCase is one behavioral statement about the contract.
// [LAW:dataflow-not-control-flow] The suite is a table walked by one loop, so
// adding a statement is adding data — never another arm in Run.
type engineCase struct {
	name string
	run  func(t *testing.T, ctx context.Context, st storage.Store)
}

// prefix is the cosmetic id prefix every case creates under. Cases assert on
// ids only by comparing ids the engine returned, never by predicting their
// shape — id minting is engine business.
const prefix = "conf"

var cases = []engineCase{
	{"create_read_roundtrip", createReadRoundtrip},
	{"create_defaults", createDefaults},
	{"create_requires_title", createRequiresTitle},
	{"create_normalizes_topic", createNormalizesTopic},
	{"create_under_missing_parent_is_not_found", createUnderMissingParent},
	{"get_missing_issue_is_not_found", getMissingIssue},
	{"apply_field_patch", applyFieldPatch},
	{"apply_status_transition", applyStatusTransition},
	{"apply_missing_issue_is_not_found", applyMissingIssue},
	{"apply_to_container_is_refused", applyToContainer},
	{"container_state_follows_live_children", containerStateFollowsLiveChildren},
	{"history_records_mutations", historyRecordsMutations},
	{"list_defaults_to_rank_order", listDefaultsToRankOrder},
	{"list_filters_select", listFiltersSelect},
	{"list_hides_archived_and_deleted", listHidesArchivedAndDeleted},
	{"list_sorts_and_limits", listSortsAndLimits},
	{"rank_intents_reorder", rankIntentsReorder},
	{"rank_intents_resolve_across_frames", rankIntentsResolveAcrossFrames},
	{"rank_set_imposes_order", rankSetImposesOrder},
	{"close_redirects_to_a_canonical", closeRedirectsToCanonical},
	{"comments_roundtrip", commentsRoundtrip},
	{"labels_roundtrip", labelsRoundtrip},
	{"relations_roundtrip", relationsRoundtrip},
	{"relations_batch_buckets_edges", relationsBatchBucketsEdges},
	{"parent_wiring", parentWiring},
	{"topics_derive_from_issues", topicsDeriveFromIssues},
	{"export_carries_whole_store", exportCarriesWholeStore},
	{"bulk_apply_creates_and_updates", bulkApplyCreatesAndUpdates},
	{"bulk_apply_compensates_a_failed_batch", bulkApplyCompensatesFailure},
	{"import_tree_maps_local_ids", importTreeMapsLocalIDs},
	{"attribution_stamps_events", attributionStampsEvents},
	{"local_issue_count_tracks_creates", localIssueCountTracksCreates},
}

func createReadRoundtrip(t *testing.T, ctx context.Context, st storage.Store) {
	created := mustCreate(t, ctx, st, storage.CreateIssueInput{
		Title:       "  Renderer cleanup  ",
		Description: "  drop the legacy pass  ",
		Prompt:      "  do the thing  ",
		IssueType:   model.TypeBug,
		Topic:       "renderer",
		Priority:    model.PriorityUrgent,
		Assignee:    "ada",
		Lane:        "left",
		Labels:      []string{"perf"},
	})

	// Surrounding whitespace is stripped on the way in, so what a reader sees
	// is what a subsequent search or exact-match filter can find.
	if created.Title != "Renderer cleanup" {
		t.Errorf("Title = %q, want the trimmed title", created.Title)
	}
	if created.Description != "drop the legacy pass" {
		t.Errorf("Description = %q, want trimmed", created.Description)
	}
	if created.Prompt != "do the thing" {
		t.Errorf("Prompt = %q, want trimmed", created.Prompt)
	}

	read, err := st.GetIssue(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssue(%q) error = %v", created.ID, err)
	}
	// A read is the same record, not a similar one: an engine that dropped or
	// re-derived a field on the way back out would be losing authored data.
	for _, field := range []struct {
		name      string
		got, want string
	}{
		{"ID", read.ID, created.ID},
		{"Title", read.Title, created.Title},
		{"Description", read.Description, created.Description},
		{"Prompt", read.Prompt, created.Prompt},
		{"Topic", read.Topic, created.Topic},
		{"Assignee", read.Assignee, created.Assignee},
		{"Lane", read.Lane, created.Lane},
		{"IssueType", string(read.IssueType), string(model.TypeBug)},
		{"State", string(read.State()), string(model.StateOpen)},
	} {
		if field.got != field.want {
			t.Errorf("GetIssue %s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if read.Priority != model.PriorityUrgent {
		t.Errorf("Priority = %v, want urgent", read.Priority)
	}
	if strings.Join(read.Labels, ",") != "perf" {
		t.Errorf("Labels = %v, want [perf]", read.Labels)
	}
	if read.Rank == "" {
		t.Error("Rank is empty; every issue must land somewhere in the order")
	}
}

func createDefaults(t *testing.T, ctx context.Context, st storage.Store) {
	// Saying nothing about type or placement must reach the product defaults,
	// since that is the path every non-interactive creation surface takes.
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "unspecified", Topic: "core"})
	if issue.IssueType != model.TypeTask {
		t.Errorf("IssueType = %q, want task for an unspecified type", issue.IssueType)
	}
	if issue.State() != model.StateOpen {
		t.Errorf("State = %q, want open", issue.State())
	}
	if issue.Priority != model.PriorityNormal {
		t.Errorf("Priority = %v, want normal", issue.Priority)
	}

	// RankBottom is the zero value, so a second create files below the first.
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second", Topic: "core"})
	assertOrder(t, ctx, st, "default placement appends", issue.ID, second.ID)

	top := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "top", Topic: "core", Placement: storage.RankTop})
	assertOrder(t, ctx, st, "RankTop leads", top.ID, issue.ID, second.ID)
}

func createRequiresTitle(t *testing.T, ctx context.Context, st storage.Store) {
	// Whitespace is not a title: the trim happens before the requirement, so a
	// blank-looking title cannot slip in as a non-empty string.
	for _, title := range []string{"", "   "} {
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: title, Topic: "core"}); err == nil {
			t.Errorf("CreateIssue(title=%q) succeeded; want an error", title)
		}
	}
}

// createNormalizesTopic pins the topic as already-canonical by the time it is
// stored. The vocabulary is DERIVED from what issues carry, and a topic that
// reached storage in one spelling could never be found under another — so
// normalizing is the store's job, not each caller's, and a name too short to
// be one is refused rather than stored. [LAW:parse-dont-validate]
func createNormalizesTopic(t *testing.T, ctx context.Context, st storage.Store) {
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "shaped", Topic: "  Renderer Cleanup  "})
	if issue.Topic != "renderer-cleanup" {
		t.Errorf("Topic = %q, want the normalized renderer-cleanup", issue.Topic)
	}
	topics, err := st.ListTopics(ctx)
	if err != nil {
		t.Fatalf("ListTopics error = %v", err)
	}
	assertStrings(t, "topics after a normalized create", topics, []string{"renderer-cleanup"})

	for _, topic := range []string{"", "   ", "ab", "-!-"} {
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "unnamed", Topic: topic, Prefix: prefix}); err == nil {
			t.Errorf("CreateIssue(topic=%q) succeeded; want an error", topic)
		}
	}
}

func createUnderMissingParent(t *testing.T, ctx context.Context, st storage.Store) {
	_, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "orphan", Topic: "core", ParentID: "no-such-issue"})
	assertNotFound(t, err, "issue", "CreateIssue under a missing parent")
}

func getMissingIssue(t *testing.T, ctx context.Context, st storage.Store) {
	_, err := st.GetIssue(ctx, "no-such-issue")
	assertNotFound(t, err, "issue", "GetIssue")

	_, err = st.GetIssueDetail(ctx, "no-such-issue")
	assertNotFound(t, err, "issue", "GetIssueDetail")
}

func applyFieldPatch(t *testing.T, ctx context.Context, st storage.Store) {
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "before", Topic: "core", Assignee: "ada"})

	title := "after"
	priority := model.PriorityUrgent
	updated, err := st.Apply(ctx, issue.ID, storage.Change{
		Actor:  "grace",
		Fields: storage.UpdateIssueInput{Title: &title, Priority: &priority, Reason: "sharpened"},
	})
	if err != nil {
		t.Fatalf("Apply field patch error = %v", err)
	}
	if updated.Title != "after" || updated.Priority != model.PriorityUrgent {
		t.Errorf("Apply returned %q/%v, want after/urgent", updated.Title, updated.Priority)
	}
	// A nil pointer means "leave alone", so an untouched field survives a patch
	// that never mentions it. This is the whole reason the patch is pointers.
	if updated.Assignee != "ada" {
		t.Errorf("Assignee = %q, want the untouched ada", updated.Assignee)
	}

	read, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue after patch error = %v", err)
	}
	if read.Title != "after" {
		t.Errorf("patch did not persist: Title = %q", read.Title)
	}
}

func applyStatusTransition(t *testing.T, ctx context.Context, st storage.Store) {
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "work", Topic: "core"})

	started, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"})
	if err != nil {
		t.Fatalf("Apply start error = %v", err)
	}
	if started.State() != model.StateInProgress {
		t.Errorf("State after start = %q, want in_progress", started.State())
	}
	// Start is the one action that rewrites ownership, which is why it is the
	// one variant carrying an assignee.
	if started.Assignee != "ada" {
		t.Errorf("Assignee after start = %q, want ada", started.Assignee)
	}

	done, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Done{}, Actor: "ada"})
	if err != nil {
		t.Fatalf("Apply done error = %v", err)
	}
	if done.State() != model.StateClosed {
		t.Errorf("State after done = %q, want closed", done.State())
	}

	reopened, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Reopen{}, Actor: "ada"})
	if err != nil {
		t.Fatalf("Apply reopen error = %v", err)
	}
	if reopened.State() != model.StateOpen {
		t.Errorf("State after reopen = %q, want open", reopened.State())
	}
}

func applyMissingIssue(t *testing.T, ctx context.Context, st storage.Store) {
	_, err := st.Apply(ctx, "no-such-issue", storage.Change{Action: model.Start{}, Actor: "ada"})
	assertNotFound(t, err, "issue", "Apply")
}

func applyToContainer(t *testing.T, ctx context.Context, st storage.Store) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})

	// An epic with children has no status of its own — its state is derived
	// from theirs — so acting on it directly is refused with an error the CLI
	// dispatches on, never quietly ignored.
	_, err := st.Apply(ctx, epic.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"})
	var containerErr model.ContainerActionError
	if !errors.As(err, &containerErr) {
		t.Fatalf("Apply to a container error = %v, want ContainerActionError", err)
	}
	if containerErr.ID != epic.ID {
		t.Errorf("ContainerActionError.ID = %q, want %q", containerErr.ID, epic.ID)
	}
}

// containerStateFollowsLiveChildren pins the one rule that decides what an
// epic's derived state is about. A container in the flow reads only children
// in the flow, so archiving the last unfinished child finishes the epic; a
// container that is itself out of the flow keeps its whole child set, so an
// archived epic stays the state it had when it left instead of collapsing.
func containerStateFollowsLiveChildren(t *testing.T, ctx context.Context, st storage.Store) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	finished := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "finished", Topic: "core", ParentID: epic.ID})
	unfinished := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "unfinished", Topic: "core", ParentID: epic.ID})

	if _, err := st.Apply(ctx, finished.ID, storage.Change{Action: model.Done{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply done error = %v", err)
	}
	assertState(t, ctx, st, epic.ID, model.StateInProgress, "one of two children done")

	// Archiving the unfinished child takes it out of the epic's reading, so
	// what is left is all done.
	if _, err := st.Apply(ctx, unfinished.ID, storage.Change{Action: model.Archive{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply archive child error = %v", err)
	}
	assertState(t, ctx, st, epic.ID, model.StateClosed, "the only live child is done")

	// Archiving the epic freezes its reading: every child counts again, so
	// the state it leaves with is the state it had.
	if _, err := st.Apply(ctx, epic.ID, storage.Change{Action: model.Archive{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply archive epic error = %v", err)
	}
	assertState(t, ctx, st, epic.ID, model.StateInProgress, "an archived epic keeps its whole child set")
}

func historyRecordsMutations(t *testing.T, ctx context.Context, st storage.Store) {
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "tracked", Topic: "core"})
	title := "retitled"
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Actor: "ada", Fields: storage.UpdateIssueInput{Title: &title}}); err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	// A pure no-op writes no history: the log records mutations that happened,
	// not calls that were made.
	before := len(mustEvents(t, ctx, st))
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Actor: "ada"}); err != nil {
		t.Fatalf("Apply no-op error = %v", err)
	}
	after := mustEvents(t, ctx, st)
	if len(after) != before {
		t.Errorf("no-op Apply wrote %d events, want none", len(after)-before)
	}

	// A status action whose target state AND resulting assignee already hold
	// is the same no-op: the ticket did not move, so the log does not say it
	// did. A same-state start naming a NEW owner is the reclaim path and does
	// record, which is what keeps "who took this over" answerable.
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply start error = %v", err)
	}
	before = len(mustEvents(t, ctx, st))
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply repeated start error = %v", err)
	}
	if repeated := len(mustEvents(t, ctx, st)); repeated != before {
		t.Errorf("a start that changed nothing wrote %d events, want none", repeated-before)
	}
	reclaimed, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "grace"}, Actor: "grace"})
	if err != nil {
		t.Fatalf("Apply reclaim error = %v", err)
	}
	if reclaimed.Assignee != "grace" {
		t.Errorf("Assignee after reclaim = %q, want grace", reclaimed.Assignee)
	}
	if len(mustEvents(t, ctx, st)) == before {
		t.Error("a reclaim wrote no event; the ownership change is the audit substrate")
	}

	after = mustEvents(t, ctx, st)
	var sawRetitle bool
	for _, e := range after {
		if e.IssueID != issue.ID {
			t.Errorf("event for unexpected issue %q", e.IssueID)
		}
		for _, c := range e.Changes {
			if c.Field == "title" {
				sawRetitle = true
			}
		}
	}
	if !sawRetitle {
		t.Error("history has no title change; the field write recorded nothing")
	}
	// Oldest first is the contract, because every consumer folds it forward.
	for i := 1; i < len(after); i++ {
		if after[i].CreatedAt.Before(after[i-1].CreatedAt) {
			t.Fatalf("events are not oldest-first at index %d", i)
		}
	}
}

func listDefaultsToRankOrder(t *testing.T, ctx context.Context, st storage.Store) {
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first", Topic: "core"})
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second", Topic: "core"})
	third := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "third", Topic: "core"})

	// An unsorted listing is reproducible, not merely arbitrary: rank ascending
	// with ties broken by id. Callers rely on it — `lit backlog` is this order.
	assertOrder(t, ctx, st, "default listing", first.ID, second.ID, third.ID)
}

func listFiltersSelect(t *testing.T, ctx context.Context, st storage.Store) {
	bug := mustCreate(t, ctx, st, storage.CreateIssueInput{
		Title: "alpha widget", Topic: "renderer", IssueType: model.TypeBug, Assignee: "ada", Labels: []string{"perf", "ui"},
	})
	task := mustCreate(t, ctx, st, storage.CreateIssueInput{
		Title: "beta gadget", Topic: "parser", IssueType: model.TypeTask, Assignee: "grace",
	})
	if _, err := st.Apply(ctx, task.ID, storage.Change{Action: model.Start{Assignee: "grace"}, Actor: "grace"}); err != nil {
		t.Fatalf("Apply start error = %v", err)
	}
	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: bug.ID, Body: "note", CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddComment error = %v", err)
	}

	hasComments := true
	future := time.Now().UTC().Add(time.Hour)
	past := time.Now().UTC().Add(-time.Hour)
	for _, tc := range []struct {
		name   string
		filter storage.ListIssuesFilter
		want   []string
	}{
		{"by status", storage.ListIssuesFilter{Statuses: []model.State{model.StateInProgress}}, []string{task.ID}},
		{"by type", storage.ListIssuesFilter{IssueTypes: []model.IssueType{model.TypeBug}}, []string{bug.ID}},
		{"excluding a type", storage.ListIssuesFilter{ExcludeIssueTypes: []model.IssueType{model.TypeBug}}, []string{task.ID}},
		{"by assignee", storage.ListIssuesFilter{Assignees: []string{"grace"}}, []string{task.ID}},
		{"by id", storage.ListIssuesFilter{IDs: []string{bug.ID}}, []string{bug.ID}},
		{"by search term", storage.ListIssuesFilter{SearchTerms: []string{"widget"}}, []string{bug.ID}},
		{"search matches topic too", storage.ListIssuesFilter{SearchTerms: []string{"parser"}}, []string{task.ID}},
		{"by all labels", storage.ListIssuesFilter{LabelsAll: []string{"perf", "ui"}}, []string{bug.ID}},
		{"by a label the issue lacks", storage.ListIssuesFilter{LabelsAll: []string{"perf", "absent"}}, nil},
		{"by comment presence", storage.ListIssuesFilter{HasComments: &hasComments}, []string{bug.ID}},
		{"updated before the future", storage.ListIssuesFilter{UpdatedBefore: &future}, []string{bug.ID, task.ID}},
		{"updated after the future", storage.ListIssuesFilter{UpdatedAfter: &future}, nil},
		{"updated after the past", storage.ListIssuesFilter{UpdatedAfter: &past}, []string{bug.ID, task.ID}},
		// Criteria are ANDed across axes: a filter naming two axes selects the
		// intersection, so no caller can widen a listing by adding a criterion.
		{"two axes intersect", storage.ListIssuesFilter{IssueTypes: []model.IssueType{model.TypeBug}, Assignees: []string{"grace"}}, nil},
		// A slice is ORed within itself.
		{"one axis unions", storage.ListIssuesFilter{Assignees: []string{"ada", "grace"}}, []string{bug.ID, task.ID}},
	} {
		got := mustList(t, ctx, st, tc.filter)
		assertIssueIDs(t, tc.name, got, tc.want)
	}
}

func listHidesArchivedAndDeleted(t *testing.T, ctx context.Context, st storage.Store) {
	live := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "live", Topic: "core"})
	archived := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "archived", Topic: "core"})
	deleted := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "deleted", Topic: "core"})
	if _, err := st.Apply(ctx, archived.ID, storage.Change{Action: model.Archive{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply archive error = %v", err)
	}
	if _, err := st.Apply(ctx, deleted.ID, storage.Change{Action: model.Delete{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply delete error = %v", err)
	}

	// Retention is the one axis whose default is a filter rather than an
	// absence: saying nothing means "only what is in the flow".
	assertIssueIDs(t, "default listing", mustList(t, ctx, st, storage.ListIssuesFilter{}), []string{live.ID})
	assertIssueIDs(t, "including archived",
		mustList(t, ctx, st, storage.ListIssuesFilter{IncludeArchived: true}), []string{live.ID, archived.ID})
	assertIssueIDs(t, "including deleted",
		mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true}), []string{live.ID, deleted.ID})
	assertIssueIDs(t, "including both",
		mustList(t, ctx, st, storage.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true}),
		[]string{live.ID, archived.ID, deleted.ID})
}

func listSortsAndLimits(t *testing.T, ctx context.Context, st storage.Store) {
	b := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "b", Topic: "core"})
	a := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "a", Topic: "core"})
	c := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c", Topic: "core"})

	assertIssueIDs(t, "sorted by title ascending",
		mustList(t, ctx, st, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "title"}}}),
		[]string{a.ID, b.ID, c.ID})
	assertIssueIDs(t, "sorted by title descending",
		mustList(t, ctx, st, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "title", Desc: true}}}),
		[]string{c.ID, b.ID, a.ID})

	// Limit truncates the ordered result rather than sampling it, so a limited
	// listing is the head of the unlimited one.
	assertIssueIDs(t, "limited to two", mustList(t, ctx, st, storage.ListIssuesFilter{Limit: 2}), []string{b.ID, a.ID})
	// Zero is not a limit of zero; it is the absence of a limit.
	assertIssueIDs(t, "unlimited", mustList(t, ctx, st, storage.ListIssuesFilter{Limit: 0}), []string{b.ID, a.ID, c.ID})

	if _, err := st.ListIssues(ctx, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "nonsense"}}}); err == nil {
		t.Error("sorting by an unknown field succeeded; want an error")
	}
}

func rankIntentsReorder(t *testing.T, ctx context.Context, st storage.Store) {
	a := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "a", Topic: "core"})
	b := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "b", Topic: "core"})
	c := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c", Topic: "core"})
	assertOrder(t, ctx, st, "as created", a.ID, b.ID, c.ID)

	move, err := st.RankAbove(ctx, c.ID, a.ID)
	if err != nil {
		t.Fatalf("RankAbove error = %v", err)
	}
	// Same-frame issues are ranked as named, so the report is the inputs
	// unchanged; it differs only when a frame forced a substitution.
	if move.MovedID != c.ID || move.AnchorID != a.ID {
		t.Errorf("RankAbove reported %+v, want moved=%s anchor=%s", move, c.ID, a.ID)
	}
	assertOrder(t, ctx, st, "after RankAbove", c.ID, a.ID, b.ID)

	if _, err := st.RankBelow(ctx, c.ID, b.ID); err != nil {
		t.Fatalf("RankBelow error = %v", err)
	}
	assertOrder(t, ctx, st, "after RankBelow", a.ID, b.ID, c.ID)

	if err := st.RankToTop(ctx, b.ID); err != nil {
		t.Fatalf("RankToTop error = %v", err)
	}
	assertOrder(t, ctx, st, "after RankToTop", b.ID, a.ID, c.ID)

	if err := st.RankToBottom(ctx, b.ID); err != nil {
		t.Fatalf("RankToBottom error = %v", err)
	}
	assertOrder(t, ctx, st, "after RankToBottom", a.ID, c.ID, b.ID)

	// An intent naming an issue that is not there is an error, never a
	// silently-dropped reorder.
	if _, err := st.RankAbove(ctx, a.ID, "no-such-issue"); err == nil {
		t.Error("RankAbove a missing anchor succeeded; want an error")
	}
	if err := st.RankToTop(ctx, "no-such-issue"); err == nil {
		t.Error("RankToTop of a missing issue succeeded; want an error")
	}
}

// rankIntentsResolveAcrossFrames is why the anchored verbs report a RankMove
// at all. Rank meaning is frame-local — an issue's position is only ever read
// against its frame-mates — so an intent naming two issues from different
// frames is honored against the containing ancestors that ARE comparable, and
// the substitution comes back so the caller can say so.
func rankIntentsResolveAcrossFrames(t *testing.T, ctx context.Context, st storage.Store) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})
	standalone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "standalone", Topic: "core"})

	// "put the child above the standalone" is a request about the epic: the
	// child has no position the standalone can be compared against.
	move, err := st.RankAbove(ctx, child.ID, standalone.ID)
	if err != nil {
		t.Fatalf("RankAbove across frames error = %v", err)
	}
	if move.MovedID != epic.ID || move.AnchorID != standalone.ID {
		t.Errorf("RankAbove reported %+v, want moved=%s anchor=%s", move, epic.ID, standalone.ID)
	}
	// Nothing inside the epic was reordered, and the epic now precedes the
	// standalone.
	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertPrecedes(t, listed, epic.ID, standalone.ID)

	// An issue and its own container share no comparable frame, so there is no
	// order between them to write — and saying so beats writing one of the two
	// orders that could be meant. [LAW:no-silent-failure]
	if _, err := st.RankAbove(ctx, child.ID, epic.ID); err == nil {
		t.Error("RankAbove against its own container succeeded; want an error")
	}
	if _, err := st.RankBelow(ctx, epic.ID, child.ID); err == nil {
		t.Error("RankBelow against its own child succeeded; want an error")
	}
}

func rankSetImposesOrder(t *testing.T, ctx context.Context, st storage.Store) {
	a := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "a", Topic: "core"})
	b := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "b", Topic: "core"})
	c := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c", Topic: "core"})

	resolutions, err := st.RankSet(ctx, []string{c.ID, a.ID, b.ID})
	if err != nil {
		t.Fatalf("RankSet error = %v", err)
	}
	assertOrder(t, ctx, st, "after RankSet", c.ID, a.ID, b.ID)

	// Every named id is accounted for, in the order named, so a caller can
	// report substitutions without matching up two lists itself.
	if len(resolutions) != 3 {
		t.Fatalf("RankSet returned %d resolutions, want 3", len(resolutions))
	}
	for i, want := range []string{c.ID, a.ID, b.ID} {
		if resolutions[i].NamedID != want {
			t.Errorf("resolution %d NamedID = %q, want %q", i, resolutions[i].NamedID, want)
		}
		if resolutions[i].RankedID != want {
			t.Errorf("resolution %d RankedID = %q, want the same frame-mate %q", i, resolutions[i].RankedID, want)
		}
	}
}

// closeRedirectsToCanonical pins the close payload as a whole: a redirecting
// outcome records both the resolution and the ticket the work moved to, a
// reopen clears all of it at once, and a redirect to an issue that is not
// there is refused rather than stored as a dangling pointer.
func closeRedirectsToCanonical(t *testing.T, ctx context.Context, st storage.Store) {
	canonical := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "canonical", Topic: "core"})
	duplicate := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "duplicate", Topic: "core"})

	closed, err := st.Apply(ctx, duplicate.ID, storage.Change{
		Action: model.Close{Outcome: model.Duplicate{Of: canonical.ID}}, Actor: "ada",
	})
	if err != nil {
		t.Fatalf("Apply close-as-duplicate error = %v", err)
	}
	if closed.State() != model.StateClosed {
		t.Errorf("State after close = %q, want closed", closed.State())
	}
	// The outcome travels through the state machine into the closed leaf, so
	// both halves come back off the issue rather than out of a second write.
	if resolution := closed.ResolutionValue(); resolution == nil || *resolution != model.ResolutionDuplicate {
		t.Errorf("Resolution after close = %v, want duplicate", resolution)
	}
	if target := closed.RedirectTargetValue(); target == nil || *target != canonical.ID {
		t.Errorf("RedirectTarget after close = %v, want %s", target, canonical.ID)
	}
	if closed.ClosedAtValue() == nil {
		t.Error("ClosedAt after close is nil; a close must record when")
	}

	reopened, err := st.Apply(ctx, duplicate.ID, storage.Change{Action: model.Reopen{}, Actor: "ada"})
	if err != nil {
		t.Fatalf("Apply reopen error = %v", err)
	}
	// Reopening clears the whole close payload together — a live issue
	// carrying a stale resolution or redirect is a state no reader could make
	// sense of.
	if reopened.ResolutionValue() != nil || reopened.RedirectTargetValue() != nil || reopened.ClosedAtValue() != nil {
		t.Errorf("reopen left a close payload: resolution=%v redirect=%v closedAt=%v",
			reopened.ResolutionValue(), reopened.RedirectTargetValue(), reopened.ClosedAtValue())
	}

	_, err = st.Apply(ctx, duplicate.ID, storage.Change{
		Action: model.Close{Outcome: model.Duplicate{Of: "no-such-issue"}}, Actor: "ada",
	})
	assertNotFound(t, err, "issue", "closing as a duplicate of a missing issue")
	if _, err := st.Apply(ctx, duplicate.ID, storage.Change{
		Action: model.Close{Outcome: model.Duplicate{Of: duplicate.ID}}, Actor: "ada",
	}); err == nil {
		t.Error("closing an issue as a duplicate of itself succeeded; want an error")
	}
}

func commentsRoundtrip(t *testing.T, ctx context.Context, st storage.Store) {
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "discussed", Topic: "core"})

	comment, withComment, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: issue.ID, Body: "first", CreatedBy: "ada"})
	if err != nil {
		t.Fatalf("AddComment error = %v", err)
	}
	if comment.Body != "first" || comment.CreatedBy != "ada" || comment.IssueID != issue.ID {
		t.Errorf("AddComment returned %+v, want the comment as written", comment)
	}
	// The second return is the issue as it stands after the write, so a caller
	// rendering both does not re-read for the second one.
	if withComment.ID != issue.ID {
		t.Errorf("AddComment returned issue %q, want %q", withComment.ID, issue.ID)
	}

	detail, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail error = %v", err)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].ID != comment.ID {
		t.Fatalf("detail comments = %+v, want the one just added", detail.Comments)
	}

	deleted, err := st.DeleteComment(ctx, comment.ID)
	if err != nil {
		t.Fatalf("DeleteComment error = %v", err)
	}
	// Delete reports what it removed, so the deletion is describable without a
	// prior read.
	if deleted.ID != comment.ID || deleted.Body != "first" {
		t.Errorf("DeleteComment returned %+v, want the removed comment", deleted)
	}

	detail, err = st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail after delete error = %v", err)
	}
	if len(detail.Comments) != 0 {
		t.Errorf("detail comments = %+v, want none after delete", detail.Comments)
	}

	_, err = st.DeleteComment(ctx, "no-such-comment")
	assertNotFound(t, err, "comment", "DeleteComment")

	_, _, err = st.AddComment(ctx, storage.AddCommentInput{IssueID: "no-such-issue", Body: "x", CreatedBy: "ada"})
	assertNotFound(t, err, "issue", "AddComment on a missing issue")
}

func labelsRoundtrip(t *testing.T, ctx context.Context, st storage.Store) {
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "tagged", Topic: "core"})

	// Every mutating verb returns the resulting set, so "what does it have now"
	// is never a follow-up read a concurrent writer could answer differently.
	labels, err := st.AddLabel(ctx, storage.AddLabelInput{IssueID: issue.ID, Name: "zeta", CreatedBy: "ada"})
	if err != nil {
		t.Fatalf("AddLabel error = %v", err)
	}
	assertStrings(t, "after adding zeta", labels, []string{"zeta"})

	labels, err = st.AddLabel(ctx, storage.AddLabelInput{IssueID: issue.ID, Name: "alpha", CreatedBy: "ada"})
	if err != nil {
		t.Fatalf("AddLabel error = %v", err)
	}
	// The set is ordered by name, not by when each label arrived.
	assertStrings(t, "after adding alpha", labels, []string{"alpha", "zeta"})

	// Adding a label twice is the same end state, not an error: the caller
	// asked for the label to be present, and it is.
	labels, err = st.AddLabel(ctx, storage.AddLabelInput{IssueID: issue.ID, Name: "alpha", CreatedBy: "ada"})
	if err != nil {
		t.Fatalf("AddLabel duplicate error = %v", err)
	}
	assertStrings(t, "after a duplicate add", labels, []string{"alpha", "zeta"})

	labels, err = st.RemoveLabel(ctx, issue.ID, "alpha")
	if err != nil {
		t.Fatalf("RemoveLabel error = %v", err)
	}
	assertStrings(t, "after removing alpha", labels, []string{"zeta"})

	// Removing what was never there is not a silent success — nothing was
	// removed, and saying otherwise would be an answer-shaped void.
	_, err = st.RemoveLabel(ctx, issue.ID, "alpha")
	assertNotFound(t, err, "label", "RemoveLabel of an absent label")

	if err := st.ReplaceLabels(ctx, issue.ID, []string{"gamma", "beta"}, "ada"); err != nil {
		t.Fatalf("ReplaceLabels error = %v", err)
	}
	listed, err := st.ListLabels(ctx, issue.ID)
	if err != nil {
		t.Fatalf("ListLabels error = %v", err)
	}
	// Replace states the whole set: what was there and is not named is gone.
	assertStrings(t, "after replace", listed, []string{"beta", "gamma"})

	_, err = st.AddLabel(ctx, storage.AddLabelInput{IssueID: "no-such-issue", Name: "x", CreatedBy: "ada"})
	assertNotFound(t, err, "issue", "AddLabel on a missing issue")
}

func relationsRoundtrip(t *testing.T, ctx context.Context, st storage.Store) {
	dependent := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "dependent", Topic: "core"})
	dependency := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "dependency", Topic: "core"})
	peer := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "peer", Topic: "core"})

	rel, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID: dependent.ID, DstID: dependency.ID, Type: model.RelBlocks, CreatedBy: "ada",
	})
	if err != nil {
		t.Fatalf("AddRelation blocks error = %v", err)
	}
	// The direction convention is contract: src is the dependent, dst is the
	// dependency. Two engines that disagreed here would disagree about which
	// work is ready.
	if rel.SrcID != dependent.ID || rel.DstID != dependency.ID {
		t.Errorf("AddRelation returned %+v, want src=dependent dst=dependency", rel)
	}
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID: dependent.ID, DstID: peer.ID, Type: model.RelRelatedTo, CreatedBy: "ada",
	}); err != nil {
		t.Fatalf("AddRelation related-to error = %v", err)
	}

	all, err := st.ListRelationsForIssue(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("ListRelationsForIssue error = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListRelationsForIssue returned %d edges, want both", len(all))
	}

	// The variadic types argument narrows rather than switching behavior:
	// naming no type means "every type", never "no types".
	blocks, err := st.ListRelationsForIssue(ctx, dependent.ID, model.RelBlocks)
	if err != nil {
		t.Fatalf("ListRelationsForIssue(blocks) error = %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != model.RelBlocks {
		t.Errorf("filtered edges = %+v, want the one blocks edge", blocks)
	}

	// Edges are read from either end — the dependency sees its dependent.
	fromOtherEnd, err := st.ListRelationsForIssue(ctx, dependency.ID, model.RelBlocks)
	if err != nil {
		t.Fatalf("ListRelationsForIssue from the dependency error = %v", err)
	}
	if len(fromOtherEnd) != 1 {
		t.Errorf("edges at the dependency = %+v, want the one blocks edge", fromOtherEnd)
	}

	if err := st.RemoveRelation(ctx, dependent.ID, dependency.ID, model.RelBlocks); err != nil {
		t.Fatalf("RemoveRelation error = %v", err)
	}
	err = st.RemoveRelation(ctx, dependent.ID, dependency.ID, model.RelBlocks)
	assertNotFound(t, err, "relation", "RemoveRelation of an absent edge")

	// related-to is symmetric, so an issue cannot be related to itself.
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID: peer.ID, DstID: peer.ID, Type: model.RelRelatedTo, CreatedBy: "ada",
	}); err == nil {
		t.Error("AddRelation related-to self succeeded; want an error")
	}
}

func relationsBatchBucketsEdges(t *testing.T, ctx context.Context, st storage.Store) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})
	dependency := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "dependency", Topic: "core"})
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID: child.ID, DstID: dependency.ID, Type: model.RelBlocks, CreatedBy: "ada",
	}); err != nil {
		t.Fatalf("AddRelation error = %v", err)
	}

	byID, err := st.GetRelationsByIDs(ctx, []string{epic.ID, child.ID, dependency.ID})
	if err != nil {
		t.Fatalf("GetRelationsByIDs error = %v", err)
	}
	if len(byID) != 3 {
		t.Fatalf("GetRelationsByIDs returned %d entries, want 3", len(byID))
	}

	childEdges := byID[child.ID]
	if childEdges.Parent == nil || childEdges.Parent.ID != epic.ID {
		t.Errorf("child Parent = %+v, want the epic", childEdges.Parent)
	}
	assertIssueIDs(t, "child DependsOn", childEdges.DependsOn, []string{dependency.ID})

	assertIssueIDs(t, "epic Children", byID[epic.ID].Children, []string{child.ID})

	// DependsOn and Blocks are the two readings of one edge set, which is why
	// the dependency sees the child under Blocks without a second row existing.
	assertIssueIDs(t, "dependency Blocks", byID[dependency.ID].Blocks, []string{child.ID})

	// An id nobody asked about is simply not in the map; asking about none is
	// an empty result, not an error.
	empty, err := st.GetRelationsByIDs(ctx, nil)
	if err != nil {
		t.Fatalf("GetRelationsByIDs(nil) error = %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("GetRelationsByIDs(nil) = %+v, want empty", empty)
	}
}

func parentWiring(t *testing.T, ctx context.Context, st storage.Store) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	other := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "other epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core"})

	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: child.ID, ParentID: epic.ID, CreatedBy: "ada"}); err != nil {
		t.Fatalf("SetParent error = %v", err)
	}
	assertIssueIDs(t, "children of the epic", mustChildren(t, ctx, st, epic.ID), []string{child.ID})

	// An issue has at most one parent, so reparenting replaces rather than
	// adds: the old parent loses the child in the same act.
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: child.ID, ParentID: other.ID, CreatedBy: "ada"}); err != nil {
		t.Fatalf("SetParent reparent error = %v", err)
	}
	assertIssueIDs(t, "children of the old epic", mustChildren(t, ctx, st, epic.ID), nil)
	assertIssueIDs(t, "children of the new epic", mustChildren(t, ctx, st, other.ID), []string{child.ID})

	if err := st.ClearParent(ctx, child.ID); err != nil {
		t.Fatalf("ClearParent error = %v", err)
	}
	assertIssueIDs(t, "children after clearing", mustChildren(t, ctx, st, other.ID), nil)

	// Clearing a parent that is not there removed no edge, and says so.
	err := st.ClearParent(ctx, child.ID)
	assertNotFound(t, err, "parent relation", "ClearParent of a parentless child")

	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: child.ID, ParentID: child.ID}); err == nil {
		t.Error("SetParent to itself succeeded; want an error")
	}
	_, err = st.SetParent(ctx, storage.SetParentInput{ChildID: child.ID, ParentID: "no-such-issue"})
	assertNotFound(t, err, "issue", "SetParent under a missing parent")
}

func topicsDeriveFromIssues(t *testing.T, ctx context.Context, st storage.Store) {
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "one", Topic: "renderer"})
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "two", Topic: "renderer"})
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "three", Topic: "parser"})

	topics, err := st.ListTopics(ctx)
	if err != nil {
		t.Fatalf("ListTopics error = %v", err)
	}
	// Topics are derived vocabulary — distinct, ascending, never a stored list
	// that could disagree with the issues.
	assertStrings(t, "topics", topics, []string{"parser", "renderer"})
}

func exportCarriesWholeStore(t *testing.T, ctx context.Context, st storage.Store) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID, Labels: []string{"perf"}})
	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: child.ID, Body: "note", CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddComment error = %v", err)
	}
	archived := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "archived", Topic: "core"})
	if _, err := st.Apply(ctx, archived.ID, storage.Change{Action: model.Archive{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply archive error = %v", err)
	}

	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export error = %v", err)
	}
	// Export is the differential oracle's surface, so it is the WHOLE store —
	// out-of-flow issues included. An export that honored the listing default
	// would silently drop archived work from every backup and every diff.
	assertIssueIDs(t, "exported issues", export.Issues, []string{epic.ID, child.ID, archived.ID})
	if len(export.Relations) != 1 || export.Relations[0].SrcID != child.ID {
		t.Errorf("exported relations = %+v, want the one parent edge", export.Relations)
	}
	if len(export.Comments) != 1 || export.Comments[0].IssueID != child.ID {
		t.Errorf("exported comments = %+v, want the one comment", export.Comments)
	}
	if len(export.Labels) != 1 || export.Labels[0].Name != "perf" {
		t.Errorf("exported labels = %+v, want the one label", export.Labels)
	}
	if len(export.Events) == 0 {
		t.Error("exported events are empty; the history must travel with the state")
	}
	if export.ExportedAt.IsZero() {
		t.Error("ExportedAt is zero; an export must say when it was taken")
	}
}

func bulkApplyCreatesAndUpdates(t *testing.T, ctx context.Context, st storage.Store) {
	title := "parent doc"
	childTitle := "child doc"
	topic := "core"
	issueType := "task"
	epicType := "epic"

	result, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{LocalID: "root", Title: &title, Topic: &topic, IssueType: &epicType},
		{LocalID: "leaf", Title: &childTitle, Topic: &topic, IssueType: &issueType, Parent: "root"},
	})
	if err != nil {
		t.Fatalf("BulkApply error = %v", err)
	}
	// A create document is nameable by the LocalID it chose, which is what lets
	// the batch wire its own internal references before any id exists.
	rootID, ok := result.Created["root"]
	if !ok {
		t.Fatalf("BulkApply Created = %+v, want a root entry", result.Created)
	}
	leafID, ok := result.Created["leaf"]
	if !ok {
		t.Fatalf("BulkApply Created = %+v, want a leaf entry", result.Created)
	}
	assertIssueIDs(t, "children wired by local id", mustChildren(t, ctx, st, rootID), []string{leafID})

	// A document that names a real id is an update, not a second create.
	newTitle := "retitled by bulk"
	result, err = st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{{ID: leafID, Title: &newTitle}})
	if err != nil {
		t.Fatalf("BulkApply update error = %v", err)
	}
	assertStrings(t, "bulk updated ids", result.Updated, []string{leafID})
	if len(result.Created) != 0 {
		t.Errorf("BulkApply Created = %+v, want none for an update-only batch", result.Created)
	}
	read, err := st.GetIssue(ctx, leafID)
	if err != nil {
		t.Fatalf("GetIssue error = %v", err)
	}
	if read.Title != newTitle {
		t.Errorf("Title = %q, want %q", read.Title, newTitle)
	}
}

// bulkApplyCompensatesFailure pins what a batch owes when it fails partway.
// The contract trades atomicity for an account: the issues the batch created
// are undone, and what could not be undone is named in the error. A batch that
// left its early creates standing and said nothing would be the worst of both.
// [LAW:no-silent-failure]
func bulkApplyCompensatesFailure(t *testing.T, ctx context.Context, st storage.Store) {
	kept := "created before the failure"
	orphan := "parented to nothing"
	topic := "core"
	issueType := "task"

	_, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{LocalID: "first", Title: &kept, Topic: &topic, IssueType: &issueType},
		// A parent that is neither a name in this batch nor a real id passes
		// the file's own validation and fails at the write — which is exactly
		// the mid-batch failure the compensation exists for.
		{LocalID: "second", Title: &orphan, Topic: &topic, IssueType: &issueType, Parent: "no-such-issue"},
	})
	if err == nil {
		t.Fatal("BulkApply with an unresolvable parent succeeded; want an error")
	}
	assertIssueIDs(t, "issues after a compensated batch", mustList(t, ctx, st, storage.ListIssuesFilter{}), nil)
}

func importTreeMapsLocalIDs(t *testing.T, ctx context.Context, st storage.Store) {
	result, err := st.ImportTree(ctx, prefix, []storage.ImportTreeSpec{
		{LocalID: "root", Title: "root", IssueType: "epic", Topic: "core"},
		{LocalID: "leaf", Title: "leaf", IssueType: "task", Topic: "core", Parent: "root"},
		{LocalID: "other", Title: "other", IssueType: "task", Topic: "core", DependsOn: []string{"leaf"}},
	})
	if err != nil {
		t.Fatalf("ImportTree error = %v", err)
	}
	if len(result.IDMap) != 3 {
		t.Fatalf("IDMap = %+v, want an entry per spec", result.IDMap)
	}
	assertIssueIDs(t, "imported children", mustChildren(t, ctx, st, result.IDMap["root"]), []string{result.IDMap["leaf"]})

	// depends_on in the file means "this issue depends on that one", so the
	// dependent is the edge's src.
	edges, err := st.ListRelationsForIssue(ctx, result.IDMap["other"], model.RelBlocks)
	if err != nil {
		t.Fatalf("ListRelationsForIssue error = %v", err)
	}
	if len(edges) != 1 || edges[0].SrcID != result.IDMap["other"] || edges[0].DstID != result.IDMap["leaf"] {
		t.Errorf("imported dependency edges = %+v, want other→leaf", edges)
	}
}

func attributionStampsEvents(t *testing.T, ctx context.Context, st storage.Store) {
	// Before attribution is named, work is recorded unattributed rather than
	// half-attributed — the read-mode open of a never-mutated checkout.
	before := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "anonymous", Topic: "core"})

	st.AttributeTo("stream-token")
	after := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "attributed", Topic: "core"})

	byIssue := map[string][]model.IssueEvent{}
	for _, e := range mustEvents(t, ctx, st) {
		byIssue[e.IssueID] = append(byIssue[e.IssueID], e)
	}
	for _, e := range byIssue[before.ID] {
		if e.Attribution.Present() {
			t.Errorf("event %q carries attribution written before AttributeTo", e.ID)
		}
	}
	if len(byIssue[after.ID]) == 0 {
		t.Fatal("the attributed issue recorded no events")
	}
	for _, e := range byIssue[after.ID] {
		// Attribution is the store's, stamped at its one insertion point, so it
		// is on every event a mutation produced rather than on the ones a call
		// site remembered to stamp.
		if !e.Attribution.Present() {
			t.Errorf("event %q carries no attribution after AttributeTo", e.ID)
		}
		if e.Attribution.Stream() != "stream-token" {
			t.Errorf("event %q stream = %q, want stream-token", e.ID, e.Attribution.Stream())
		}
		// The pair is complete or absent; a stream with no workspace to scope
		// it is a half-fact the type refuses to carry.
		if e.Attribution.Workspace() == "" {
			t.Errorf("event %q has a stream but no workspace", e.ID)
		}
	}
}

func localIssueCountTracksCreates(t *testing.T, ctx context.Context, st storage.Store) {
	// A store with nothing in it reports zero rather than failing: "no issues
	// yet" is a real state, not a fault.
	count, err := st.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount error = %v", err)
	}
	if count != 0 {
		t.Errorf("LocalIssueCount on a fresh engine = %d, want 0", count)
	}

	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "one", Topic: "core"})
	deleted := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "two", Topic: "core"})
	if _, err := st.Apply(ctx, deleted.ID, storage.Change{Action: model.Delete{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply delete error = %v", err)
	}

	count, err = st.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount error = %v", err)
	}
	// It counts what the store holds, which is the adopt-safety question —
	// a soft-deleted issue is still work that would be lost.
	if count != 2 {
		t.Errorf("LocalIssueCount = %d, want 2", count)
	}
}

// --- helpers -------------------------------------------------------------
//
// Every helper fails the test rather than returning an error to be checked, so
// a case body reads as the behavioral statement it is and not as error
// plumbing around one.

func mustCreate(t *testing.T, ctx context.Context, st storage.Store, in storage.CreateIssueInput) model.Issue {
	t.Helper()
	in.Prefix = prefix
	issue, err := st.CreateIssue(ctx, in)
	if err != nil {
		t.Fatalf("CreateIssue(%q) error = %v", in.Title, err)
	}
	return issue
}

func mustList(t *testing.T, ctx context.Context, st storage.Store, filter storage.ListIssuesFilter) []model.Issue {
	t.Helper()
	issues, err := st.ListIssues(ctx, filter)
	if err != nil {
		t.Fatalf("ListIssues(%+v) error = %v", filter, err)
	}
	return issues
}

func mustChildren(t *testing.T, ctx context.Context, st storage.Store, parentID string) []model.Issue {
	t.Helper()
	children, err := st.ListChildren(ctx, parentID)
	if err != nil {
		t.Fatalf("ListChildren(%q) error = %v", parentID, err)
	}
	return children
}

func mustEvents(t *testing.T, ctx context.Context, st storage.Store) []model.IssueEvent {
	t.Helper()
	events, err := st.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents error = %v", err)
	}
	return events
}

// assertOrder pins the order of a default (unsorted) listing, which is the
// only surface through which rank is observable across engines — the Rank
// value itself is an engine's own encoding and is deliberately never asserted.
func assertOrder(t *testing.T, ctx context.Context, st storage.Store, what string, want ...string) {
	t.Helper()
	assertIssueIDs(t, what, mustList(t, ctx, st, storage.ListIssuesFilter{}), want)
}

// assertState reads an issue back and pins its derived state, including the
// out-of-flow issues a default listing would not show.
func assertState(t *testing.T, ctx context.Context, st storage.Store, id string, want model.State, why string) {
	t.Helper()
	issue, err := st.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue(%q) error = %v", id, err)
	}
	if issue.State() != want {
		t.Errorf("State of %s = %q, want %q — %s", id, issue.State(), want, why)
	}
}

// assertPrecedes pins a relative position without pinning the whole listing,
// for the cases where the contract states which of two issues comes first and
// says nothing about what else sits between them.
func assertPrecedes(t *testing.T, issues []model.Issue, first, second string) {
	t.Helper()
	firstAt, secondAt := -1, -1
	for i, issue := range issues {
		switch issue.ID {
		case first:
			firstAt = i
		case second:
			secondAt = i
		}
	}
	if firstAt < 0 || secondAt < 0 {
		t.Fatalf("listing is missing %s or %s: %v", first, second, issues)
	}
	if firstAt > secondAt {
		t.Errorf("%s follows %s in the listing; want it to precede", first, second)
	}
}

func assertIssueIDs(t *testing.T, what string, got []model.Issue, want []string) {
	t.Helper()
	ids := make([]string, 0, len(got))
	for _, issue := range got {
		ids = append(ids, issue.ID)
	}
	assertStrings(t, what, ids, want)
}

// assertStrings compares sequences by content and order. A nil result and an
// empty one compare equal on purpose: "there is nothing here" is one fact, and
// an engine that spelled it as nil rather than as a zero-length slice has not
// behaved differently.
func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// assertNotFound pins the identity of the contract's absence error, entity
// included: callers dispatch on the type, and the entity is what tells a user
// WHICH thing was missing when an operation touches several.
func assertNotFound(t *testing.T, err error, entity, what string) {
	t.Helper()
	var notFound storage.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("%s error = %v, want storage.NotFoundError", what, err)
	}
	if notFound.Entity != entity {
		t.Errorf("%s reported entity %q, want %q", what, notFound.Entity, entity)
	}
}
