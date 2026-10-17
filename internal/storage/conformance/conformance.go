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
//		conformance.Run(t, func(t *testing.T, clock storage.Clock) storage.Store { return openMyEngine(t, clock) })
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
// Dolt's behavior is the tiebreak — not because it is the only implementation,
// but because the S0 migration state's whole gate is that nothing observable
// changes. Where a behavior is ambiguous these cases record what Dolt does
// rather than what would be tidier; where the second engine answers BETTER, it
// is moved to match rather than the contract moved to meet it, because an
// engine that is right where the other is arbitrary still reads as divergence
// to the differential oracle. Correcting one of those faults moves observable
// output, which makes it a ticket rather than a cleanup.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// NewEngine mints a fresh, empty engine for one case, already registered for
// cleanup with t and stamping its writes from clock. Every case gets its own: a
// suite whose cases shared a store would be pinning the order they run in as
// much as the contract.
//
// The clock is a parameter of the factory rather than something a case reaches
// for, and that is what makes honoring it mandatory: an engine that cannot be
// told what time it is cannot supply this signature, and an engine that takes
// the clock and ignores it fails the cases that pin one. No capability gates
// it, so no engine can opt out of the statements it makes reachable.
// [LAW:types-are-the-program]
type NewEngine func(t *testing.T, clock storage.Clock) storage.Store

// Run executes the whole suite against engines newEngine produces.
func Run(t *testing.T, newEngine NewEngine) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := newClock()
			c.run(t, context.Background(), newEngine(t, clk.read), clk)
		})
	}
}

// engineCase is one behavioral statement about the contract.
// [LAW:dataflow-not-control-flow] The suite is a table walked by one loop, so
// adding a statement is adding data — never another arm in Run.
type engineCase struct {
	name string
	run  func(t *testing.T, ctx context.Context, st storage.Store, clk *clock)
}

// clock is the instant the engine under test stamps its writes with.
//
// It runs on the real clock until a case pins it. Pinning is what makes two
// facts constructible that no real clock will hand out on demand: a run of
// writes sharing one instant, which is the tie the (created_at, id) rule exists
// to settle, and a pair of instants whose TEXT order is the reverse of their
// instant order, which is how an engine that sorts a timestamp by its spelling
// gets caught.
//
// [LAW:dataflow-not-control-flow] Pinning replaces the function rather than
// setting a "pinned" flag some branch consults, so there is one read path
// whatever a case has asked for.
type clock struct {
	mu  sync.Mutex
	now storage.Clock
}

func newClock() *clock { return &clock{now: storage.SystemClock} }

// Pin fixes the instant every subsequent stamp reads. It is not an advancing
// sequence: one contract operation may stamp several rows, and a clock that
// moved between them would make what a case asserts depend on how many times
// the engine happened to read it. [LAW:no-ambient-temporal-coupling]
func (c *clock) Pin(instant time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = func() time.Time { return instant }
}

// read is the [storage.Clock] the engine under test holds. The lock is here
// because the case writes what the engine reads, and that is shared mutable
// state however single-threaded a given case happens to be.
// [LAW:no-shared-mutable-globals]
func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now()
}

// prefix is the cosmetic id prefix every case creates under. Cases assert on
// ids only by comparing ids the engine returned, never by predicting their
// shape — id minting is engine business.
const prefix = "conf"

// earlierInstant and laterInstant are the pair every case that pins time uses,
// and their SPELLING is why this exact pair: RFC3339Nano trims trailing zeros,
// so the earlier instant renders as the shorter string, and at the character
// where that string ends the other is still going — "Z" (0x5A) outranks any
// digit. ".12Z" therefore sorts after ".123456789Z" while denoting an instant
// three milliseconds before it.
//
// An engine that round-trips a stamp through text and then compares the text
// orders this pair backwards. That is not hypothetical: the Dolt engine keeps
// created_at in a varchar. An engine holding a real instant passes without
// noticing there was anything to get wrong. Left to a live nanosecond clock the
// collision arises about once in ten million pairs, which is why no case can
// state it without the contract's clock seam.
//
// They are fixed values rather than offsets from the real clock because a case
// that pins time is asserting about an exact stamp, and deriving one from
// time.Now would hand part of that assertion back to the clock it just took
// away. [LAW:no-ambient-temporal-coupling]
var (
	earlierInstant = time.Date(2026, 1, 1, 0, 0, 0, 120_000_000, time.UTC)
	laterInstant   = time.Date(2026, 1, 1, 0, 0, 0, 123_456_789, time.UTC)
)

var cases = []engineCase{
	{"create_read_roundtrip", createReadRoundtrip},
	{"create_defaults", createDefaults},
	{"create_requires_title", createRequiresTitle},
	{"create_normalizes_topic", createNormalizesTopic},
	{"create_under_missing_parent_is_not_found", createUnderMissingParent},
	{"get_missing_issue_is_not_found", getMissingIssue},
	{"apply_field_patch", applyFieldPatch},
	{"an_assignee_no_filter_can_select_is_refused", anAssigneeNoFilterCanSelectIsRefused},
	{"apply_status_transition", applyStatusTransition},
	{"apply_missing_issue_is_not_found", applyMissingIssue},
	{"apply_to_container_is_refused", applyToContainer},
	{"apply_to_frozen_names_the_thaw", applyToFrozenNamesTheThaw},
	{"container_state_follows_live_children", containerStateFollowsLiveChildren},
	{"history_records_mutations", historyRecordsMutations},
	{"a_phase_move_records_its_event", aPhaseMoveRecordsItsEvent},
	{"every_editable_field_records_history", everyEditableFieldRecordsHistory},
	{"a_new_checkout_taking_a_ticket_records", takeoverByANewCheckoutRecords},
	{"a_reported_handover_is_the_one_recorded", aReportedHandoverIsTheOneRecorded},
	{"one_issues_history_matches_the_whole_log", oneIssuesHistoryMatchesTheWholeLog},
	{"list_defaults_to_rank_order", listDefaultsToRankOrder},
	{"list_filters_select", listFiltersSelect},
	{"list_by_parent", listByParent},
	{"list_hides_archived_and_deleted", listHidesArchivedAndDeleted},
	{"list_sorts_and_limits", listSortsAndLimits},
	{"list_breaks_sort_ties_by_id", listBreaksSortTiesByID},
	{"list_orders_by_each_value_key", listOrdersByEachValueKey},
	{"list_sorts_status_by_derived_state", listSortsStatusByDerivedState},
	{"stamps_come_from_the_clock_the_engine_was_given", stampsComeFromTheClock},
	{"list_orders_timestamps_by_instant_not_spelling", listOrdersTimestampsByInstant},
	{"events_are_totally_ordered", eventsAreTotallyOrdered},
	{"rank_intents_reorder", rankIntentsReorder},
	{"rank_intents_resolve_across_frames", rankIntentsResolveAcrossFrames},
	{"rank_to_edge_stays_inside_its_frame", rankToEdgeStaysInsideItsFrame},
	{"rank_to_edge_ignores_a_deleted_frame_mate", rankToEdgeIgnoresADeletedFrameMate},
	{"create_at_top_files_inside_its_frame", createAtTopFilesInsideItsFrame},
	{"create_at_top_of_an_empty_frame_takes_a_distinct_key", createAtTopOfAnEmptyFrameTakesADistinctKey},
	{"placing_at_a_frame_edge_keeps_the_key_beside_its_frame", placingAtAFrameEdgeKeepsTheKeyBesideItsFrame},
	{"a_relative_move_keeps_every_rank_distinct", aRelativeMoveKeepsEveryRankDistinct},
	{"rank_set_imposes_order", rankSetImposesOrder},
	{"rank_set_stays_inside_its_frame", rankSetStaysInsideItsFrame},
	{"rank_set_over_a_whole_frame_permutes_its_keys", rankSetOverAWholeFramePermutesItsKeys},
	{"rank_verbs_refuse_a_deleted_issue", rankVerbsRefuseADeletedIssue},
	{"a_container_lists_before_everything_inside_it", aContainerListsBeforeEverythingInsideIt},
	{"close_redirects_to_a_canonical", closeRedirectsToCanonical},
	{"follow_ups_link_both_ways", followUpsLinkBothWays},
	{"a_purge_removes_the_issue_and_everything_it_owns", aPurgeRemovesTheIssueAndEverythingItOwns},
	{"a_purge_refuses_a_container_that_holds_tickets", aPurgeRefusesAContainerThatHoldsTickets},
	{"a_purge_of_a_missing_issue_is_not_found", aPurgeOfAMissingIssueIsNotFound},
	{"a_purged_id_is_never_minted_again", aPurgedIDIsNeverMintedAgain},
	{"comments_roundtrip", commentsRoundtrip},
	{"labels_roundtrip", labelsRoundtrip},
	{"relations_roundtrip", relationsRoundtrip},
	{"relations_batch_buckets_edges", relationsBatchBucketsEdges},
	{"rank_inversions_read_tree_order", rankInversionsReadTreeOrder},
	{"interleaved_epics_hold_no_rank_inversion", interleavedEpicsHoldNoRankInversion},
	{"parent_wiring", parentWiring},
	{"every_ticket_lives_inside_a_container", everyTicketLivesInsideAContainer},
	{"an_import_file_that_breaks_a_rule_is_refused_whole", anImportFileThatBreaksARuleIsRefusedWhole},
	{"the_hierarchy_stays_a_tree", theHierarchyStaysATree},
	{"blocks_never_runs_along_the_hierarchy", blocksNeverRunsAlongTheHierarchy},
	{"blocks_never_orders_two_leaves_of_one_epic", blocksNeverOrdersTwoLeavesOfOneEpic},
	{"topics_derive_from_issues", topicsDeriveFromIssues},
	{"issue_identities_cover_every_row", issueIdentitiesCoverEveryRow},
	{"export_carries_whole_store", exportCarriesWholeStore},
	{"bulk_apply_creates_and_updates", bulkApplyCreatesAndUpdates},
	{"bulk_apply_compensates_a_failed_batch", bulkApplyCompensatesFailure},
	{"import_tree_maps_local_ids", importTreeMapsLocalIDs},
	{"attribution_stamps_events", attributionStampsEvents},
	{"observers_hear_every_change", observersHearEveryChange},
	{"local_issue_count_tracks_creates", localIssueCountTracksCreates},
}

func createReadRoundtrip(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "renderer", IssueType: model.TypeEpic})
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
		ParentID:    epic.ID,
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

func createDefaults(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// Saying nothing about type or placement must reach the product defaults,
	// since that is the path every non-interactive creation surface takes. The
	// epic is the home every leaf needs; it leads the listing as the container
	// of everything filed below.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "unspecified", Topic: "core", ParentID: epic.ID})
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
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second", Topic: "core", ParentID: epic.ID})
	assertOrder(t, ctx, st, "default placement appends", epic.ID, issue.ID, second.ID)

	top := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "top", Topic: "core", ParentID: epic.ID, Placement: storage.RankTop})
	assertOrder(t, ctx, st, "RankTop leads", epic.ID, top.ID, issue.ID, second.ID)
}

func createRequiresTitle(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The leaf is filed under an epic so the title is the only thing wrong with
	// it: a refusal here has to be the title's.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	// Whitespace is not a title: the trim happens before the requirement, so a
	// blank-looking title cannot slip in as a non-empty string.
	for _, title := range []string{"", "   "} {
		_, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: title, Topic: "core", ParentID: epic.ID})
		assertRefused(t, err, fmt.Sprintf("CreateIssue(title=%q)", title))
	}
}

// createNormalizesTopic pins the topic as already-canonical by the time it is
// stored. The vocabulary is DERIVED from what issues carry, and a topic that
// reached storage in one spelling could never be found under another — so
// normalizing is the store's job, not each caller's, and a name too short to
// be one is refused rather than stored. [LAW:parse-dont-validate]
func createNormalizesTopic(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The epic shares the leaf's topic so the vocabulary below is one name.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "renderer-cleanup", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "shaped", Topic: "  Renderer Cleanup  ", ParentID: epic.ID})
	if issue.Topic != "renderer-cleanup" {
		t.Errorf("Topic = %q, want the normalized renderer-cleanup", issue.Topic)
	}
	topics, err := st.ListTopics(ctx)
	if err != nil {
		t.Fatalf("ListTopics error = %v", err)
	}
	assertStrings(t, "topics after a normalized create", topics, []string{"renderer-cleanup"})

	// Filed under the epic so the topic is the only fault the refusal can be
	// about.
	for _, topic := range []string{"", "   ", "ab", "-!-", strings.Repeat("a", 31)} {
		_, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "unnamed", Topic: topic, ParentID: epic.ID, Prefix: prefix})
		assertRefused(t, err, fmt.Sprintf("CreateIssue(topic=%q)", topic))
	}
}

func createUnderMissingParent(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	_, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "orphan", Topic: "core", ParentID: "no-such-issue"})
	assertNotFound(t, err, "issue", "CreateIssue under a missing parent")
}

func getMissingIssue(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	_, err := st.GetIssue(ctx, "no-such-issue")
	assertNotFound(t, err, "issue", "GetIssue")

	_, err = st.GetIssueDetail(ctx, "no-such-issue")
	assertNotFound(t, err, "issue", "GetIssueDetail")
}

func applyFieldPatch(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "before", Topic: "core", Assignee: "ada", ParentID: epic.ID})

	title := "after"
	priority := model.PriorityUrgent
	updated, err := st.Apply(ctx, issue.ID, storage.Change{
		Actor:  "grace",
		Fields: storage.UpdateIssueInput{Title: &title, Priority: &priority, Reason: "sharpened"},
	})
	if err != nil {
		t.Fatalf("Apply field patch error = %v", err)
	}
	if updated.Issue.Title != "after" || updated.Issue.Priority != model.PriorityUrgent {
		t.Errorf("Apply returned %q/%v, want after/urgent", updated.Issue.Title, updated.Issue.Priority)
	}
	// A nil pointer means "leave alone", so an untouched field survives a patch
	// that never mentions it. This is the whole reason the patch is pointers.
	if updated.Issue.Assignee != "ada" {
		t.Errorf("Assignee = %q, want the untouched ada", updated.Issue.Assignee)
	}

	read, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue after patch error = %v", err)
	}
	if read.Title != "after" {
		t.Errorf("patch did not persist: Title = %q", read.Title)
	}
}

// anAssigneeNoFilterCanSelectIsRefused pins that no write stores an assignee
// carrying a comma. Every assignee filter reads a comma as the separator
// between names, so "Doe, J" stored would select Doe's and J's tickets and
// never this one. Create, the field update, and start are each tried, and each
// refusal is checked on the stored row: a write that errored and still landed
// would pass an error-only check.
func anAssigneeNoFilterCanSelectIsRefused(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "work", Topic: "core", Assignee: "ada", ParentID: epic.ID})
	before := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true, IncludeArchived: true})
	comma := "Doe, J"

	_, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "comma", Topic: "core", Assignee: comma, ParentID: epic.ID, Prefix: prefix})
	assertRefused(t, err, "CreateIssue(assignee "+comma+")")
	_, err = st.Apply(ctx, issue.ID, storage.Change{Actor: "ada", Fields: storage.UpdateIssueInput{Assignee: &comma}})
	assertRefused(t, err, "Apply(update assignee "+comma+")")
	_, err = st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: comma}, Actor: comma})
	assertRefused(t, err, "Apply(start as "+comma+")")

	if after := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true, IncludeArchived: true}); len(after) != len(before) {
		t.Errorf("issues after the refused create = %d, want %d", len(after), len(before))
	}
	read, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue error = %v", err)
	}
	if read.Assignee != "ada" || read.State() != model.StateOpen {
		t.Errorf("after the refused update and start: assignee %q, state %q; want ada, open", read.Assignee, read.State())
	}
}

func applyStatusTransition(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "work", Topic: "core", ParentID: epic.ID})

	started, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"})
	if err != nil {
		t.Fatalf("Apply start error = %v", err)
	}
	if started.Issue.State() != model.StateInProgress {
		t.Errorf("State after start = %q, want in_progress", started.Issue.State())
	}
	// Start is the one action that rewrites ownership, which is why it is the
	// one variant carrying an assignee.
	if started.Issue.Assignee != "ada" {
		t.Errorf("Assignee after start = %q, want ada", started.Issue.Assignee)
	}

	done, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Done{}, Actor: "ada"})
	if err != nil {
		t.Fatalf("Apply done error = %v", err)
	}
	if done.Issue.State() != model.StateClosed {
		t.Errorf("State after done = %q, want closed", done.Issue.State())
	}

	reopened, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Reopen{}, Actor: "ada"})
	if err != nil {
		t.Fatalf("Apply reopen error = %v", err)
	}
	if reopened.Issue.State() != model.StateOpen {
		t.Errorf("State after reopen = %q, want open", reopened.Issue.State())
	}
}

func applyMissingIssue(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	_, err := st.Apply(ctx, "no-such-issue", storage.Change{Action: model.Start{}, Actor: "ada"})
	assertNotFound(t, err, "issue", "Apply")
}

func applyToContainer(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
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

// applyToFrozenNamesTheThaw pins the refusal a status action meets on an
// archived or deleted issue: the issue's state and the command that returns it
// to the flow, with no verb of the refused action in it. Reopen is the action
// under test because its persisted name, reopen, is a command lit does not have.
//
// It lives in the conformance suite because each engine reaches the refusal
// through its own planning path, and a test beside either one would let the
// other wrap or replace it. Here both answer to the same statement.
// [LAW:single-enforcer]
func applyToFrozenNamesTheThaw(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	for _, tc := range []struct {
		freeze model.RetentionAction
		state  string
		thaw   string
	}{
		{model.Archive{}, "archived", "unarchive"},
		{model.Delete{}, "deleted", "restore"},
	} {
		epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
		issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: tc.state, Topic: "core", ParentID: epic.ID})
		if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: tc.freeze, Actor: "ada"}); err != nil {
			t.Fatalf("%s error = %v", tc.freeze.Name(), err)
		}
		_, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Reopen{}, Actor: "ada"})
		var frozen model.FrozenIssueError
		if !errors.As(err, &frozen) {
			t.Fatalf("Apply(Reopen) to a %s issue error = %v, want FrozenIssueError", tc.state, err)
		}
		want := fmt.Sprintf("issue %s is %s, so its status cannot change: run `lit %s %s` first", issue.ID, tc.state, tc.thaw, issue.ID)
		if err.Error() != want {
			t.Errorf("Apply(Reopen) to a %s issue error = %q, want %q", tc.state, err.Error(), want)
		}
	}
}

// containerStateFollowsLiveChildren pins the one rule that decides what an
// epic's derived state is about. A container in the flow reads only children
// in the flow, so archiving the last unfinished child finishes the epic; a
// container that is itself out of the flow keeps its whole child set, so an
// archived epic stays the state it had when it left instead of collapsing.
func containerStateFollowsLiveChildren(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
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

// aPhaseMoveRecordsItsEvent: a phase lives in an in-progress ticket's history.
// A start enters plan; a move records one `phase` event naming the phase left
// and the phase entered, and stamps updated_at; a move to the phase the ticket
// is already in records nothing; a ticket with no phase refuses the move; and
// a fresh start after a reopen enters plan again, whatever phase the earlier
// run reached.
func aPhaseMoveRecordsItsEvent(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "phased", Topic: "core", ParentID: epic.ID})
	phaseOf := func() (model.Phase, bool) {
		t.Helper()
		detail, err := st.GetIssueDetail(ctx, issue.ID)
		if err != nil {
			t.Fatalf("GetIssueDetail error = %v", err)
		}
		return model.PhaseOf(detail.Issue, detail.Events)
	}
	move := func(to model.Phase) (storage.Applied, error) {
		return st.Apply(ctx, issue.ID, storage.Change{Action: model.SetPhase{Phase: to}, Actor: "ada"})
	}
	var refused model.ValidationError

	if _, err := move(model.PhaseImpl); !errors.As(err, &refused) {
		t.Fatalf("phase move on an open ticket error = %v, want a ValidationError", err)
	}
	if phase, ok := phaseOf(); ok {
		t.Errorf("open ticket has phase %q, want none", phase)
	}
	// Pinned from the start on: a phase is the newest event that set one, so
	// every event here has to be stamped on the one clock the case controls.
	clk.Pin(earlierInstant)
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply start error = %v", err)
	}
	if phase, ok := phaseOf(); !ok || phase != model.PhasePlan {
		t.Errorf("phase after start = %q (%v), want plan", phase, ok)
	}

	clk.Pin(laterInstant)
	before := len(mustEvents(t, ctx, st))
	moved, err := move(model.PhaseReview)
	if err != nil {
		t.Fatalf("phase move error = %v", err)
	}
	events := mustEvents(t, ctx, st)
	if len(events) != before+1 {
		t.Fatalf("phase move wrote %d events, want 1", len(events)-before)
	}
	var recorded *model.IssueEvent
	for i := range events {
		if events[i].Action == string(model.ActionPhase) {
			recorded = &events[i]
		}
	}
	if recorded == nil {
		t.Fatalf("no %q event among %+v", model.ActionPhase, events)
	}
	want := []model.FieldChange{{Field: model.PhaseField, From: string(model.PhasePlan), To: string(model.PhaseReview)}}
	if !slices.Equal(recorded.Changes, want) {
		t.Errorf("phase event changes = %+v, want %+v", recorded.Changes, want)
	}
	if !moved.Issue.UpdatedAt.Equal(laterInstant) {
		t.Errorf("UpdatedAt after phase move = %v, want %v", moved.Issue.UpdatedAt, laterInstant)
	}
	if phase, _ := phaseOf(); phase != model.PhaseReview {
		t.Errorf("phase after move = %q, want review", phase)
	}

	before = len(mustEvents(t, ctx, st))
	if _, err := move(model.PhaseReview); err != nil {
		t.Fatalf("repeated phase move error = %v", err)
	}
	if after := len(mustEvents(t, ctx, st)); after != before {
		t.Errorf("a phase move that changed nothing wrote %d events, want none", after-before)
	}

	// A move stamped at the instant the row already holds still lands: the
	// clock has not moved since the review move, so updated_at does not change.
	if _, err := move(model.PhaseImpl); err != nil {
		t.Errorf("a phase move at the row's own updated_at error = %v, want it to land", err)
	}

	if _, err := st.Apply(ctx, epic.ID, storage.Change{Action: model.SetPhase{Phase: model.PhaseImpl}, Actor: "ada"}); !errors.As(err, &refused) {
		t.Errorf("phase move on an epic error = %v, want a ValidationError", err)
	}

	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Reopen{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply reopen error = %v", err)
	}
	if _, err := move(model.PhaseImpl); !errors.As(err, &refused) {
		t.Errorf("phase move on a reopened ticket error = %v, want a ValidationError", err)
	}
	// Later than every event above: on a tie the newer of two events is the one
	// with the greater id, which no case can choose.
	clk.Pin(laterInstant.Add(time.Second))
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply restart error = %v", err)
	}
	if phase, _ := phaseOf(); phase != model.PhasePlan {
		t.Errorf("phase after a fresh start = %q, want plan", phase)
	}
}

func historyRecordsMutations(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "tracked", Topic: "core", ParentID: epic.ID})
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

	// A status action whose target state AND resulting claimant already hold is
	// the same no-op: the ticket did not move, so the log does not say it did. A
	// same-state start naming a NEW owner is the reclaim path and does record,
	// which is what keeps "who took this over" answerable. This case moves the
	// assignee half; the checkout half is its own case below, and it has to be,
	// because a takeover routinely moves ONLY the checkout.
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
	if reclaimed.Issue.Assignee != "grace" {
		t.Errorf("Assignee after reclaim = %q, want grace", reclaimed.Issue.Assignee)
	}
	if len(mustEvents(t, ctx, st)) == before {
		t.Error("a reclaim wrote no event; the ownership change is the audit substrate")
	}

	after = mustEvents(t, ctx, st)
	var sawRetitle bool
	for _, e := range after {
		// The epic's own create is the one event here that is not the issue's.
		if e.IssueID == epic.ID {
			continue
		}
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

// everyEditableFieldRecordsHistory holds the field axis to its own promise:
// every field a patch can write, history reports. Not "the fields somebody
// remembered" — every one of them.
//
// So this walks the whole editable set rather than any one field. A per-field
// case passes for the fields that work and is never written for the one that
// does not.
func everyEditableFieldRecordsHistory(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	newType := model.TypeBug
	priority := model.PriorityUrgent
	labels := []string{"beta"}
	title, description, prompt, assignee, lane := "retitled", "described", "render at 1024x768", "ada", "left"
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})

	// One patch per field, each moving exactly that field, so a recorded row
	// can only have come from the field the case names.
	for _, edit := range []struct {
		field string
		patch storage.UpdateIssueInput
		want  string
	}{
		{"title", storage.UpdateIssueInput{Title: &title}, "retitled"},
		{"description", storage.UpdateIssueInput{Description: &description}, "described"},
		{"prompt", storage.UpdateIssueInput{Prompt: &prompt}, "render at 1024x768"},
		{"issue_type", storage.UpdateIssueInput{IssueType: &newType}, "bug"},
		{"priority", storage.UpdateIssueInput{Priority: &priority}, "1"},
		{"assignee", storage.UpdateIssueInput{Assignee: &assignee}, "ada"},
		{"lane", storage.UpdateIssueInput{Lane: &lane}, "left"},
		{"labels", storage.UpdateIssueInput{Labels: &labels}, "beta"},
	} {
		issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "recorded", Topic: "core", IssueType: model.TypeTask, ParentID: epic.ID})
		if _, err := st.Apply(ctx, issue.ID, storage.Change{Actor: "ada", Fields: edit.patch}); err != nil {
			t.Errorf("Apply %s patch error = %v", edit.field, err)
			continue
		}
		events, err := st.ListEvents(ctx, issue.ID)
		if err != nil {
			t.Fatalf("ListEvents after %s patch error = %v", edit.field, err)
		}
		var got []model.FieldChange
		for _, event := range events {
			got = append(got, event.Changes...)
		}
		index := slices.IndexFunc(got, func(c model.FieldChange) bool { return c.Field == edit.field })
		if index < 0 {
			t.Errorf("patching %s recorded %v; the write left no history of itself", edit.field, got)
			continue
		}
		if row := got[index]; row.To != edit.want {
			t.Errorf("%s change row To = %q, want %q", edit.field, row.To, edit.want)
		}
	}
}

// aReportedHandoverIsTheOneRecorded holds Apply's report to the history it
// wrote. A caller announces "claim transferred: X -> Y" from the handover
// Apply returns, so the handover has to name the holder the record named
// before the start and the holder the start recorded — each half checked
// against the events, not against a second reading of the claim rule.
//
// The walk crosses every way a start can stand toward a claim: the first start
// of a ticket whose assignee was preset, which takes it from nobody; the holder
// starting it again; a second checkout taking it under the same assignee, so
// only the checkout moves; that checkout handing it to a new assignee, so only
// the assignee moves; and a done, which records a write and takes nothing.
func aReportedHandoverIsTheOneRecorded(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "contested", Topic: "core", ParentID: epic.ID, Assignee: "ada"})
	start := func(assignee string) storage.Applied {
		t.Helper()
		applied, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: assignee}, Actor: assignee})
		if err != nil {
			t.Fatalf("Apply start as %s error = %v", assignee, err)
		}
		return applied
	}
	// recordedStart is the newest start event the stream wrote, found by
	// attribution rather than by position among the issue's events.
	recordedStart := func(stream string) model.IssueEvent {
		t.Helper()
		events, err := st.ListEvents(ctx, issue.ID)
		if err != nil {
			t.Fatalf("ListEvents error = %v", err)
		}
		var found []model.IssueEvent
		for _, event := range events {
			if event.Action == string(model.ActionStart) && event.Attribution.Stream() == stream {
				found = append(found, event)
			}
		}
		if len(found) == 0 {
			t.Fatalf("no start event carries stream %q among %+v", stream, events)
		}
		return found[len(found)-1]
	}

	st.AttributeTo("checkout-a")
	if first := start("ada"); first.Handover != nil {
		t.Errorf("the first start of a preset assignee reported handover %+v; nobody had taken the ticket to hand it over", *first.Handover)
	}
	byA := recordedStart("checkout-a")
	if again := start("ada"); again.Handover != nil {
		t.Errorf("the holder starting its own ticket again reported handover %+v", *again.Handover)
	}

	st.AttributeTo("checkout-b")
	taken := start("ada")
	byB := recordedStart("checkout-b")
	if byB.Attribution == byA.Attribution {
		t.Fatalf("both start events carry %+v; the store did not record the second checkout", byA.Attribution)
	}
	want := claims.Handover{
		From: claims.Claimant{Established: true, Assignee: "ada", Checkout: byA.Attribution},
		To:   claims.Claimant{Established: true, Assignee: "ada", Checkout: byB.Attribution},
	}
	if taken.Handover == nil || *taken.Handover != want {
		t.Errorf("a second checkout's takeover reported %+v, want %+v: the checkouts on the two recorded start events", taken.Handover, want)
	}

	handed := start("grace")
	var assigneeRow model.FieldChange
	for _, change := range recordedStart("checkout-b").Changes {
		if change.Field == "assignee" {
			assigneeRow = change
		}
	}
	if assigneeRow != (model.FieldChange{Field: "assignee", From: "ada", To: "grace"}) {
		t.Fatalf("the hand-off to grace recorded assignee row %+v, want ada -> grace", assigneeRow)
	}
	want = claims.Handover{
		From: claims.Claimant{Established: true, Assignee: assigneeRow.From, Checkout: byB.Attribution},
		To:   claims.Claimant{Established: true, Assignee: assigneeRow.To, Checkout: byB.Attribution},
	}
	if handed.Handover == nil || *handed.Handover != want {
		t.Errorf("a hand-off to a new assignee reported %+v, want %+v: the assignee row the start recorded", handed.Handover, want)
	}

	done, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Done{}, Actor: "grace"})
	if err != nil {
		t.Fatalf("Apply done error = %v", err)
	}
	if done.Handover != nil {
		t.Errorf("a done reported handover %+v; finishing a ticket takes nothing", *done.Handover)
	}
}

// takeoverByANewCheckoutRecords is the two-checkout takeover reduced to what a
// storage engine can see: one store, told it is a different checkout between
// two starts, with the SAME assignee named both times.
//
// That sameness is the whole test, and it is the common case rather than a
// corner one. A checkout driving no agent session resolves no assignee at all,
// so every checkout one person runs starts work under the identical (empty)
// identity, and two worktrees of one agent session share theirs. An engine that
// decides "did anything change" on the assignee alone reads the second
// checkout's start as a repeat of the first, writes nothing, and exits 0 — so
// the lane stays with the first checkout while the second believes it holds it,
// and both work the same ticket. [LAW:no-silent-failure]
func takeoverByANewCheckoutRecords(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "contested", Topic: "core", ParentID: epic.ID})

	st.AttributeTo("checkout-a")
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply start from checkout a error = %v", err)
	}
	before := len(mustEvents(t, ctx, st))

	// The holding checkout starting again still records nothing: the claimant
	// it would install is the one already standing, on both halves.
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply repeated start from checkout a error = %v", err)
	}
	if repeated := len(mustEvents(t, ctx, st)); repeated != before {
		t.Errorf("a start from the holding checkout wrote %d events, want none", repeated-before)
	}

	st.AttributeTo("checkout-b")
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply takeover start from checkout b error = %v", err)
	}
	// Found by attribution rather than by position: the assertion is that the
	// taking checkout is ON the record, which is what a claim derivation reads,
	// and that is true regardless of how two same-instant events sort.
	var takeover model.IssueEvent
	var found bool
	for _, event := range mustEvents(t, ctx, st) {
		if event.Attribution.Stream() == "checkout-b" {
			takeover, found = event, true
		}
	}
	if !found {
		t.Fatal("a takeover by a second checkout wrote no event carrying its stream: the record still names the first checkout, and nothing says otherwise")
	}
	if takeover.IssueID != issue.ID {
		t.Errorf("takeover event issue = %q, want %q", takeover.IssueID, issue.ID)
	}
	// The verb matters as much as the stream: claim derivation transfers a lane
	// only on an establishing verb, so a takeover recorded under any other one
	// would leave the lane where it was.
	if takeover.Action != string(model.ActionStart) {
		t.Errorf("takeover event action = %q, want %q", takeover.Action, model.ActionStart)
	}
}

// oneIssuesHistoryMatchesTheWholeLog holds ListEvents to the promise its
// contract doc makes: one issue's events, "ordered exactly as ListAllEvents
// orders the whole of it". Two engines answer it independently — the memory one
// reads the issue's own run of history, the SQL one queries with a predicate —
// and a divergence (the wrong issue's rows, an unsorted return, a dropped
// ordering key) would otherwise surface only as strange claim-transfer behavior
// three layers up, never as a failure against the contract that promised it.
//
// The expectation is DERIVED from ListAllEvents rather than written down,
// because agreeing with ListAllEvents is the entire promise: a hand-written
// sequence would agree with neither reader the moment an engine changed what it
// records, and would then be pinning this test's memory of the engine rather
// than the contract. [LAW:one-source-of-truth]
func oneIssuesHistoryMatchesTheWholeLog(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// Two pinned instants, the LATER one recorded first: the history arrives
	// out of instant order, as one merged from two checkouts does. A reader
	// returning its rows as recorded fails here for certain, and so does one
	// comparing the stamps' spelling, which orders this pair backwards. Inside
	// each group every event shares one stamp, so only the id tie-break orders
	// it. Ids are minted at random, so a reader ordering by id alone escapes
	// only when every earlier-instant id of the subject's happens to sort below
	// every later-instant one: seven of each, 1 run in C(14,7) = 3432.
	//
	// The creates are inside the later group, not before it: an unpinned create
	// would stamp the real instant, later than either pinned one.
	clk.Pin(laterInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	subject := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "subject", Topic: "core", ParentID: epic.ID})
	other := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "other", Topic: "core", ParentID: epic.ID})

	// Interleaved on purpose. A reader that returns the wrong issue's events,
	// and one that returns the right issue's in the wrong order, both survive a
	// history in which one issue's events happen to be contiguous.
	for _, group := range []struct {
		at     time.Time
		titles []string
	}{
		{laterInstant, []string{"one", "two", "three", "four", "five", "six"}},
		{earlierInstant, []string{"seven", "eight", "nine", "ten", "eleven", "twelve"}},
	} {
		clk.Pin(group.at)
		for _, title := range group.titles {
			for _, id := range []string{subject.ID, other.ID} {
				if _, err := st.Apply(ctx, id, storage.Change{Fields: storage.UpdateIssueInput{Title: &title}, Actor: "conformance"}); err != nil {
					t.Fatalf("Apply title %q to %s error = %v", title, id, err)
				}
			}
		}
	}
	// A lifecycle verb as well as field edits, so the history under test is the
	// mixed kind a claimant is actually read out of.
	for _, id := range []string{subject.ID, other.ID} {
		if _, err := st.Apply(ctx, id, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
			t.Fatalf("Apply start to %s error = %v", id, err)
		}
	}

	whole := mustEvents(t, ctx, st)
	want := sameIssueInOrder(whole, subject.ID)
	if len(want) == len(whole) {
		t.Fatalf("all %d logged events belong to %s: the case seeded nothing for a filter to exclude, so it cannot tell a filtered reader from an unfiltered one", len(whole), subject.ID)
	}

	got, err := st.ListEvents(ctx, subject.ID)
	if err != nil {
		t.Fatalf("ListEvents error = %v", err)
	}
	if !slices.EqualFunc(got, want, func(a, b model.IssueEvent) bool { return a.ID == b.ID }) {
		t.Errorf("ListEvents(%s) disagrees with the whole log narrowed to that issue:\n  listed = %v\n  narrowed = %v",
			subject.ID, eventIDsOf(got), eventIDsOf(want))
	}
}

// sameIssueInOrder narrows the whole log to one issue and puts it in the order
// the contract names — oldest first, ties broken by id. Sorting a slice that
// arrived sorted is a no-op; stating the order here is what keeps the
// expectation from inheriting ListAllEvents' ordering as an assumption instead
// of asserting the contract's own rule.
func sameIssueInOrder(all []model.IssueEvent, issueID string) []model.IssueEvent {
	mine := make([]model.IssueEvent, 0, len(all))
	for _, event := range all {
		if event.IssueID == issueID {
			mine = append(mine, event)
		}
	}
	slices.SortFunc(mine, storage.EventOrdering)
	return mine
}

func listDefaultsToRankOrder(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first", Topic: "core", ParentID: epic.ID})
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second", Topic: "core", ParentID: epic.ID})
	third := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "third", Topic: "core", ParentID: epic.ID})

	// An unsorted listing is reproducible, not merely arbitrary: rank ascending
	// with ties broken by id, the container ahead of what it holds. Callers
	// rely on it — `lit backlog` is this order.
	assertOrder(t, ctx, st, "default listing", epic.ID, first.ID, second.ID, third.ID)
}

func listFiltersSelect(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The epic that homes both leaves is a row like any other: it lists wherever
	// its own type, its derived state (in_progress, once the task starts) and
	// its stamps match, and nowhere a field it does not carry is asked for.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "gamma home", Topic: "core", IssueType: model.TypeEpic})
	bug := mustCreate(t, ctx, st, storage.CreateIssueInput{
		Title: "alpha widget", Topic: "renderer", IssueType: model.TypeBug, Assignee: "ada", Labels: []string{"perf", "ui"}, ParentID: epic.ID,
	})
	task := mustCreate(t, ctx, st, storage.CreateIssueInput{
		Title: "beta gadget", Topic: "parser", IssueType: model.TypeTask, Assignee: "grace", ParentID: epic.ID,
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
		{"by status", storage.ListIssuesFilter{Statuses: []model.State{model.StateInProgress}}, []string{epic.ID, task.ID}},
		{"by type", storage.ListIssuesFilter{IssueTypes: []model.IssueType{model.TypeBug}}, []string{bug.ID}},
		{"excluding a type", storage.ListIssuesFilter{ExcludeIssueTypes: []model.IssueType{model.TypeBug}}, []string{epic.ID, task.ID}},
		{"by assignee", storage.ListIssuesFilter{Assignees: []string{"grace"}}, []string{task.ID}},
		{"by id", storage.ListIssuesFilter{IDs: []string{bug.ID}}, []string{bug.ID}},
		{"by search term", storage.ListIssuesFilter{SearchTerms: []string{"widget"}}, []string{bug.ID}},
		{"search matches topic too", storage.ListIssuesFilter{SearchTerms: []string{"parser"}}, []string{task.ID}},
		{"by all labels", storage.ListIssuesFilter{LabelsAll: []string{"perf", "ui"}}, []string{bug.ID}},
		{"by a label the issue lacks", storage.ListIssuesFilter{LabelsAll: []string{"perf", "absent"}}, nil},
		{"by comment presence", storage.ListIssuesFilter{HasComments: &hasComments}, []string{bug.ID}},
		{"updated before the future", storage.ListIssuesFilter{UpdatedBefore: &future}, []string{epic.ID, bug.ID, task.ID}},
		{"updated after the future", storage.ListIssuesFilter{UpdatedAfter: &future}, nil},
		{"updated after the past", storage.ListIssuesFilter{UpdatedAfter: &past}, []string{epic.ID, bug.ID, task.ID}},
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

func listByParent(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	other := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "other epic", Topic: "core", IssueType: model.TypeEpic})
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first", Topic: "core", ParentID: epic.ID})
	// The second child is itself an epic: only an epic holds children, so it is
	// the one kind of child that can parent the grandchild.
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second", Topic: "core", IssueType: model.TypeEpic, ParentID: epic.ID})
	grandchild := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "grandchild", Topic: "core", ParentID: second.ID})
	cousin := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "cousin", Topic: "core", ParentID: other.ID})
	// A top-level epic nobody below names, so a parent filter has something to
	// leave out.
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "loose epic", Topic: "core", IssueType: model.TypeEpic})
	if _, err := st.Apply(ctx, first.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply start error = %v", err)
	}

	for _, tc := range []struct {
		name   string
		filter storage.ListIssuesFilter
		want   []string
	}{
		// Direct children only, in the canonical rank order: the grandchild
		// belongs to second, not to the epic.
		{"direct children of one parent", storage.ListIssuesFilter{ParentIDs: []string{epic.ID}}, []string{first.ID, second.ID}},
		{"a child can itself be a parent", storage.ListIssuesFilter{ParentIDs: []string{second.ID}}, []string{grandchild.ID}},
		{"a leaf parent has no children", storage.ListIssuesFilter{ParentIDs: []string{cousin.ID}}, nil},
		{"parents union", storage.ListIssuesFilter{ParentIDs: []string{epic.ID, other.ID}}, []string{first.ID, second.ID, cousin.ID}},
		{"parent intersects another axis", storage.ListIssuesFilter{ParentIDs: []string{epic.ID}, Statuses: []model.State{model.StateInProgress}}, []string{first.ID}},
	} {
		assertIssueIDs(t, tc.name, mustList(t, ctx, st, tc.filter), tc.want)
	}

	// An unknown parent is not a parent with no children: the listing says so
	// rather than answering with the empty list a childless parent gets.
	_, err := st.ListIssues(ctx, storage.ListIssuesFilter{ParentIDs: []string{epic.ID, "no-such-issue"}})
	assertNotFound(t, err, "issue", "ListIssues under a missing parent")
}

func listHidesArchivedAndDeleted(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The epic stays in the flow throughout, so it leads every listing below
	// whatever retention state its children are in.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	live := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "live", Topic: "core", ParentID: epic.ID})
	archived := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "archived", Topic: "core", ParentID: epic.ID})
	deleted := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "deleted", Topic: "core", ParentID: epic.ID})
	if _, err := st.Apply(ctx, archived.ID, storage.Change{Action: model.Archive{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply archive error = %v", err)
	}
	if _, err := st.Apply(ctx, deleted.ID, storage.Change{Action: model.Delete{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply delete error = %v", err)
	}

	// Retention is the one axis whose default is a filter rather than an
	// absence: saying nothing means "only what is in the flow".
	assertIssueIDs(t, "default listing", mustList(t, ctx, st, storage.ListIssuesFilter{}), []string{epic.ID, live.ID})
	assertIssueIDs(t, "including archived",
		mustList(t, ctx, st, storage.ListIssuesFilter{IncludeArchived: true}), []string{epic.ID, live.ID, archived.ID})
	assertIssueIDs(t, "including deleted",
		mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true}), []string{epic.ID, live.ID, deleted.ID})
	assertIssueIDs(t, "including both",
		mustList(t, ctx, st, storage.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true}),
		[]string{epic.ID, live.ID, archived.ID, deleted.ID})
}

func listSortsAndLimits(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// Titled past its children so a title sort puts it at one end or the other
	// and never between them.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "d", Topic: "core", IssueType: model.TypeEpic})
	b := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "b", Topic: "core", ParentID: epic.ID})
	a := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "a", Topic: "core", ParentID: epic.ID})
	c := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c", Topic: "core", ParentID: epic.ID})

	assertIssueIDs(t, "sorted by title ascending",
		mustList(t, ctx, st, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "title"}}}),
		[]string{a.ID, b.ID, c.ID, epic.ID})
	assertIssueIDs(t, "sorted by title descending",
		mustList(t, ctx, st, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "title", Desc: true}}}),
		[]string{epic.ID, c.ID, b.ID, a.ID})

	// Limit truncates the ordered result rather than sampling it, so a limited
	// listing is the head of the unlimited one.
	assertIssueIDs(t, "limited to two", mustList(t, ctx, st, storage.ListIssuesFilter{Limit: 2}), []string{epic.ID, b.ID})
	// Zero is not a limit of zero; it is the absence of a limit.
	assertIssueIDs(t, "unlimited", mustList(t, ctx, st, storage.ListIssuesFilter{Limit: 0}), []string{epic.ID, b.ID, a.ID, c.ID})

	if _, err := st.ListIssues(ctx, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "nonsense"}}}); err == nil {
		t.Error("sorting by an unknown field succeeded; want an error")
	}
}

// listBreaksSortTiesByID pins the trailing id key. Every other sort case in
// this suite uses distinct values, which is exactly why none of them can catch
// an engine that leaves tied rows in whatever order it held them — and two
// engines holding them differently is what the differential oracle would read
// as divergence.
func listBreaksSortTiesByID(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// One title across four issues, the epic included: the sort key cannot
	// separate them, so the contract's tie-break is the only thing deciding the
	// order.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "same", Topic: "core", IssueType: model.TypeEpic})
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "same", Topic: "core", ParentID: epic.ID})
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "same", Topic: "core", ParentID: epic.ID})
	third := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "same", Topic: "core", ParentID: epic.ID})

	ascending := []string{epic.ID, first.ID, second.ID, third.ID}
	slices.Sort(ascending)
	assertIssueIDs(t, "tied titles ordered by id",
		mustList(t, ctx, st, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "title"}}}),
		ascending)

	// Descending reverses the named key, never the tie-break: id stays
	// ascending, so a caller paging a descending listing sees a stable order.
	assertIssueIDs(t, "tied titles descending still ordered by id",
		mustList(t, ctx, st, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "title", Desc: true}}}),
		ascending)

	// The id key itself, descending: the one listing the trailing id key cannot
	// answer for it, since that key would put the rows back in ascending order.
	assertIssueIDs(t, "sorted by id descending",
		mustList(t, ctx, st, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "id", Desc: true}}}),
		[]string{ascending[3], ascending[2], ascending[1], ascending[0]})
}

// listOrdersByEachValueKey pins what each key that reads one of an issue's
// own values compares, one pair per key, against a literal order.
//
// Inside a pair every other value a key could read is equal, and the pair is
// created in the reverse of the order the key puts it in — at the earlier
// instant first — so rank and both timestamps all disagree with the key. Each
// listing takes created_at as its second key, in the key's own direction, so a
// comparison that returns nothing, or reads any other value, hands the order
// to created_at and gets it backwards both ways.
//
// Where a key's comparison has a way to go wrong that still separates the
// pair, the values are chosen to hit it. Byte-wise, an uppercase letter sorts
// before every lowercase one, so a case-folding collation reverses "Zeta" and
// "alpha", and "Zed" and "ada". "bug" sorts before "task" as text and after it
// in model.IssueTypes, the vocabulary's declared order.
func listOrdersByEachValueKey(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "keys", IssueType: model.TypeEpic})
	for _, row := range []struct {
		key string
		// first is the issue the key orders first ascending.
		first, second storage.CreateIssueInput
	}{
		{"title",
			storage.CreateIssueInput{Title: "Zeta", Topic: "keys"},
			storage.CreateIssueInput{Title: "alpha", Topic: "keys"}},
		{"assignee",
			storage.CreateIssueInput{Title: "pair", Topic: "keys", Assignee: "Zed"},
			storage.CreateIssueInput{Title: "pair", Topic: "keys", Assignee: "ada"}},
		{"topic",
			storage.CreateIssueInput{Title: "pair", Topic: "alpha"},
			storage.CreateIssueInput{Title: "pair", Topic: "beta"}},
		{"type",
			storage.CreateIssueInput{Title: "pair", Topic: "keys", IssueType: model.TypeBug},
			storage.CreateIssueInput{Title: "pair", Topic: "keys", IssueType: model.TypeTask}},
		{"priority",
			storage.CreateIssueInput{Title: "pair", Topic: "keys", Priority: model.PriorityNormal},
			storage.CreateIssueInput{Title: "pair", Topic: "keys", Priority: model.PriorityUrgent}},
	} {
		row.first.ParentID, row.second.ParentID = epic.ID, epic.ID
		clk.Pin(earlierInstant)
		second := mustCreate(t, ctx, st, row.second)
		clk.Pin(laterInstant)
		first := mustCreate(t, ctx, st, row.first)

		pair := []string{first.ID, second.ID}
		assertIssueIDs(t, "sorted by "+row.key+" ascending",
			mustList(t, ctx, st, storage.ListIssuesFilter{IDs: pair, SortBy: []storage.SortSpec{{Field: row.key}, {Field: "created_at"}}}),
			[]string{first.ID, second.ID})
		assertIssueIDs(t, "sorted by "+row.key+" descending",
			mustList(t, ctx, st, storage.ListIssuesFilter{IDs: pair, SortBy: []storage.SortSpec{{Field: row.key, Desc: true}, {Field: "created_at", Desc: true}}}),
			[]string{second.ID, first.ID})
	}
}

// listSortsStatusByDerivedState pins the one sort key that does not read a
// field. "status" orders the derived lifecycle state — the same reading the
// listing's status FILTER takes — so one listing means one thing by the word,
// and a container, holding no stored status at all, is the row that makes an
// engine which regressed to the column visible.
func listSortsStatusByDerivedState(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The two leaves live in their own epic, which the listings below leave out
	// along with the child, so the epic under test is the only container read.
	home := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "home", Topic: "core", IssueType: model.TypeEpic})
	closedLeaf := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "closed leaf", Topic: "core", ParentID: home.ID})
	openLeaf := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "open leaf", Topic: "core", ParentID: home.ID})
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})

	if _, err := st.Apply(ctx, closedLeaf.ID, storage.Change{Action: model.Done{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply done error = %v", err)
	}
	if _, err := st.Apply(ctx, child.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply start error = %v", err)
	}
	assertState(t, ctx, st, epic.ID, model.StateInProgress, "its only child is in progress")

	// The child is excluded from the listing so the epic shares its bucket with
	// nothing: every row here derives a DIFFERENT state, so the epic's position
	// is decided by the sort key alone and no id tie-break can supply the right
	// answer by luck. Reading the stored column instead puts the epic at an END
	// of the listing — leading ascending on its absent value, trailing
	// descending — so both directions below fail loudly rather than one of them
	// passing either way.
	onlyLeavesAndEpic := []string{closedLeaf.ID, openLeaf.ID, epic.ID}

	// "closed" < "in_progress" < "open" as tokens, and the epic derives
	// in_progress, so it sorts BETWEEN the two leaves in both directions.
	assertIssueIDs(t, "container sorts among the leaves ascending",
		mustList(t, ctx, st, storage.ListIssuesFilter{IDs: onlyLeavesAndEpic, SortBy: []storage.SortSpec{{Field: "status"}}}),
		[]string{closedLeaf.ID, epic.ID, openLeaf.ID})

	assertIssueIDs(t, "container sorts among the leaves descending",
		mustList(t, ctx, st, storage.ListIssuesFilter{IDs: onlyLeavesAndEpic, SortBy: []storage.SortSpec{{Field: "status", Desc: true}}}),
		[]string{openLeaf.ID, epic.ID, closedLeaf.ID})
}

// stampsComeFromTheClock states the clock seam itself: an engine stamps what
// the clock it was built with reads, not what the wall clock reads.
//
// It is the case every other pinned case rests on. An engine that took a clock
// and ignored it would still pass most of them — pin two instants in creation
// order and a real clock produces the same relative order by itself — so
// without this one, "the suite pins time" would be a claim about the suite
// rather than about the engine. Here the pinned instant is nowhere near now,
// so a wall-clock stamp cannot coincide with it. [LAW:verifiable-goals]
func stampsComeFromTheClock(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(earlierInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "clock", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "stamped", Topic: "clock", ParentID: epic.ID})

	// Both columns, because a create reads the clock once and an engine that
	// stamped only one of them from it would be half-honoring the seam.
	if !issue.CreatedAt.Equal(earlierInstant) {
		t.Errorf("CreatedAt = %s, want the pinned %s", issue.CreatedAt, earlierInstant)
	}
	if !issue.UpdatedAt.Equal(earlierInstant) {
		t.Errorf("UpdatedAt = %s, want the pinned %s", issue.UpdatedAt, earlierInstant)
	}

	// And it is the STORED stamp, not something the create's return value
	// carried out of a value the engine never wrote.
	read, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue error = %v", err)
	}
	if !read.CreatedAt.Equal(earlierInstant) {
		t.Errorf("stored CreatedAt = %s, want the pinned %s", read.CreatedAt, earlierInstant)
	}
}

// listOrdersTimestampsByInstant states that a timestamp sort key orders by the
// instant a stamp denotes, never by the text an engine happens to hold it as.
//
// Each column's pair of stamps is spelled the way [earlierInstant] describes,
// so text order is the reverse of instant order in both. The two columns order
// the pair opposite ways, and each listing takes the other column as its
// second key, in the same direction: a key that compares nothing, or reads the
// other column, hands the order to that second key and gets it backwards in
// both directions.
func listOrdersTimestampsByInstant(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The updates' pair repeats the creates' trap past both of them: ".13Z"
	// sorts after ".1300001Z" as text while denoting the earlier instant.
	touchedInstant := time.Date(2026, 1, 1, 0, 0, 0, 130_000_000, time.UTC)
	retouchedInstant := time.Date(2026, 1, 1, 0, 0, 0, 130_000_100, time.UTC)

	// The later instant is created FIRST, so recording order and instant order
	// disagree. Creating them in instant order would let an engine that ignored
	// the clock entirely pass — its real stamps would land in the same relative
	// order the case asserts — and the case would be pinning nothing.
	// CreateIssue stamps created_at and updated_at from one read of the clock;
	// the description patches below then move updated_at alone. The epic is
	// stamped by the live clock, years past every pinned instant, and the
	// listings are narrowed to the pair so it decides nothing below.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "clock", IssueType: model.TypeEpic})
	clk.Pin(laterInstant)
	newer := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "created later", Topic: "clock", ParentID: epic.ID})
	clk.Pin(earlierInstant)
	older := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "created earlier", Topic: "clock", ParentID: epic.ID})

	for _, touch := range []struct {
		issue   model.Issue
		instant time.Time
	}{{newer, touchedInstant}, {older, retouchedInstant}} {
		clk.Pin(touch.instant)
		description := "touched"
		if _, err := st.Apply(ctx, touch.issue.ID, storage.Change{Fields: storage.UpdateIssueInput{Description: &description}, Actor: "ada"}); err != nil {
			t.Fatalf("Apply description error = %v", err)
		}
	}

	pair := []string{older.ID, newer.ID}
	assertIssueIDs(t, "sorted by created_at ascending",
		mustList(t, ctx, st, storage.ListIssuesFilter{IDs: pair, SortBy: []storage.SortSpec{{Field: "created_at"}, {Field: "updated_at"}}}),
		[]string{older.ID, newer.ID})
	assertIssueIDs(t, "sorted by created_at descending",
		mustList(t, ctx, st, storage.ListIssuesFilter{IDs: pair, SortBy: []storage.SortSpec{{Field: "created_at", Desc: true}, {Field: "updated_at", Desc: true}}}),
		[]string{newer.ID, older.ID})
	assertIssueIDs(t, "sorted by updated_at ascending",
		mustList(t, ctx, st, storage.ListIssuesFilter{IDs: pair, SortBy: []storage.SortSpec{{Field: "updated_at"}, {Field: "created_at"}}}),
		[]string{newer.ID, older.ID})
	assertIssueIDs(t, "sorted by updated_at descending",
		mustList(t, ctx, st, storage.ListIssuesFilter{IDs: pair, SortBy: []storage.SortSpec{{Field: "updated_at", Desc: true}, {Field: "created_at", Desc: true}}}),
		[]string{older.ID, newer.ID})
}

// eventsAreTotallyOrdered pins the history ordering to (created_at, id), and
// pins each key against a run of events the other one cannot separate.
//
// Two pinned instants, several events inside each. Across the groups only the
// timestamp key can order them, and it does so against the pair whose text
// order is the reverse of their instant order, so an engine comparing the
// spelling fails here as well as in the listing. Inside a group every event
// shares one stamp, so only the id tie-break is left — which is the whole
// reason the rule names a second key, and it is unreachable unless the case
// decides what time it is: on a live nanosecond clock the tie this settles
// simply never occurs, and an engine returning raw recording order passes.
func eventsAreTotallyOrdered(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The creates are inside the first group, not before it: an unpinned create
	// would stamp the real instant, which is later than either pinned one, and
	// the sequence would step backwards for a reason unrelated to the rule.
	clk.Pin(earlierInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "audited", Topic: "core", ParentID: epic.ID})
	for _, group := range []struct {
		at     time.Time
		titles []string
	}{
		{earlierInstant, []string{"first", "second", "third"}},
		{laterInstant, []string{"fourth", "fifth", "sixth"}},
	} {
		clk.Pin(group.at)
		for _, title := range group.titles {
			if _, err := st.Apply(ctx, issue.ID, storage.Change{Fields: storage.UpdateIssueInput{Title: &title}, Actor: "conformance"}); err != nil {
				t.Fatalf("apply %q: %v", title, err)
			}
		}
	}

	events, err := st.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents: %v", err)
	}
	if len(events) < 7 {
		t.Fatalf("got %d events, want at least the create plus six changes", len(events))
	}
	// Each key is asserted on its own rather than through one comparator, and
	// deliberately not through [storage.EventOrdering]: that is what both
	// engines sort by, so a case checking their output against it could not
	// fail on a change to the rule itself — reverse it and engines and oracle
	// reverse together. [LAW:behavior-not-structure]
	for i := 1; i < len(events); i++ {
		prev, cur := events[i-1], events[i]
		if cur.CreatedAt.Before(prev.CreatedAt) {
			t.Errorf("events step backwards in time at index %d: %s after %s", i, cur.CreatedAt, prev.CreatedAt)
		}
		if cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID <= prev.ID {
			t.Errorf("events sharing an instant are not id-ascending at index %d: %s after %s", i, cur.ID, prev.ID)
		}
	}
}

func rankIntentsReorder(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// Three epics: the top level holds only epics, so they are the frame-mates
	// a top-level reorder is read against.
	a := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "a", Topic: "core", IssueType: model.TypeEpic})
	b := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "b", Topic: "core", IssueType: model.TypeEpic})
	c := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c", Topic: "core", IssueType: model.TypeEpic})
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

	end, err := st.RankToTop(ctx, b.ID)
	if err != nil {
		t.Fatalf("RankToTop error = %v", err)
	}
	// These three are top-level, so the frame the move was scoped to is the
	// top level and there is no substitution for a caller to report.
	if end.Frame != storage.TopLevel {
		t.Errorf("RankToTop reported frame %q, want the top level", end.Frame)
	}
	if !end.Moved {
		t.Error("RankToTop reported no move, but b was not at the top")
	}
	assertOrder(t, ctx, st, "after RankToTop", b.ID, a.ID, c.ID)

	// Ranking to an edge the issue already holds changes nothing, and the
	// contract is that the caller is told so rather than handed a success it
	// cannot distinguish from a real move. [LAW:no-silent-failure]
	end, err = st.RankToTop(ctx, b.ID)
	if err != nil {
		t.Fatalf("RankToTop of the top issue error = %v", err)
	}
	if end.Moved {
		t.Error("RankToTop of the already-top issue reported a move; want none")
	}
	assertOrder(t, ctx, st, "after a no-op RankToTop", b.ID, a.ID, c.ID)

	end, err = st.RankToBottom(ctx, b.ID)
	if err != nil {
		t.Fatalf("RankToBottom error = %v", err)
	}
	if !end.Moved {
		t.Error("RankToBottom reported no move, but b was at the top")
	}
	assertOrder(t, ctx, st, "after RankToBottom", a.ID, c.ID, b.ID)

	// An intent naming an issue that is not there is an error, never a
	// silently-dropped reorder.
	if _, err := st.RankAbove(ctx, a.ID, "no-such-issue"); err == nil {
		t.Error("RankAbove a missing anchor succeeded; want an error")
	}
	if _, err := st.RankToTop(ctx, "no-such-issue"); err == nil {
		t.Error("RankToTop of a missing issue succeeded; want an error")
	}
}

// rankToEdgeStaysInsideItsFrame is the contract behind `lit rank <child>
// --top`: the ends a child is sent to are its epic's, not the workspace's.
// Both engines must leave every issue outside the frame exactly where it was,
// and must name the frame they moved within so the caller can say which order
// changed.
func rankToEdgeStaysInsideItsFrame(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first", Topic: "core", ParentID: epic.ID})
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second", Topic: "core", ParentID: epic.ID})
	// The outsider is an epic: nothing but an epic sits at the top level beside
	// the one under test.
	standalone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "standalone epic", Topic: "core", IssueType: model.TypeEpic})

	end, err := st.RankToTop(ctx, second.ID)
	if err != nil {
		t.Fatalf("RankToTop of a child error = %v", err)
	}
	// The frame is the epic, not the top level: a caller printing "moved to the
	// top" without this would be describing a promotion that did not happen.
	if end.Frame != storage.Frame(epic.ID) {
		t.Errorf("RankToTop of a child reported frame %q, want the epic %q", end.Frame, epic.ID)
	}
	if !end.Moved {
		t.Error("RankToTop of the second child reported no move; want one")
	}
	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertPrecedes(t, listed, second.ID, first.ID)
	// The epic itself never moved, so the standalone filed after it still
	// follows it. A child's edge move that reshuffles the top level is the
	// defect this case exists for.
	assertPrecedes(t, listed, epic.ID, standalone.ID)

	// The child now leads its frame, so the same intent is a no-op — and the
	// epic keeps its own place either way.
	end, err = st.RankToTop(ctx, second.ID)
	if err != nil {
		t.Fatalf("repeat RankToTop of a child error = %v", err)
	}
	if end.Moved {
		t.Error("RankToTop of the leading child reported a move; want none")
	}
	if end.Frame != storage.Frame(epic.ID) {
		t.Errorf("no-op RankToTop reported frame %q, want the epic %q", end.Frame, epic.ID)
	}

	// An only child holds both ends of its frame at once: there is no
	// frame-mate its position is read against, so neither end can move it.
	lone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "lone epic", Topic: "core", IssueType: model.TypeEpic})
	only := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "only", Topic: "core", ParentID: lone.ID})
	for _, tc := range []struct {
		name string
		run  func() (storage.RankEnd, error)
	}{
		{"RankToTop", func() (storage.RankEnd, error) { return st.RankToTop(ctx, only.ID) }},
		{"RankToBottom", func() (storage.RankEnd, error) { return st.RankToBottom(ctx, only.ID) }},
	} {
		end, err := tc.run()
		if err != nil {
			t.Fatalf("%s of an only child error = %v", tc.name, err)
		}
		if end.Moved {
			t.Errorf("%s of an only child reported a move; want none", tc.name)
		}
		if end.Frame != storage.Frame(lone.ID) {
			t.Errorf("%s of an only child reported frame %q, want %q", tc.name, end.Frame, lone.ID)
		}
	}
}

// rankToEdgeIgnoresADeletedFrameMate pins that a deleted issue is not a
// frame-mate. Deleting an issue only flips its retention — the memory engine
// keeps its slot in the order forever — so an engine reading frame membership
// off slice position alone anchors against a row no view shows and reports
// stepping past something invisible. The SQL engine excludes it with
// `deleted_at IS NULL`; this case is what keeps the two answering alike.
func rankToEdgeIgnoresADeletedFrameMate(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	gone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "gone", Topic: "core", ParentID: epic.ID})
	lead := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "lead", Topic: "core", ParentID: epic.ID})
	tail := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "tail", Topic: "core", ParentID: epic.ID})

	// The deleted child holds the frame's first slot, which is precisely the row
	// an unfiltered edge lookup would anchor against.
	if _, err := st.Apply(ctx, gone.ID, storage.Change{Action: model.Delete{}, Actor: "tester"}); err != nil {
		t.Fatalf("delete the leading child: %v", err)
	}

	// lead already leads every child anyone can see, so the top edge is a no-op.
	// An engine counting the deleted child as a mate reports a move here — the
	// false success RankEnd.Moved exists to prevent.
	end, err := st.RankToTop(ctx, lead.ID)
	if err != nil {
		t.Fatalf("RankToTop of the leading live child error = %v", err)
	}
	if end.Moved {
		t.Error("RankToTop of the leading live child reported a move; the only child above it is deleted")
	}
	if end.Frame != storage.Frame(epic.ID) {
		t.Errorf("RankToTop reported frame %q, want the epic %q", end.Frame, epic.ID)
	}
	assertPrecedes(t, mustList(t, ctx, st, storage.ListIssuesFilter{}), lead.ID, tail.ID)
}

// createAtTopFilesInsideItsFrame is rankToEdgeStaysInsideItsFrame's creation
// twin: filing an issue at the top places it at the top of the frame it is
// filed into, never at the top of the workspace.
//
// Two epics are what make the defect visible to both engines. One epic cannot
// show it: a child filed at the top leads its siblings either way, and the
// engines represent rank too differently for the case to assert a key's value
// — the SQL store holds fractional strings, the memory engine holds positions.
// Filing at the top of two frames in turn is the observable they share. Keys
// taken from each frame leave the frames in the order they were created, so
// the first epic's new lead precedes the second's. Keys taken from the
// workspace's first rank chain off each other instead — each filing computes
// against the one before it and lands below it — so the second epic's lead
// overtakes the first's and the two frames come back inverted.
func createAtTopFilesInsideItsFrame(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// Filed in this order so that each frame's keys sit above the frame filed
	// before it, and a top-level epic holds the workspace's first key.
	outsider := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "outsider epic", Topic: "core", IssueType: model.TypeEpic})
	firstEpic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first epic", Topic: "core", IssueType: model.TypeEpic})
	firstChild := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first child", Topic: "core", ParentID: firstEpic.ID})
	secondEpic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second epic", Topic: "core", IssueType: model.TypeEpic})
	secondChild := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second child", Topic: "core", ParentID: secondEpic.ID})

	firstLead := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first lead", Topic: "core", ParentID: firstEpic.ID, Placement: storage.RankTop})
	secondLead := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second lead", Topic: "core", ParentID: secondEpic.ID, Placement: storage.RankTop})

	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	// Each new issue leads the siblings it was filed among. True of the defect
	// as well, and asserted anyway: it is what filing at the top is for, and a
	// scoping fix that lost it would be no fix.
	assertPrecedes(t, listed, firstLead.ID, firstChild.ID)
	assertPrecedes(t, listed, secondLead.ID, secondChild.ID)
	// The two frames keep the order they were filed in. This is the assertion
	// the defect fails.
	assertPrecedes(t, listed, firstLead.ID, secondLead.ID)
	// The default placement is untouched: it still files after everything that
	// exists, which is what keeps an authored batch in its file's order.
	assertPrecedes(t, listed, firstChild.ID, secondChild.ID)

	// The top level is a frame like any other, so an epic filed at the top
	// with no parent leads the epics that share that frame.
	topLead := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "top lead", Topic: "core", IssueType: model.TypeEpic, Placement: storage.RankTop})
	listed = mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertPrecedes(t, listed, topLead.ID, outsider.ID)
	assertPrecedes(t, listed, topLead.ID, firstEpic.ID)
}

// placingAtAFrameEdgeKeepsTheKeyBesideItsFrame is the case that separates
// "which key do I land beside" from "how much room is there beside it".
//
// The first question is about a frame: a child sent to the top of its epic
// leads its siblings and nobody else. The second is about the keyspace, and the
// keyspace is one order every frame shares. Answering the second with the
// first's scope — pairing the frame's leading key with an open bound, as though
// everything below it were free — asks for the middle of a range that is not
// empty, and hands back a key an issue outside the frame is already holding.
//
// The interleaving is what makes that reachable, and it is why this case moves
// a top-level issue before filing anything: it parks a top-level key underneath
// the epic, in the span an open bound claims is empty. Both engines are pinned
// by the same two observables — one rank per issue, and a child whose key sits
// inside its own epic's span rather than below every top-level issue. The
// memory engine has no keys to collide, and that is the point of asking it: its
// answer is the position, which is what the store's keys have to render.
func placingAtAFrameEdgeKeepsTheKeyBesideItsFrame(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})
	// The two top-level issues are epics, the only kind the top level holds.
	trailing := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "trailing", Topic: "core", IssueType: model.TypeEpic})
	leading := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "leading", Topic: "core", IssueType: model.TypeEpic, Placement: storage.RankTop})
	// Parks a top-level key between the two the frame's own placement would
	// otherwise treat as an empty span.
	if _, err := st.RankBelow(ctx, trailing.ID, leading.ID); err != nil {
		t.Fatalf("RankBelow(trailing, leading) error = %v", err)
	}

	lead := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "new lead", Topic: "core", ParentID: epic.ID, Placement: storage.RankTop})
	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertPrecedes(t, listed, lead.ID, child.ID)
	// The key belongs to the frame it was filed into: after the epic that holds
	// it, not below every top-level issue.
	assertPrecedes(t, listed, epic.ID, lead.ID)

	// The rank verb answers the same question and must answer it the same way.
	moved := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "moved", Topic: "core", ParentID: epic.ID})
	if _, err := st.RankToTop(ctx, moved.ID); err != nil {
		t.Fatalf("RankToTop(moved) error = %v", err)
	}
	listed = mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertPrecedes(t, listed, moved.ID, lead.ID)
	assertPrecedes(t, listed, epic.ID, moved.ID)
}

// createAtTopOfAnEmptyFrameTakesADistinctKey pins what filing at the top of a
// frame that holds nothing yet does — a case that looks like the one above and
// is not.
//
// An empty frame offers no member to lead and no key to sit beside, and its two
// ends name the same position, so the end a caller asked for cannot decide
// anything. What still has to hold is that the new rank belongs to that issue
// alone: an order in which two issues claim one rank is not an order, and it is
// what VerifyCandidate's rank law rejects when it rebuilds a workspace. Scoping
// the top edge to the frame is exactly what makes this reachable — the frame's
// edge reads as absent, and an engine answering "the middle of the keyspace"
// there hands the issue the key the workspace's first issue already holds. So
// an empty frame files beside the issue that frames it — just past its
// container's key, the one key such a frame does offer to sit beside, and a
// position nothing can already be using. Filing it after everything that exists
// answers the distinctness law too, but it reads as last rather than first in
// any view that orders an issue by its own position.
func createAtTopOfAnEmptyFrameTakesADistinctKey(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The first issue in a workspace holds the opening key — the very key a
	// "middle of the keyspace" answer would hand out a second time. It and the
	// trailing issue are epics, since only an epic sits at the top level.
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first", Topic: "core", IssueType: model.TypeEpic})
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "childless epic", Topic: "core", IssueType: model.TypeEpic})
	// Filed after the epic so that "beside the container" and "past everything"
	// are different positions. With the epic trailing the workspace the two
	// coincide, and the case cannot tell them apart.
	trailing := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "trailing", Topic: "core", IssueType: model.TypeEpic})
	only := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "only child", Topic: "core", ParentID: epic.ID, Placement: storage.RankTop})

	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertPrecedes(t, listed, first.ID, only.ID)
	assertPrecedes(t, listed, epic.ID, only.ID)
	// The first member of a frame lands beside the issue that frames it, not at
	// the end of the workspace. Filed at the end it would come back after
	// everything — which is what a view sorting a non-epic parent's child by
	// its own position would then show for an issue that asked for the top.
	assertPrecedes(t, listed, only.ID, trailing.ID)

	// The second child, filed at the top of a frame that now holds one, leads
	// it — the frame has an order to lead again.
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second child", Topic: "core", ParentID: epic.ID, Placement: storage.RankTop})
	listed = mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertPrecedes(t, listed, second.ID, only.ID)
}

// aRelativeMoveKeepsEveryRankDistinct is the four-command case: an epic, a
// top-level epic after it, and a child of the first epic after that, then the
// child ranked below the top-level epic.
//
// Rank pair resolution substitutes the epic for the child, which is the frame's
// say in the move — it picks the key the move lands beside. The room beside
// that key is not the frame's to answer: the only key past the anchor belongs
// to another frame, so a frame-scoped read finds nothing and calls the span
// open, and the midpoint of an open span is a key that other frame is holding.
//
// The memory engine cannot express that — it moves the epic to the slot after
// its anchor and every other position stands — so this case is the store being
// held to the order the memory engine already produces, and assertDistinctRanks
// is what names the failure for what it is rather than leaving the listing to
// an id tiebreak.
func aRelativeMoveKeepsEveryRankDistinct(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	standalone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "standalone epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})

	if _, err := st.RankBelow(ctx, child.ID, standalone.ID); err != nil {
		t.Fatalf("RankBelow(child, standalone) error = %v", err)
	}
	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertPrecedes(t, listed, standalone.ID, epic.ID)
	assertPrecedes(t, listed, epic.ID, child.ID)

	// The other end, against the same interleaving.
	if _, err := st.RankAbove(ctx, child.ID, standalone.ID); err != nil {
		t.Fatalf("RankAbove(child, standalone) error = %v", err)
	}
	listed = mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertPrecedes(t, listed, epic.ID, standalone.ID)
	assertPrecedes(t, listed, epic.ID, child.ID)
}

// rankSetStaysInsideItsFrame pins RankSet's anchor as the representatives' own
// frame. Setting an order among one epic's children leads that epic's children
// and moves nothing outside them.
//
// The assertion is on an outsider's rank rather than on the rendered order,
// because the rendered order cannot see this bug: children sort under their
// epic whatever keys they hold, so stacking them at the front of the whole
// workspace leaves the listing identical and only the keyspace wrong. An
// outsider whose rank changed is the observable that a write reached outside
// the frame it named.
func rankSetStaysInsideItsFrame(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	outsider := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "outsider epic", Topic: "core", IssueType: model.TypeEpic})
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	c1 := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c1", Topic: "core", ParentID: epic.ID})
	c2 := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c2", Topic: "core", ParentID: epic.ID})
	c3 := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c3", Topic: "core", ParentID: epic.ID})

	before := mustGet(t, ctx, st, outsider.ID).Rank
	epicBefore := mustGet(t, ctx, st, epic.ID).Rank

	if _, err := st.RankSet(ctx, []string{c3.ID, c1.ID}); err != nil {
		t.Fatalf("RankSet among siblings error = %v", err)
	}

	// The named order holds among the siblings, and the unnamed one keeps its
	// place behind them.
	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertPrecedes(t, listed, c3.ID, c1.ID)
	assertPrecedes(t, listed, c1.ID, c2.ID)

	// Nothing outside the frame was rewritten. Neither the epic nor an unrelated
	// top-level epic was named, so neither key may move.
	if after := mustGet(t, ctx, st, outsider.ID).Rank; after != before {
		t.Errorf("rank set among an epic's children moved an outside issue: %s rank %q -> %q", outsider.ID, before, after)
	}
	if after := mustGet(t, ctx, st, epic.ID).Rank; after != epicBefore {
		t.Errorf("rank set among an epic's children moved the epic itself: rank %q -> %q", epicBefore, after)
	}
}

// rankSetOverAWholeFramePermutesItsKeys pins what rank set writes: the keys
// its frame already held, handed round in the new order, and nothing else.
//
// The fixture is built so that minting keys instead has something to land on.
// Every child of the epic is named, so no unnamed sibling anchors the stack,
// and three top-level epics are ranked above the epic, onto keys below its
// own, where a stack walking downward from just past the epic reaches them. A
// rank set that computed fresh keys there wrote one of those top-level keys
// onto a child, and sorted two of the children above their own epic.
//
// The assertions are on keys, because the rendered order cannot see either
// failure: children list under their epic whatever keys they hold. The frame's
// keys as a set, and every key outside the frame, are the same before and
// after — which on the memory engine, whose keys are rendered positions, says
// the same thing as it does on the SQL one.
func rankSetOverAWholeFramePermutesItsKeys(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	// The outsider is the far wall past the epic's key, so the room just past
	// the epic is a closed gap rather than the open end of the keyspace.
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "outsider epic", Topic: "core", IssueType: model.TypeEpic})
	low := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "low", Topic: "core", IssueType: model.TypeEpic})
	mid := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "mid", Topic: "core", IssueType: model.TypeEpic})
	high := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "high", Topic: "core", IssueType: model.TypeEpic})
	if _, err := st.RankAbove(ctx, low.ID, epic.ID); err != nil {
		t.Fatalf("RankAbove(low, epic) error = %v", err)
	}
	if _, err := st.RankAbove(ctx, mid.ID, epic.ID); err != nil {
		t.Fatalf("RankAbove(mid, epic) error = %v", err)
	}
	if _, err := st.RankBelow(ctx, high.ID, low.ID); err != nil {
		t.Fatalf("RankBelow(high, low) error = %v", err)
	}
	children := []model.Issue{
		mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c1", Topic: "core", ParentID: epic.ID}),
		mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c2", Topic: "core", ParentID: epic.ID}),
		mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c3", Topic: "core", ParentID: epic.ID}),
	}
	frame := map[string]struct{}{}
	for _, child := range children {
		frame[child.ID] = struct{}{}
	}
	before := ranksByID(mustList(t, ctx, st, storage.ListIssuesFilter{}))

	named := []string{children[2].ID, children[0].ID, children[1].ID}
	if _, err := st.RankSet(ctx, named); err != nil {
		t.Fatalf("RankSet over every child error = %v", err)
	}

	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertIssueIDs(t, "the epic's children after rank set", childrenOf(listed, frame), named)
	after := ranksByID(listed)
	var keysBefore, keysAfter []string
	for id, key := range before {
		if _, inFrame := frame[id]; inFrame {
			keysBefore = append(keysBefore, key)
			keysAfter = append(keysAfter, after[id])
			continue
		}
		if after[id] != key {
			t.Errorf("rank set among an epic's children moved %s outside it: rank %q -> %q", id, key, after[id])
		}
	}
	slices.Sort(keysBefore)
	slices.Sort(keysAfter)
	if !slices.Equal(keysBefore, keysAfter) {
		t.Errorf("the epic's children hold keys %v after rank set, want the keys %v they held before, reordered", keysAfter, keysBefore)
	}
}

func ranksByID(issues []model.Issue) map[string]string {
	out := make(map[string]string, len(issues))
	for _, issue := range issues {
		out[issue.ID] = issue.Rank
	}
	return out
}

func childrenOf(issues []model.Issue, frame map[string]struct{}) []model.Issue {
	var out []model.Issue
	for _, issue := range issues {
		if _, in := frame[issue.ID]; in {
			out = append(out, issue)
		}
	}
	return out
}

// rankVerbsRefuseADeletedIssue pins that naming a trashed issue in any rank
// verb is refused, and that the refusal costs the order nothing.
//
// Rank is a position among issues that are actually listed, so a deleted issue
// has none. A verb that let one through would write a key onto a row no view
// shows, and RankSet, which rewrites its frame's slots in place, has the worst
// answer of all: its slot list counts only live members while its replacement
// list would count every representative, so a deleted one makes the second
// longer than the first and the rewrite commits the prefix that fits, dropping
// whichever live sibling owns the slots that run out. That issue then belongs
// to no position at all.
//
// The survivor assertion is the point of this case and outlives the particular
// answer: whatever a rank verb decides to do about a deleted issue, an issue
// nobody named must still be listed afterwards. [LAW:no-silent-failure]
func rankVerbsRefuseADeletedIssue(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	gone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "gone", Topic: "core", ParentID: epic.ID})
	sibling := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "sibling", Topic: "core", ParentID: epic.ID})
	// bystander is named by nothing below. It is the issue a truncated rewrite
	// would drop, so its survival is what separates a refused write from a
	// half-applied one.
	bystander := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "bystander", Topic: "core", ParentID: epic.ID})

	if _, err := st.Apply(ctx, gone.ID, storage.Change{Action: model.Delete{}, Actor: "tester"}); err != nil {
		t.Fatalf("delete the child: %v", err)
	}
	bystanderRank := mustGet(t, ctx, st, bystander.ID).Rank

	// Every verb, named the same deleted issue. Listing them as values rather
	// than writing four near-identical blocks keeps a verb added later from
	// quietly skipping this case. [LAW:dataflow-not-control-flow]
	refusals := []struct {
		verb string
		call func() error
	}{
		{"RankToTop", func() error { _, err := st.RankToTop(ctx, gone.ID); return err }},
		{"RankToBottom", func() error { _, err := st.RankToBottom(ctx, gone.ID); return err }},
		{"RankAbove", func() error { _, err := st.RankAbove(ctx, gone.ID, sibling.ID); return err }},
		{"RankBelow", func() error { _, err := st.RankBelow(ctx, gone.ID, sibling.ID); return err }},
		{"RankAbove(anchor)", func() error { _, err := st.RankAbove(ctx, sibling.ID, gone.ID); return err }},
		{"RankSet", func() error { _, err := st.RankSet(ctx, []string{gone.ID, sibling.ID}); return err }},
	}
	// The message is asserted whole, not merely as "some error", and compared
	// against one expectation for both engines — which is the only thing that
	// makes the parity claim testable. Containment would not do it: a substring
	// check passes straight through one engine wrapping this refusal in a prefix
	// its counterpart does not. [LAW:one-source-of-truth]
	wantRefusal := fmt.Sprintf("cannot rank deleted issue %s; restore it first", gone.ID)
	for _, r := range refusals {
		err := r.call()
		if err == nil {
			t.Errorf("%s naming the deleted issue %s succeeded; want a refusal", r.verb, gone.ID)
			continue
		}
		if err.Error() != wantRefusal {
			t.Errorf("%s refused with %q; want exactly %q", r.verb, err.Error(), wantRefusal)
		}
		assertRefused(t, err, r.verb+"(deleted issue)")
	}

	// Nothing that was not named may have moved, and nothing may have fallen out
	// of the order entirely.
	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	found := false
	for _, issue := range listed {
		if issue.ID == bystander.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("%s is no longer listed after refused rank writes; a partial rewrite dropped it from the order", bystander.ID)
	}
	if after := mustGet(t, ctx, st, bystander.ID).Rank; after != bystanderRank {
		t.Errorf("a refused rank write moved the unnamed %s: rank %q -> %q", bystander.ID, bystanderRank, after)
	}
	assertPrecedes(t, listed, sibling.ID, bystander.ID)
}

// aContainerListsBeforeEverythingInsideIt pins what "rank order" means once
// frames nest: tree order. A container lists before everything inside it and
// its subtree lists together, whatever keys the frames around it hold.
//
// Keys are frame-local, so nothing stops a container's key from passing its
// descendants' — a container moved past an outsider crosses them, and one a
// rank set displaces crosses them without being named. The fixture builds
// both, with a grandchild under a nested epic so the rule is seen to hold at a
// second level. The outsiders are epics, the only kind the top level holds.
// Every listing is asserted whole, because a key-ordered listing puts the
// container after its own children in each of them.
func aContainerListsBeforeEverythingInsideIt(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	outer := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "outer", Topic: "core", IssueType: model.TypeEpic})
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})
	nested := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "nested epic", Topic: "core", IssueType: model.TypeEpic, ParentID: epic.ID})
	grandchild := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "grandchild", Topic: "core", ParentID: nested.ID})
	last := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "last", Topic: "core", IssueType: model.TypeEpic})

	// Moved: the epic's new key lies past every key its subtree holds.
	if _, err := st.RankBelow(ctx, epic.ID, last.ID); err != nil {
		t.Fatalf("RankBelow(epic, last) error = %v", err)
	}
	listed := mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertIssueIDs(t, "after the epic is moved past its own children", listed,
		[]string{outer.ID, last.ID, epic.ID, child.ID, nested.ID, grandchild.ID})
	assertIssueIDs(t, "descending is the same order reversed",
		mustList(t, ctx, st, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: "rank", Desc: true}}}),
		[]string{grandchild.ID, nested.ID, child.ID, epic.ID, last.ID, outer.ID})
	// The epic's key orders its descendants even in a listing that leaves the
	// epic out.
	assertIssueIDs(t, "a listing without the container still orders by it",
		mustList(t, ctx, st, storage.ListIssuesFilter{IDs: []string{outer.ID, last.ID, child.ID, nested.ID, grandchild.ID}}),
		[]string{outer.ID, last.ID, child.ID, nested.ID, grandchild.ID})

	if _, err := st.RankAbove(ctx, epic.ID, outer.ID); err != nil {
		t.Fatalf("RankAbove(epic, outer) error = %v", err)
	}
	assertOrder(t, ctx, st, "after the epic returns to the top",
		epic.ID, child.ID, nested.ID, grandchild.ID, outer.ID, last.ID)

	// Displaced: a rank set names only the outsiders, and the epic takes the
	// last of its frame's keys, past everything inside it.
	if _, err := st.RankSet(ctx, []string{outer.ID, last.ID}); err != nil {
		t.Fatalf("RankSet(outer, last) error = %v", err)
	}
	listed = mustList(t, ctx, st, storage.ListIssuesFilter{})
	assertDistinctRanks(t, listed)
	assertIssueIDs(t, "after a rank set displaces the epic past its own children", listed,
		[]string{outer.ID, last.ID, epic.ID, child.ID, nested.ID, grandchild.ID})

	// A view's group of related issues follows the same rule: dependencies
	// spread over three frames list in tree order, not by their own keys.
	for _, dependency := range []model.Issue{grandchild, epic, last} {
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: outer.ID, DstID: dependency.ID, Type: model.RelBlockedBy, CreatedBy: "ada"}); err != nil {
			t.Fatalf("AddRelation(%s blocks-on %s) error = %v", outer.ID, dependency.ID, err)
		}
	}
	detail, err := st.GetIssueDetail(ctx, outer.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(%s) error = %v", outer.ID, err)
	}
	assertIssueIDs(t, "an issue's dependencies across frames", detail.BlockedBy,
		[]string{last.ID, epic.ID, grandchild.ID})
}

// rankIntentsResolveAcrossFrames is why the anchored verbs report a RankMove
// at all. Rank meaning is frame-local — an issue's position is only ever read
// against its frame-mates — so an intent naming two issues from different
// frames is honored against the containing ancestors that ARE comparable, and
// the substitution comes back so the caller can say so.
func rankIntentsResolveAcrossFrames(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})
	standalone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "standalone epic", Topic: "core", IssueType: model.TypeEpic})

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

func rankSetImposesOrder(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// Three epics, so the frame they share is the top level.
	a := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "a", Topic: "core", IssueType: model.TypeEpic})
	b := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "b", Topic: "core", IssueType: model.TypeEpic})
	c := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "c", Topic: "core", IssueType: model.TypeEpic})

	result, err := st.RankSet(ctx, []string{c.ID, a.ID, b.ID})
	if err != nil {
		t.Fatalf("RankSet error = %v", err)
	}
	assertOrder(t, ctx, st, "after RankSet", c.ID, a.ID, b.ID)

	// Every named id is accounted for, in the order named, so a caller can
	// report substitutions without matching up two lists itself.
	resolutions := result.Resolutions
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
	// Three top-level epics stack in the top-level frame, and both engines must
	// say so — the frame is what the CLI names in its summary, so a wrong one
	// there tells the reader the backlog head moved when it did not.
	if result.Frame != storage.TopLevel {
		t.Errorf("RankSet frame = %q, want the top level", result.Frame)
	}
}

// followUpsLinkBothWays pins the follow-up link as a birth fact on the row:
// a create that names the ticket it follows up reads it back, the detail of
// each side hydrates the other (the followed-up ticket on the follow-up, every
// follow-up on the ticket they follow up, in rank order), the create event
// records which ticket it was filed on, and no later field write moves it. The
// hierarchy is not consulted: the follow-up here is a sibling of the ticket it
// follows up, under the same epic, exactly as `lit followup` files it.
func followUpsLinkBothWays(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	origin := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "origin", Topic: "core", ParentID: epic.ID})
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first follow-up", Topic: "core", ParentID: epic.ID, FollowsUp: "  " + origin.ID + "  "})
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second follow-up", Topic: "core", ParentID: epic.ID, FollowsUp: origin.ID})

	if first.FollowsUp != origin.ID {
		t.Errorf("created FollowsUp = %q, want the trimmed %q", first.FollowsUp, origin.ID)
	}
	if got := mustGet(t, ctx, st, first.ID).FollowsUp; got != origin.ID {
		t.Errorf("GetIssue FollowsUp = %q, want %q", got, origin.ID)
	}
	if got := mustGet(t, ctx, st, origin.ID).FollowsUp; got != "" {
		t.Errorf("the followed-up ticket's own FollowsUp = %q, want none", got)
	}

	detail, err := st.GetIssueDetail(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(follow-up) error = %v", err)
	}
	if detail.FollowsUp == nil || detail.FollowsUp.ID != origin.ID {
		t.Errorf("detail.FollowsUp = %v, want %s", detail.FollowsUp, origin.ID)
	}
	assertIssueIDs(t, "follow-ups of a follow-up", detail.FollowUps, nil)
	// The follow-up sits beside the ticket it follows up, not under it.
	if detail.Parent == nil || detail.Parent.ID != epic.ID {
		t.Errorf("detail.Parent = %v, want the epic %s", detail.Parent, epic.ID)
	}
	originDetail, err := st.GetIssueDetail(ctx, origin.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(origin) error = %v", err)
	}
	assertIssueIDs(t, "follow-ups of the origin", originDetail.FollowUps, []string{first.ID, second.ID})
	if originDetail.FollowsUp != nil {
		t.Errorf("origin detail.FollowsUp = %v, want none", originDetail.FollowsUp)
	}
	assertIssueIDs(t, "children of the origin", originDetail.Children, nil)

	// The create event is the one record of the origin in history.
	events, err := st.ListEvents(ctx, first.ID)
	if err != nil {
		t.Fatalf("ListEvents(follow-up) error = %v", err)
	}
	var recorded bool
	for _, event := range events {
		for _, change := range event.Changes {
			if event.Action == "created" && change.Field == "follows_up" && change.From == "" && change.To == origin.ID {
				recorded = true
			}
		}
	}
	if !recorded {
		t.Errorf("the create event does not record follows_up %q: %+v", origin.ID, events)
	}

	// A birth fact: a later field write leaves it where it was.
	title := "retitled follow-up"
	retitled, err := st.Apply(ctx, first.ID, storage.Change{Actor: "ada", Fields: storage.UpdateIssueInput{Title: &title}})
	if err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	if retitled.Issue.FollowsUp != origin.ID {
		t.Errorf("FollowsUp after a field write = %q, want %q", retitled.Issue.FollowsUp, origin.ID)
	}

	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export error = %v", err)
	}
	var exported bool
	for _, issue := range export.Issues {
		if issue.ID == first.ID && issue.FollowsUp == origin.ID {
			exported = true
		}
	}
	if !exported {
		t.Errorf("export does not carry follows_up for %s", first.ID)
	}

	// The link names a ticket that exists, as a parent must; a follows_up no
	// row answers to is refused at the write, not stored and never rendered.
	_, err = st.CreateIssue(ctx, storage.CreateIssueInput{Title: "dangling follow-up", Topic: "core", ParentID: epic.ID, FollowsUp: "core-nope"})
	assertNotFound(t, err, "issue", "CreateIssue(FollowsUp names no issue)")
}

// closeRedirectsToCanonical pins the close payload as a whole: a redirecting
// outcome records both the resolution and the ticket the work moved to, a
// reopen clears all of it at once, and a redirect to an issue that is not
// there is refused rather than stored as a dangling pointer.
func closeRedirectsToCanonical(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	canonical := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "canonical", Topic: "core", ParentID: epic.ID})
	duplicate := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "duplicate", Topic: "core", ParentID: epic.ID})

	closed, err := st.Apply(ctx, duplicate.ID, storage.Change{
		Action: model.Close{Outcome: model.Duplicate{Of: canonical.ID}}, Actor: "ada",
	})
	if err != nil {
		t.Fatalf("Apply close-as-duplicate error = %v", err)
	}
	if closed.Issue.State() != model.StateClosed {
		t.Errorf("State after close = %q, want closed", closed.Issue.State())
	}
	// The outcome travels through the state machine into the closed leaf, so
	// both halves come back off the issue rather than out of a second write.
	if resolution := closed.Issue.ResolutionValue(); resolution == nil || *resolution != model.ResolutionDuplicate {
		t.Errorf("Resolution after close = %v, want duplicate", resolution)
	}
	if target := closed.Issue.RedirectTargetValue(); target == nil || *target != canonical.ID {
		t.Errorf("RedirectTarget after close = %v, want %s", target, canonical.ID)
	}
	if closed.Issue.ClosedAtValue() == nil {
		t.Error("ClosedAt after close is nil; a close must record when")
	}

	reopened, err := st.Apply(ctx, duplicate.ID, storage.Change{Action: model.Reopen{}, Actor: "ada"})
	if err != nil {
		t.Fatalf("Apply reopen error = %v", err)
	}
	// Reopening clears the whole close payload together — a live issue
	// carrying a stale resolution or redirect is a state no reader could make
	// sense of.
	if reopened.Issue.ResolutionValue() != nil || reopened.Issue.RedirectTargetValue() != nil || reopened.Issue.ClosedAtValue() != nil {
		t.Errorf("reopen left a close payload: resolution=%v redirect=%v closedAt=%v",
			reopened.Issue.ResolutionValue(), reopened.Issue.RedirectTargetValue(), reopened.Issue.ClosedAtValue())
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

func commentsRoundtrip(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "discussed", Topic: "core", ParentID: epic.ID})

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
	// A blank body is refused, not stored: the rule lives in each engine, so
	// this is the one place both are held to it. Whitespace-only counts as
	// blank, because both engines trim before they look.
	_, _, err = st.AddComment(ctx, storage.AddCommentInput{IssueID: issue.ID, Body: " \t\n", CreatedBy: "ada"})
	assertRefused(t, err, "AddComment(blank body)")

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
	_, err = st.DeleteComment(ctx, " \t")
	assertRefused(t, err, "DeleteComment(blank id)")

	_, _, err = st.AddComment(ctx, storage.AddCommentInput{IssueID: "no-such-issue", Body: "x", CreatedBy: "ada"})
	assertNotFound(t, err, "issue", "AddComment on a missing issue")
}

func labelsRoundtrip(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "tagged", Topic: "core", ParentID: epic.ID})

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

func relationsRoundtrip(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The dependency lives in another epic: between two leaves of one epic,
	// rank orders the work and a blocked-by edge is refused.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	upstream := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "upstream epic", Topic: "core", IssueType: model.TypeEpic})
	dependent := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "dependent", Topic: "core", ParentID: epic.ID})
	dependency := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "dependency", Topic: "core", ParentID: upstream.ID})
	peer := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "peer", Topic: "core", ParentID: epic.ID})

	rel, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID: dependent.ID, DstID: dependency.ID, Type: model.RelBlockedBy, CreatedBy: "ada",
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
	// Adding an edge the graph already holds is answered, not faulted: the
	// edge asked for is in place, so every engine returns the contract's
	// satisfied answer naming it. related-to is undirected, so naming its ends
	// against the stored order asks for the same edge, and the answer names
	// the stored order. The count below proves neither repeat wrote a second
	// row.
	low, high := model.RelRelatedTo.CanonicalEndpoints(dependent.ID, peer.ID)
	for _, repeat := range []storage.AddRelationInput{
		{SrcID: dependent.ID, DstID: dependency.ID, Type: model.RelBlockedBy, CreatedBy: "ada"},
		{SrcID: high, DstID: low, Type: model.RelRelatedTo, CreatedBy: "ada"},
	} {
		_, err := st.AddRelation(ctx, repeat)
		var exists storage.RelationExistsError
		if !errors.As(err, &exists) {
			t.Fatalf("AddRelation(%+v) again error = %v, want storage.RelationExistsError", repeat, err)
		}
		wantSrc, wantDst := repeat.Type.CanonicalEndpoints(repeat.SrcID, repeat.DstID)
		if exists != (storage.RelationExistsError{SrcID: wantSrc, DstID: wantDst, Type: repeat.Type}) || !exists.Satisfied() {
			t.Errorf("AddRelation(%+v) again = %+v, want the satisfied answer naming %s->%s", repeat, exists, wantSrc, wantDst)
		}
	}

	all, err := st.ListRelationsForIssue(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("ListRelationsForIssue error = %v", err)
	}
	// Both edges just written, plus the parent edge the dependent was born
	// with: naming no type narrows nothing.
	if len(all) != 3 {
		t.Errorf("ListRelationsForIssue returned %d edges, want the two written and the parent edge", len(all))
	}

	// The variadic types argument narrows rather than switching behavior:
	// naming no type means "every type", never "no types".
	blocks, err := st.ListRelationsForIssue(ctx, dependent.ID, model.RelBlockedBy)
	if err != nil {
		t.Fatalf("ListRelationsForIssue(blocks) error = %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != model.RelBlockedBy {
		t.Errorf("filtered edges = %+v, want the one blocked-by edge", blocks)
	}

	// Edges are read from either end — the dependency sees its dependent.
	fromOtherEnd, err := st.ListRelationsForIssue(ctx, dependency.ID, model.RelBlockedBy)
	if err != nil {
		t.Fatalf("ListRelationsForIssue from the dependency error = %v", err)
	}
	if len(fromOtherEnd) != 1 {
		t.Errorf("edges at the dependency = %+v, want the one blocked-by edge", fromOtherEnd)
	}

	if err := st.RemoveRelation(ctx, dependent.ID, dependency.ID, model.RelBlockedBy); err != nil {
		t.Fatalf("RemoveRelation error = %v", err)
	}
	err = st.RemoveRelation(ctx, dependent.ID, dependency.ID, model.RelBlockedBy)
	assertNotFound(t, err, "relation", "RemoveRelation of an absent edge")

	// related-to is symmetric, so an issue cannot be related to itself.
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID: peer.ID, DstID: peer.ID, Type: model.RelRelatedTo, CreatedBy: "ada",
	}); err == nil {
		t.Error("AddRelation related-to self succeeded; want an error")
	}
}

func relationsBatchBucketsEdges(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID})
	// The dependency lives in another epic, so the epic under test holds the
	// one child its bucket is asserted to.
	other := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "other epic", Topic: "core", IssueType: model.TypeEpic})
	dependency := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "dependency", Topic: "core", ParentID: other.ID})
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID: child.ID, DstID: dependency.ID, Type: model.RelBlockedBy, CreatedBy: "ada",
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
	assertIssueIDs(t, "child BlockedBy", childEdges.BlockedBy, []string{dependency.ID})

	assertIssueIDs(t, "epic Children", byID[epic.ID].Children, []string{child.ID})

	// BlockedBy and Blocks are the two readings of one edge set, which is why
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

// A dependency is ranked below the work waiting on it when tree order lists
// it after that work — read through the epics that hold them, never by
// comparing the two issues' own keys across frames. The two epics are filed
// one way and listed the other, so the order each task was filed in is not
// the order its epic lists in. A third epic, ranked above both, holds a
// dependency that lists first.
func rankInversionsReadTreeOrder(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	early := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "early", Topic: "core", IssueType: model.TypeEpic})
	late := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "late", Topic: "core", IssueType: model.TypeEpic})
	lateTask := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "late task", Topic: "core", ParentID: late.ID})
	earlyTask := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "early task", Topic: "core", ParentID: early.ID})
	top := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "top", Topic: "core", IssueType: model.TypeEpic})
	topTask := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "top task", Topic: "core", ParentID: top.ID})
	for _, id := range []string{late.ID, top.ID} {
		if _, err := st.RankToTop(ctx, id); err != nil {
			t.Fatalf("RankToTop(%s) error = %v", id, err)
		}
	}
	mustBlock(t, ctx, st, lateTask.ID, earlyTask.ID)
	mustBlock(t, ctx, st, earlyTask.ID, topTask.ID)
	listed := mustListIDs(t, ctx, st, []string{top.ID, topTask.ID, late.ID, lateTask.ID, early.ID, earlyTask.ID})
	order, err := st.DependencyOrder(ctx)
	if err != nil {
		t.Fatalf("DependencyOrder error = %v", err)
	}
	if !order.Inverted(listed[lateTask.ID], listed[earlyTask.ID]) {
		t.Errorf("Inverted(%s blocked by %s) = false, want true: its dependency's epic lists after its own", lateTask.ID, earlyTask.ID)
	}
	if order.Inverted(listed[earlyTask.ID], listed[topTask.ID]) {
		t.Errorf("Inverted(%s blocked by %s) = true, want false: its dependency's epic lists first", earlyTask.ID, topTask.ID)
	}
}

// Two epics whose live tasks wait on each other's both ways are interleaved:
// no order of the two epics lists every dependency first, so neither edge is a
// rank inversion and the repair has nothing between them to satisfy. Closing
// the task one way round ends it, because only live work is ordered.
func interleavedEpicsHoldNoRankInversion(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first", Topic: "core", IssueType: model.TypeEpic})
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second", Topic: "core", IssueType: model.TypeEpic})
	later := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "later", Topic: "core", ParentID: first.ID})
	base := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "base", Topic: "core", ParentID: first.ID})
	middle := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "middle", Topic: "core", ParentID: second.ID})
	mustBlock(t, ctx, st, later.ID, middle.ID)
	mustBlock(t, ctx, st, middle.ID, base.ID)
	listed := mustListIDs(t, ctx, st, []string{first.ID, later.ID, base.ID, second.ID, middle.ID})
	order, err := st.DependencyOrder(ctx)
	if err != nil {
		t.Fatalf("DependencyOrder error = %v", err)
	}
	epics := []string{first.ID, second.ID}
	slices.Sort(epics)
	if got := order.Interleaved(); !slices.EqualFunc(got, [][]string{epics}, slices.Equal) {
		t.Errorf("Interleaved() = %v, want [%v]", got, epics)
	}
	for _, edge := range [][2]string{{later.ID, middle.ID}, {middle.ID, base.ID}} {
		if order.Inverted(listed[edge[0]], listed[edge[1]]) {
			t.Errorf("Inverted(%s blocked by %s) = true, want false: the epics are interleaved", edge[0], edge[1])
		}
	}
	if got := order.Constraints(); len(got) != 0 {
		t.Errorf("Constraints() = %v, want none: both lifted edges lie inside the group", got)
	}

	if _, err := st.Apply(ctx, base.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply start error = %v", err)
	}
	if _, err := st.Apply(ctx, base.ID, storage.Change{Action: model.Done{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply done error = %v", err)
	}
	order, err = st.DependencyOrder(ctx)
	if err != nil {
		t.Fatalf("DependencyOrder after done error = %v", err)
	}
	if got := order.Interleaved(); len(got) != 0 {
		t.Errorf("Interleaved() after %s is done = %v, want none: only live work is ordered", base.ID, got)
	}
	if !order.Inverted(listed[later.ID], listed[middle.ID]) {
		t.Errorf("Inverted(%s blocked by %s) after %s is done = false, want true: %s lists after %s", later.ID, middle.ID, base.ID, second.ID, first.ID)
	}
}

// mustBlock records dependent blocked by dependency.
func mustBlock(t *testing.T, ctx context.Context, st storage.Store, dependent, dependency string) {
	t.Helper()
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: dependent, DstID: dependency, Type: model.RelBlockedBy, CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddRelation(%s blocked by %s) error = %v", dependent, dependency, err)
	}
}

// mustListIDs lists every issue, asserts the listing is want, and returns the
// listed issues by id.
func mustListIDs(t *testing.T, ctx context.Context, st storage.Store, want []string) map[string]model.Issue {
	t.Helper()
	listed, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues error = %v", err)
	}
	assertIssueIDs(t, "listing", listed, want)
	byID := make(map[string]model.Issue, len(listed))
	for _, issue := range listed {
		byID[issue.ID] = issue
	}
	return byID
}

func parentWiring(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	other := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "other epic", Topic: "core", IssueType: model.TypeEpic})
	// The child is itself an epic: a leaf cannot be detached, and the clearing
	// half of this case is about the edge, not the child's kind.
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", IssueType: model.TypeEpic})

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

// anImportFileThatBreaksARuleIsRefusedWhole pins that every rule an import
// file can break is a ValidationError, in both formats and whatever the rule:
// the file is what is wrong, so the same file is refused the same way on every
// run, and a caller tells that from a fault by the type alone. A refused file
// writes nothing.
func anImportFileThatBreaksARuleIsRefusedWhole(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	existing := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "existing", Topic: "core", IssueType: model.TypeEpic})
	before := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true, IncludeArchived: true})
	title, topic, epicType, taskType, shortTopic, blank, renamed := "imported", "core", "epic", "task", "x", " ", "renamed"
	commaLabel := []string{"a,b"}
	commaAssignee := "Doe, J"
	lands := storage.ImportTreeSpec{LocalID: "e", Title: "lands", IssueType: "epic", Topic: "core"}
	tree := func(specs ...storage.ImportTreeSpec) func() error {
		return func() error { _, err := st.ImportTree(ctx, prefix, specs); return err }
	}
	bulk := func(specs ...storage.BulkIssueSpec) func() error {
		return func() error { _, err := st.BulkApply(ctx, prefix, "ada", specs); return err }
	}
	epic := func(localID, blockedBy string) storage.BulkIssueSpec {
		spec := storage.BulkIssueSpec{LocalID: localID, Title: &title, Topic: &topic, IssueType: &epicType}
		if blockedBy != "" {
			spec.BlockedBy = []string{blockedBy}
		}
		return spec
	}
	for _, imp := range []struct {
		what string
		do   func() error
	}{
		{"ImportTree(no specs)", tree()},
		{"ImportTree(a spec missing its title)", tree(storage.ImportTreeSpec{LocalID: "a", IssueType: "epic", Topic: "core"})},
		{"ImportTree(a spec missing its topic, after one that would land)", tree(lands,
			storage.ImportTreeSpec{LocalID: "a", Title: "no topic", IssueType: "epic"},
		)},
		{"ImportTree(a topic too short to be one, after one that would land)", tree(lands,
			storage.ImportTreeSpec{LocalID: "a", Title: "short topic", IssueType: "epic", Topic: shortTopic},
		)},
		{"ImportTree(a label the create refuses, after one that would land)", tree(lands,
			storage.ImportTreeSpec{LocalID: "a", Title: "comma label", IssueType: "epic", Topic: "core", Labels: commaLabel},
		)},
		{"ImportTree(an assignee the create refuses, after one that would land)", tree(lands,
			storage.ImportTreeSpec{LocalID: "a", Title: "comma assignee", IssueType: "epic", Topic: "core", Assignee: commaAssignee},
		)},
		{"ImportTree(a duplicate local_id)", tree(
			storage.ImportTreeSpec{LocalID: "a", Title: "one", IssueType: "epic", Topic: "core"},
			storage.ImportTreeSpec{LocalID: "a", Title: "two", IssueType: "epic", Topic: "core"},
		)},
		{"ImportTree(a parent no spec names)", tree(storage.ImportTreeSpec{LocalID: "a", Title: "t", IssueType: "task", Topic: "core", Parent: "nowhere"})},
		{"ImportTree(a blocked_by cycle)", tree(
			storage.ImportTreeSpec{LocalID: "a", Title: "a", IssueType: "epic", Topic: "core", BlockedBy: []string{"b"}},
			storage.ImportTreeSpec{LocalID: "b", Title: "b", IssueType: "epic", Topic: "core", BlockedBy: []string{"a"}},
		)},
		{"ImportTree(a blocked_by naming one record twice)", tree(lands,
			storage.ImportTreeSpec{LocalID: "b", Title: "b", IssueType: "epic", Topic: "core", BlockedBy: []string{"e", "e"}},
		)},
		{"ImportTree(a task blocked by the epic it sits under)", tree(lands,
			storage.ImportTreeSpec{LocalID: "t", Title: "t", IssueType: "task", Topic: "core", Parent: "e", BlockedBy: []string{"e"}},
		)},
		{"ImportTree(a task blocked by an epic two levels above it)", tree(lands,
			storage.ImportTreeSpec{LocalID: "s", Title: "s", IssueType: "epic", Topic: "core", Parent: "e"},
			storage.ImportTreeSpec{LocalID: "t", Title: "t", IssueType: "task", Topic: "core", Parent: "s", BlockedBy: []string{"e"}},
		)},
		{"ImportTree(a task blocked by a task beside it under one epic)", tree(lands,
			storage.ImportTreeSpec{LocalID: "a", Title: "a", IssueType: "task", Topic: "core", Parent: "e"},
			storage.ImportTreeSpec{LocalID: "b", Title: "b", IssueType: "task", Topic: "core", Parent: "e", BlockedBy: []string{"a"}},
		)},
		{"BulkApply(no documents)", bulk()},
		{"BulkApply(a create missing its topic)", bulk(storage.BulkIssueSpec{Title: &title, IssueType: &epicType})},
		{"BulkApply(a topic too short to be one, after a create that would land)", bulk(epic("e", ""),
			storage.BulkIssueSpec{Title: &title, Topic: &shortTopic, IssueType: &epicType})},
		{"BulkApply(a label the create refuses, after a create that would land)", bulk(epic("e", ""),
			storage.BulkIssueSpec{Title: &title, Topic: &topic, IssueType: &epicType, Labels: &commaLabel})},
		{"BulkApply(an update that empties a title, after a create that would land)", bulk(epic("e", ""),
			storage.BulkIssueSpec{ID: existing.ID, Title: &blank})},
		{"BulkApply(an update with a label the update refuses, after a create that would land)", bulk(epic("e", ""),
			storage.BulkIssueSpec{ID: existing.ID, Labels: &commaLabel})},
		{"BulkApply(an assignee the create refuses, after a create that would land)", bulk(epic("e", ""),
			storage.BulkIssueSpec{Title: &title, Topic: &topic, IssueType: &epicType, Assignee: &commaAssignee})},
		{"BulkApply(an update with an assignee the update refuses, after a create that would land)", bulk(epic("e", ""),
			storage.BulkIssueSpec{ID: existing.ID, Assignee: &commaAssignee})},
		{"BulkApply(a duplicate local_id)", bulk(epic("a", ""), epic("a", ""))},
		{"BulkApply(a blocked_by cycle)", bulk(epic("a", "b"), epic("b", "a"))},
		{"BulkApply(a blocked_by naming one existing issue twice)", bulk(
			storage.BulkIssueSpec{Title: &title, Topic: &topic, IssueType: &epicType, BlockedBy: []string{existing.ID, existing.ID}})},
		{"BulkApply(a task with no local_id blocked by the epic it sits under)", bulk(epic("e", ""),
			storage.BulkIssueSpec{Title: &title, Topic: &topic, IssueType: &taskType, Parent: "e", BlockedBy: []string{"e"}})},
		{"BulkApply(a task blocked by a task beside it under one epic)", bulk(epic("e", ""),
			storage.BulkIssueSpec{LocalID: "a", Title: &title, Topic: &topic, IssueType: &taskType, Parent: "e"},
			storage.BulkIssueSpec{Title: &title, Topic: &topic, IssueType: &taskType, Parent: "e", BlockedBy: []string{"a"}})},
		{"BulkApply(an update, then a task blocked by a task beside it under an existing epic)", bulk(
			storage.BulkIssueSpec{ID: existing.ID, Title: &renamed},
			storage.BulkIssueSpec{LocalID: "a", Title: &title, Topic: &topic, IssueType: &taskType, Parent: existing.ID},
			storage.BulkIssueSpec{Title: &title, Topic: &topic, IssueType: &taskType, Parent: existing.ID, BlockedBy: []string{"a"}})},
	} {
		assertRefused(t, imp.do(), imp.what)
	}
	after := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true, IncludeArchived: true})
	if len(after) != len(before) {
		t.Errorf("issues after the refused imports = %d, want %d: a file the gate refuses writes nothing", len(after), len(before))
	}
	// A refusal at a write cannot take back an update, so only the gate keeps
	// one from landing.
	if got := mustGet(t, ctx, st, existing.ID); got.Title != "existing" {
		t.Errorf("existing title after the refused imports = %q, want %q: a file the gate refuses writes nothing", got.Title, "existing")
	}
}

// everyTicketLivesInsideAContainer pins containment, the rule the hierarchy
// obeys: a leaf always has a parent, and every parent is an epic. Each write
// that could break it is tried, through every door onto that write, and the
// assertion after each refusal is on the stored shape — a write that errored
// and still landed would pass an error-only check while leaving the state
// this case forbids.
func everyTicketLivesInsideAContainer(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	leaf := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "leaf", Topic: "core", ParentID: epic.ID})
	nested := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "nested epic", Topic: "core", IssueType: model.TypeEpic, ParentID: epic.ID})

	// A leaf is never created outside an epic, whatever its type. The refusal
	// is a validation refusal, not a not-found: nothing was looked up.
	for _, leafType := range []model.IssueType{model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeChore} {
		_, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "loose", Topic: "core", IssueType: leafType, Prefix: prefix})
		var refused model.ValidationError
		if !errors.As(err, &refused) {
			t.Errorf("CreateIssue(%s with no parent) error = %v; want a ValidationError", leafType, err)
		}
	}
	// The default type is a leaf, so an input that states no type is refused
	// the same way.
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "loose", Topic: "core", Prefix: prefix}); err == nil {
		t.Error("CreateIssue(default type, no parent) succeeded; want a refusal")
	}
	// An epic may be top-level, and may sit inside another epic.
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "top-level epic", Topic: "core", IssueType: model.TypeEpic, Prefix: prefix}); err != nil {
		t.Errorf("CreateIssue(epic with no parent) error = %v; an epic is the one thing that may be top-level", err)
	}

	// Every parent is an epic: a create, a reparent, and the dep-add door onto
	// the same edge all refuse a leaf as the parent.
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "under a leaf", Topic: "core", ParentID: leaf.ID, Prefix: prefix}); err == nil {
		t.Error("CreateIssue(under a leaf) succeeded; want a refusal")
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "epic under a leaf", Topic: "core", IssueType: model.TypeEpic, ParentID: leaf.ID, Prefix: prefix}); err == nil {
		t.Error("CreateIssue(epic under a leaf) succeeded; want a refusal")
	}
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: nested.ID, ParentID: leaf.ID}); err == nil {
		t.Error("SetParent(epic under a leaf) succeeded; want a refusal")
	}
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: nested.ID, DstID: leaf.ID, Type: model.RelParentChild}); err == nil {
		t.Error("AddRelation(parent-child onto a leaf) succeeded; want a refusal")
	}
	assertIssueIDs(t, "children of the leaf after the refusals", mustChildren(t, ctx, st, leaf.ID), nil)
	assertIssueIDs(t, "children of the epic after the refusals", mustChildren(t, ctx, st, epic.ID), []string{leaf.ID, nested.ID})

	// The two file imports are gated before their first write: a file with a
	// loose leaf, or a leaf named as a parent, is refused whole, with nothing
	// of it landed — not even the tombstones a compensated import leaves.
	before := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true, IncludeArchived: true})
	epicTitle, leafTitle, underTitle, topic := "bulk epic", "bulk leaf", "bulk under a leaf", "core"
	epicType, leafType, bugType := "epic", "task", "bug"
	for _, imp := range []struct {
		what string
		do   func() error
	}{
		{"ImportTree(a loose leaf after an epic)", func() error {
			_, err := st.ImportTree(ctx, prefix, []storage.ImportTreeSpec{
				{LocalID: "e", Title: "imported epic", IssueType: "epic", Topic: "core"},
				{LocalID: "l", Title: "loose leaf", IssueType: "task", Topic: "core"},
			})
			return err
		}},
		{"ImportTree(a leaf under a leaf)", func() error {
			_, err := st.ImportTree(ctx, prefix, []storage.ImportTreeSpec{
				{LocalID: "e", Title: "imported epic", IssueType: "epic", Topic: "core"},
				{LocalID: "l", Title: "imported leaf", IssueType: "task", Topic: "core", Parent: "e"},
				{LocalID: "u", Title: "under a leaf", IssueType: "bug", Topic: "core", Parent: "l"},
			})
			return err
		}},
		{"BulkApply(a loose leaf after an epic)", func() error {
			_, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
				{LocalID: "e", Title: &epicTitle, Topic: &topic, IssueType: &epicType},
				{Title: &leafTitle, Topic: &topic, IssueType: &leafType},
			})
			return err
		}},
		{"BulkApply(a leaf under a leaf in the file)", func() error {
			_, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
				{LocalID: "e", Title: &epicTitle, Topic: &topic, IssueType: &epicType},
				{LocalID: "l", Title: &leafTitle, Topic: &topic, IssueType: &leafType, Parent: "e"},
				{Title: &underTitle, Topic: &topic, IssueType: &bugType, Parent: "l"},
			})
			return err
		}},
	} {
		err := imp.do()
		var refused model.ValidationError
		if !errors.As(err, &refused) {
			t.Errorf("%s error = %v; want a ValidationError", imp.what, err)
		}
	}
	after := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true, IncludeArchived: true})
	if len(after) != len(before) {
		t.Errorf("issues after the refused imports = %d, want %d: a file the gate refuses writes nothing", len(after), len(before))
	}

	// A leaf keeps its parent: neither detach door removes the edge, and the
	// refusal names the move that does what the caller meant.
	for _, detach := range []struct {
		door string
		do   func() error
	}{
		{"ClearParent", func() error { return st.ClearParent(ctx, leaf.ID) }},
		{"RemoveRelation(parent-child)", func() error { return st.RemoveRelation(ctx, leaf.ID, epic.ID, model.RelParentChild) }},
	} {
		err := detach.do()
		var refused model.ValidationError
		if !errors.As(err, &refused) {
			t.Errorf("%s on a leaf error = %v; want a ValidationError", detach.door, err)
		} else if !strings.Contains(err.Error(), "lit parent set") {
			t.Errorf("%s on a leaf error = %q; want it to name lit parent set as the move", detach.door, err)
		}
	}
	assertIssueIDs(t, "children of the epic after the detach refusals", mustChildren(t, ctx, st, epic.ID), []string{leaf.ID, nested.ID})

	// A nested epic is the one thing a detach lifts to the top level, through
	// either door; the second finds nothing left to remove.
	if err := st.RemoveRelation(ctx, nested.ID, epic.ID, model.RelParentChild); err != nil {
		t.Fatalf("RemoveRelation(nested epic's parent edge) error = %v; an epic may be top-level", err)
	}
	assertIssueIDs(t, "children of the epic after lifting the nested epic", mustChildren(t, ctx, st, epic.ID), []string{leaf.ID})
	assertNotFound(t, st.ClearParent(ctx, nested.ID), "parent relation", "ClearParent of the lifted epic")

	// A missing edge is still reported as missing before the rule is judged,
	// so a typo names the edge rather than the leaf.
	assertNotFound(t, st.RemoveRelation(ctx, leaf.ID, nested.ID, model.RelParentChild), "relation", "RemoveRelation of a parent edge the leaf does not hold")
}

// theHierarchyStaysATree pins the rule both engines owe the parent graph: an
// issue reaches a root by walking up, so no write may put a would-be parent at
// or below its own child. Both doors onto the edge are exercised, because a
// rule that holds for 'lit parent set' and not for 'lit dep add' is not a rule
// about the hierarchy — it is a rule about which command was typed.
//
// The assertion is on the STORED edges after each refusal, not on the error
// alone: a write that errored and still landed would satisfy an error-only
// check while leaving exactly the state this case exists to forbid.
func theHierarchyStaysATree(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	root := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "root epic", Topic: "core", IssueType: model.TypeEpic})
	mid := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "mid epic", Topic: "core", IssueType: model.TypeEpic, ParentID: root.ID})
	leaf := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "leaf", Topic: "core", ParentID: mid.ID})

	// Precondition that gives the case teeth: the chain really is root > mid >
	// leaf, so each refusal below is refusing a loop rather than a nonsense id.
	assertIssueIDs(t, "children of the root", mustChildren(t, ctx, st, root.ID), []string{mid.ID})
	assertIssueIDs(t, "children of the mid epic", mustChildren(t, ctx, st, mid.ID), []string{leaf.ID})

	refusals := []struct {
		what          string
		child, parent string
	}{
		{"a parent under its own child", root.ID, mid.ID},
		{"a root under its own grandchild", root.ID, leaf.ID},
		{"a sub-epic under its own child", mid.ID, leaf.ID},
	}
	for _, r := range refusals {
		if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: r.child, ParentID: r.parent}); err == nil {
			t.Errorf("SetParent(%s) succeeded; it closes a parent cycle", r.what)
		}
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: r.child, DstID: r.parent, Type: model.RelParentChild}); err == nil {
			t.Errorf("AddRelation(%s) succeeded; it closes a parent cycle", r.what)
		}
	}

	// Every refusal left the tree exactly as it found it.
	assertIssueIDs(t, "children of the root after the refusals", mustChildren(t, ctx, st, root.ID), []string{mid.ID})
	assertIssueIDs(t, "children of the mid epic after the refusals", mustChildren(t, ctx, st, mid.ID), []string{leaf.ID})

	// A move that closes no loop still works, or the rule has simply broken
	// reparenting.
	sibling := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "sibling epic", Topic: "core", IssueType: model.TypeEpic})
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: leaf.ID, ParentID: sibling.ID}); err != nil {
		t.Fatalf("SetParent(leaf under an unrelated epic) error = %v; this move closes no cycle", err)
	}
	assertIssueIDs(t, "children of the sibling epic", mustChildren(t, ctx, st, sibling.ID), []string{leaf.ID})
	assertIssueIDs(t, "children of the mid epic after the move", mustChildren(t, ctx, st, mid.ID), nil)
}

// blocksNeverRunsAlongTheHierarchy pins that a blocked-by edge between an issue and
// any of its ancestors is refused at every depth and in both directions, and on
// each write path that can create one: AddRelation and both import formats.
// Depth is the point — a check that compared nearest epics refused the
// direct-child case and let everything two levels down through.
func blocksNeverRunsAlongTheHierarchy(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	root := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "root epic", Topic: "core", IssueType: model.TypeEpic})
	mid := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "mid epic", Topic: "core", IssueType: model.TypeEpic, ParentID: root.ID})
	leaf := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "leaf", Topic: "core", ParentID: mid.ID})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "direct child", Topic: "core", ParentID: root.ID})

	refusedPairs := []struct {
		what                  string
		dependent, dependency string
	}{
		{"a leaf blocking its grandparent epic", root.ID, leaf.ID},
		{"a grandparent epic blocking its leaf", leaf.ID, root.ID},
		{"a sub-epic blocking its parent epic", root.ID, mid.ID},
		{"an epic blocking its sub-epic", mid.ID, root.ID},
		{"a leaf blocking its parent epic", mid.ID, leaf.ID},
		{"a direct child blocking its epic", root.ID, child.ID},
	}
	for _, r := range refusedPairs {
		_, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: r.dependent, DstID: r.dependency, Type: model.RelBlockedBy})
		assertHierarchyBlocksRefused(t, err, "AddRelation("+r.what+")")
	}
	for _, id := range []string{root.ID, mid.ID, leaf.ID, child.ID} {
		edges, err := st.ListRelationsForIssue(ctx, id, model.RelBlockedBy)
		if err != nil {
			t.Fatalf("ListRelationsForIssue(%s) error = %v", id, err)
		}
		if len(edges) != 0 {
			t.Errorf("blocked-by edges at %s after the refusals = %+v, want none", id, edges)
		}
	}

	// Branches of one tree are not ancestry: the direct child and the leaf
	// share an ancestor but neither sits above the other.
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: leaf.ID, DstID: child.ID, Type: model.RelBlockedBy}); err != nil {
		t.Fatalf("AddRelation(leaf depends on a cousin) error = %v; neither is above the other", err)
	}

	// Ancestry is the line, not the tree: a second sub-epic beside mid and an
	// issue under it sit in root's tree with mid and leaf, yet neither branch is
	// above the other, so edges between the branches are accepted at every
	// depth.
	holder := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "sibling sub-epic", Topic: "core", IssueType: model.TypeEpic, ParentID: root.ID})
	held := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "held leaf", Topic: "core", ParentID: holder.ID})
	accepted := []struct {
		what                  string
		dependent, dependency string
	}{
		{"a sub-epic blocking its sibling sub-epic", holder.ID, mid.ID},
		{"a sub-epic blocking a leaf under its sibling", mid.ID, held.ID},
		{"a leaf blocking its parent's sibling sub-epic", held.ID, mid.ID},
	}
	for _, a := range accepted {
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: a.dependent, DstID: a.dependency, Type: model.RelBlockedBy}); err != nil {
			t.Errorf("AddRelation(%s) error = %v; neither is above the other", a.what, err)
		}
		if err := st.RemoveRelation(ctx, a.dependent, a.dependency, model.RelBlockedBy); err != nil {
			t.Fatalf("RemoveRelation(%s) error = %v", a.what, err)
		}
	}

	_, err := st.ImportTree(ctx, prefix, []storage.ImportTreeSpec{
		{LocalID: "e", Title: "imported epic", IssueType: "epic", Topic: "core"},
		{LocalID: "s", Title: "imported sub-epic", IssueType: "epic", Topic: "core", Parent: "e"},
		{LocalID: "l", Title: "imported leaf", IssueType: "task", Topic: "core", Parent: "s", BlockedBy: []string{"e"}},
	})
	assertHierarchyBlocksRefused(t, err, "ImportTree(a leaf depending on its grandparent epic)")

	title, topic, issueType := "bulk leaf", "core", "task"
	_, err = st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{Title: &title, Topic: &topic, IssueType: &issueType, Parent: mid.ID, BlockedBy: []string{root.ID}},
	})
	assertHierarchyBlocksRefused(t, err, "BulkApply(a leaf depending on its grandparent epic)")
}

// blocksNeverOrdersTwoLeavesOfOneEpic pins the other half of the same-epic
// rule: a blocked-by edge between two leaves directly under one epic is
// refused on each write path that can create one — AddRelation, and both
// import formats whether the file names the epic by local_id or by real id.
// Edges that order work units stay legal: a leaf against the sub-epic beside
// it, either way round, and leaves of two different epics.
func blocksNeverOrdersTwoLeavesOfOneEpic(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "first", Topic: "core", ParentID: epic.ID})
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "second", Topic: "core", IssueType: model.TypeBug, ParentID: epic.ID})
	sub := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "sub-epic", Topic: "core", IssueType: model.TypeEpic, ParentID: epic.ID})
	other := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "other epic", Topic: "core", IssueType: model.TypeEpic})
	elsewhere := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "elsewhere", Topic: "core", ParentID: other.ID})

	_, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: second.ID, DstID: first.ID, Type: model.RelBlockedBy})
	assertHierarchyBlocksRefused(t, err, "AddRelation(a leaf blocked by the leaf beside it)")
	// A leaf is not its own sibling: its self-edge is refused as a self-edge.
	_, err = st.AddRelation(ctx, storage.AddRelationInput{SrcID: first.ID, DstID: first.ID, Type: model.RelBlockedBy})
	assertRefused(t, err, "AddRelation(a leaf blocked by itself)")
	if want := first.ID + " cannot be blocked by itself"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("AddRelation(a leaf blocked by itself) error = %v, want it to carry %q", err, want)
	}
	edges, err := st.ListRelationsForIssue(ctx, second.ID, model.RelBlockedBy)
	if err != nil {
		t.Fatalf("ListRelationsForIssue(%s) error = %v", second.ID, err)
	}
	if len(edges) != 0 {
		t.Errorf("blocked-by edges at %s after the refusal = %+v, want none", second.ID, edges)
	}

	_, err = st.ImportTree(ctx, prefix, []storage.ImportTreeSpec{
		{LocalID: "e", Title: "imported epic", IssueType: "epic", Topic: "core"},
		{LocalID: "a", Title: "imported first", IssueType: "task", Topic: "core", Parent: "e"},
		{LocalID: "b", Title: "imported second", IssueType: "task", Topic: "core", Parent: "e", BlockedBy: []string{"a"}},
	})
	assertHierarchyBlocksRefused(t, err, "ImportTree(a leaf blocked by the leaf beside it)")

	title, topic, epicType, taskType := "bulk", "core", "epic", "task"
	_, err = st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{LocalID: "e", Title: &title, Topic: &topic, IssueType: &epicType},
		{LocalID: "a", Title: &title, Topic: &topic, IssueType: &taskType, Parent: "e"},
		{Title: &title, Topic: &topic, IssueType: &taskType, Parent: "e", BlockedBy: []string{"a"}},
	})
	assertHierarchyBlocksRefused(t, err, "BulkApply(a leaf blocked by the leaf beside it, epic in the file)")
	_, err = st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{Title: &title, Topic: &topic, IssueType: &taskType, Parent: epic.ID, BlockedBy: []string{first.ID}},
	})
	assertHierarchyBlocksRefused(t, err, "BulkApply(a leaf blocked by an existing leaf beside it)")

	accepted := []struct {
		what                  string
		dependent, dependency string
	}{
		{"a leaf blocked by the sub-epic beside it", first.ID, sub.ID},
		{"a sub-epic blocked by the leaf beside it", sub.ID, first.ID},
		{"a leaf blocked by a leaf of another epic", first.ID, elsewhere.ID},
	}
	for _, a := range accepted {
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: a.dependent, DstID: a.dependency, Type: model.RelBlockedBy}); err != nil {
			t.Errorf("AddRelation(%s) error = %v; it orders work units, not two leaves of one epic", a.what, err)
		}
		if err := st.RemoveRelation(ctx, a.dependent, a.dependency, model.RelBlockedBy); err != nil {
			t.Fatalf("RemoveRelation(%s) error = %v", a.what, err)
		}
	}

	// A reparent can leave an edge between two leaves of one epic behind. The
	// reverse edge then also closes a cycle, and is refused as an edge inside
	// the epic, the refusal that names the fix.
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: second.ID, ParentID: other.ID}); err != nil {
		t.Fatalf("SetParent(%s into the other epic) error = %v", second.ID, err)
	}
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: second.ID, DstID: first.ID, Type: model.RelBlockedBy}); err != nil {
		t.Fatalf("AddRelation(%s blocked by %s, across epics) error = %v", second.ID, first.ID, err)
	}
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: second.ID, ParentID: epic.ID}); err != nil {
		t.Fatalf("SetParent(%s back into its epic) error = %v", second.ID, err)
	}
	_, err = st.AddRelation(ctx, storage.AddRelationInput{SrcID: first.ID, DstID: second.ID, Type: model.RelBlockedBy})
	assertHierarchyBlocksRefused(t, err, "AddRelation(a leaf blocked by the leaf beside it, closing a cycle)")
}

func assertHierarchyBlocksRefused(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s succeeded; want the same-epic refusal", what)
		return
	}
	assertRefused(t, err, what)
	if !strings.Contains(err.Error(), storage.SameEpicBlocksRejectionMessage) {
		t.Errorf("%s error = %q, want it to carry %q", what, err, storage.SameEpicBlocksRejectionMessage)
	}
}

func topicsDeriveFromIssues(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "renderer", IssueType: model.TypeEpic})
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "one", Topic: "renderer", ParentID: epic.ID})
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "two", Topic: "renderer", ParentID: epic.ID})
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "three", Topic: "parser", ParentID: epic.ID})

	topics, err := st.ListTopics(ctx)
	if err != nil {
		t.Fatalf("ListTopics error = %v", err)
	}
	// Topics are derived vocabulary — distinct, ascending, never a stored list
	// that could disagree with the issues.
	assertStrings(t, "topics", topics, []string{"parser", "renderer"})
}

// issueIdentitiesCoverEveryRow pins that the identity read ignores retention —
// an archived or deleted issue's id still occupies its prefix — and answers in
// id order with the topic each issue was created under.
func issueIdentitiesCoverEveryRow(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "parser", ParentID: epic.ID})
	archived := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "archived", Topic: "renderer", ParentID: epic.ID})
	deleted := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "deleted", Topic: "renderer", ParentID: epic.ID})
	if _, err := st.Apply(ctx, archived.ID, storage.Change{Action: model.Archive{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply archive error = %v", err)
	}
	if _, err := st.Apply(ctx, deleted.ID, storage.Change{Action: model.Delete{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply delete error = %v", err)
	}

	got, err := st.ListIssueIdentities(ctx)
	if err != nil {
		t.Fatalf("ListIssueIdentities error = %v", err)
	}
	want := []storage.IssueIdentity{
		{ID: epic.ID, Topic: "core"},
		{ID: child.ID, Topic: "parser"},
		{ID: archived.ID, Topic: "renderer"},
		{ID: deleted.ID, Topic: "renderer"},
	}
	slices.SortFunc(want, func(a, b storage.IssueIdentity) int { return strings.Compare(a.ID, b.ID) })
	if !slices.Equal(got, want) {
		t.Errorf("ListIssueIdentities = %v, want %v", got, want)
	}
}

func exportCarriesWholeStore(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// One instant for the whole case, so every event this store holds shares a
	// stamp and the export's order is decided entirely by the id tie-break.
	clk.Pin(earlierInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "child", Topic: "core", ParentID: epic.ID, Labels: []string{"perf"}})
	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: child.ID, Body: "note", CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddComment error = %v", err)
	}
	archived := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "archived", Topic: "core", ParentID: epic.ID})
	if _, err := st.Apply(ctx, archived.ID, storage.Change{Action: model.Archive{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply archive error = %v", err)
	}

	// Same-tick ties are what make the export's ordering observable at all: with
	// every timestamp distinct, recording order and (created_at, id) coincide,
	// and this case would pass an engine that exported its raw recording order,
	// which is the defect it exists to catch.
	//
	// The pinned clock is what supplies the tie. Every event below shares one
	// instant, so a broken export fails every run.
	tied := make([]string, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "tied " + name, Topic: "core", ParentID: epic.ID})
		title := "started " + name
		if _, err := st.Apply(ctx, issue.ID, storage.Change{
			Action: model.Start{Assignee: "ada"},
			Fields: storage.UpdateIssueInput{Title: &title},
			Actor:  "ada",
		}); err != nil {
			t.Fatalf("Apply combined action-and-fields change to %q error = %v", name, err)
		}
		tied = append(tied, issue.ID)
	}

	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export error = %v", err)
	}
	// Export is the differential oracle's surface, so it is the WHOLE store —
	// out-of-flow issues included. An export that honored the listing default
	// would silently drop archived work from every backup and every diff.
	leaves := append([]string{child.ID, archived.ID}, tied...)
	assertIssueIDs(t, "exported issues", export.Issues, append([]string{epic.ID}, leaves...))
	// Every leaf holds a parent edge to the epic, and the export carries each
	// one — the archived leaf's included.
	var parented []string
	for _, rel := range export.Relations {
		if rel.Type != model.RelParentChild || rel.DstID != epic.ID {
			t.Errorf("exported relation %+v, want a parent edge onto the epic", rel)
			continue
		}
		parented = append(parented, rel.SrcID)
	}
	slices.Sort(parented)
	wantParented := slices.Clone(leaves)
	slices.Sort(wantParented)
	assertStrings(t, "exported parent edges", parented, wantParented)
	if len(export.Comments) != 1 || export.Comments[0].IssueID != child.ID {
		t.Errorf("exported comments = %+v, want the one comment", export.Comments)
	}
	if len(export.Labels) != 1 || export.Labels[0].Name != "perf" {
		t.Errorf("exported labels = %+v, want the one label", export.Labels)
	}
	if len(export.Events) == 0 {
		t.Error("exported events are empty; the history must travel with the state")
	}
	// Export and ListAllEvents are two reads of one ordered history, so they
	// must agree. events_are_totally_ordered pins ListAllEvents to the
	// contract's (created_at, id); this pins Export to ListAllEvents. Together
	// they say the export is ordered without restating the ordering rule in a
	// second place that could drift from it. An engine serving raw recording
	// order here would diverge from ITSELF on any same-tick tie — before the
	// oracle ever compared two engines. [LAW:one-source-of-truth]
	listed, err := st.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents error = %v", err)
	}
	if !slices.EqualFunc(export.Events, listed, func(a, b model.IssueEvent) bool { return a.ID == b.ID }) {
		t.Errorf("Export's event order differs from ListAllEvents':\n  export = %v\n  list   = %v",
			eventIDsOf(export.Events), eventIDsOf(listed))
	}
	if export.ExportedAt.IsZero() {
		t.Error("ExportedAt is zero; an export must say when it was taken")
	}
}

// eventIDsOf renders an event sequence as its ids, so an ordering failure names
// the sequence instead of dumping whole structs.
func eventIDsOf(events []model.IssueEvent) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return ids
}

func bulkApplyCreatesAndUpdates(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	title := "parent doc"
	childTitle := "child doc"
	topic := "core"
	issueType := "task"
	epicType := "epic"

	// leaf is deliberately listed before the root it parents to: creation must
	// reorder to referent-first, so the asserted sequence below fails for an
	// engine that reports file order — not just one that reports map order.
	result, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{LocalID: "leaf", Title: &childTitle, Topic: &topic, IssueType: &issueType, Parent: "root"},
		{LocalID: "root", Title: &title, Topic: &topic, IssueType: &epicType},
	})
	if err != nil {
		t.Fatalf("BulkApply error = %v", err)
	}
	// A create document is nameable by the LocalID it chose, which is what lets
	// the batch wire its own internal references before any id exists — and the
	// report lists creates in creation order (parent before child), so the same
	// file reports the same sequence on every run.
	assertStrings(t, "bulk created refs in creation order", refsOf(result.Created), []string{"root", "leaf"})
	rootID, ok := createdID(result.Created, "root")
	if !ok {
		t.Fatalf("BulkApply Created = %+v, want a root entry", result.Created)
	}
	leafID, ok := createdID(result.Created, "leaf")
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
func bulkApplyCompensatesFailure(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	home := "the epic created first"
	kept := "created before the failure"
	orphan := "parented to nothing"
	topic := "core"
	issueType := "task"
	epicType := "epic"

	_, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{LocalID: "home", Title: &home, Topic: &topic, IssueType: &epicType},
		{LocalID: "first", Title: &kept, Topic: &topic, IssueType: &issueType, Parent: "home"},
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

func importTreeMapsLocalIDs(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The file lists dependents first: other depends on leaf, and both parent
	// to root, so creation must reverse the file order — the asserted sequence
	// below fails for an engine that reports file order instead of creation
	// order. other is a sub-epic: between two leaves of one epic a blocked_by
	// is refused, since rank orders them.
	result, err := st.ImportTree(ctx, prefix, []storage.ImportTreeSpec{
		{LocalID: "other", Title: "other", IssueType: "epic", Topic: "core", Parent: "root", BlockedBy: []string{"leaf"}},
		{LocalID: "leaf", Title: "leaf", IssueType: "task", Topic: "core", Parent: "root"},
		{LocalID: "root", Title: "root", IssueType: "epic", Topic: "core"},
	})
	if err != nil {
		t.Fatalf("ImportTree error = %v", err)
	}
	// The report lists creates in creation order — referents before the specs
	// that name them — so the same file reports the same sequence on every run.
	assertStrings(t, "imported refs in creation order", refsOf(result.Created), []string{"root", "leaf", "other"})
	rootID := mustCreatedID(t, result.Created, "root")
	leafID := mustCreatedID(t, result.Created, "leaf")
	otherID := mustCreatedID(t, result.Created, "other")
	assertIssueIDs(t, "imported children", mustChildren(t, ctx, st, rootID), []string{leafID, otherID})

	// blocked_by in the file means "this issue is blocked by that one", so the
	// dependent is the edge's src.
	edges, err := st.ListRelationsForIssue(ctx, otherID, model.RelBlockedBy)
	if err != nil {
		t.Fatalf("ListRelationsForIssue error = %v", err)
	}
	if len(edges) != 1 || edges[0].SrcID != otherID || edges[0].DstID != leafID {
		t.Errorf("imported dependency edges = %+v, want other→leaf", edges)
	}
}

// createdID looks up one create's real id in a result's ordered report.
func createdID(created []storage.IDMapping, ref string) (string, bool) {
	for _, m := range created {
		if m.Ref == ref {
			return m.ID, true
		}
	}
	return "", false
}

func mustCreatedID(t *testing.T, created []storage.IDMapping, ref string) string {
	t.Helper()
	id, ok := createdID(created, ref)
	if !ok {
		t.Fatalf("created report = %+v, want a %q entry", created, ref)
	}
	return id
}

// refsOf renders a created report as its refs, so an ordering failure names
// the sequence instead of dumping whole structs.
func refsOf(created []storage.IDMapping) []string {
	refs := make([]string, 0, len(created))
	for _, m := range created {
		refs = append(refs, m.Ref)
	}
	return refs
}

func attributionStampsEvents(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// Before attribution is named, work is recorded unattributed rather than
	// half-attributed — the read-mode open of a never-mutated checkout.
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	before := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "anonymous", Topic: "core", ParentID: epic.ID})

	st.AttributeTo("stream-token")
	after := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "attributed", Topic: "core", ParentID: epic.ID})

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

func observersHearEveryChange(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	task := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "task", Topic: "core", ParentID: epic.ID})
	type heard struct {
		action model.Action
		id     string
		title  string
	}
	var got []heard
	st.ObserveChanges(func(c storage.Change, issue model.Issue) {
		got = append(got, heard{c.Action, issue.ID, issue.Title})
	})

	retitle := func(title string) storage.UpdateIssueInput { return storage.UpdateIssueInput{Title: &title} }
	if _, err := st.Apply(ctx, task.ID, storage.Change{Actor: "ada", Fields: retitle("by apply")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(ctx, task.ID, storage.Change{Actor: "ada", Action: model.Start{Assignee: "ada"}}); err != nil {
		t.Fatal(err)
	}
	// The observer hears a bulk batch's update as it hears a lone Apply, and
	// a change that landed before its batch failed is heard: it stays made.
	bulkTitle, neverTitle := "by bulk", "never"
	if _, err := st.BulkApply(ctx, "", "ada", []storage.BulkIssueSpec{
		{ID: task.ID, Title: &bulkTitle},
		{ID: task.ID + "-missing", Title: &neverTitle},
	}); err == nil {
		t.Fatal("BulkApply naming a missing issue succeeded; want an error")
	}
	want := []heard{
		{nil, task.ID, "by apply"},
		{model.Start{Assignee: "ada"}, task.ID, "by apply"},
		{nil, task.ID, "by bulk"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("observer heard %+v\nwant %+v", got, want)
	}
}

func localIssueCountTracksCreates(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// A store with nothing in it reports zero rather than failing: "no issues
	// yet" is a real state, not a fault.
	count, err := st.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount error = %v", err)
	}
	if count != 0 {
		t.Errorf("LocalIssueCount on a fresh engine = %d, want 0", count)
	}

	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "one", Topic: "core", ParentID: epic.ID})
	deleted := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "two", Topic: "core", ParentID: epic.ID})
	if _, err := st.Apply(ctx, deleted.ID, storage.Change{Action: model.Delete{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply delete error = %v", err)
	}

	count, err = st.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount error = %v", err)
	}
	// It counts what the store holds, which is the adopt-safety question —
	// the epic is a row, and a soft-deleted issue is still work that would be
	// lost.
	if count != 3 {
		t.Errorf("LocalIssueCount = %d, want 3", count)
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

func mustGet(t *testing.T, ctx context.Context, st storage.Store, id string) model.Issue {
	t.Helper()
	issue, err := st.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue(%q) error = %v", id, err)
	}
	return issue
}

// mustChildren lists every child of parentID the edge records, in every
// retention state, so a membership assertion is never narrowed by visibility.
func mustChildren(t *testing.T, ctx context.Context, st storage.Store, parentID string) []model.Issue {
	t.Helper()
	return mustList(t, ctx, st, storage.ListIssuesFilter{ParentIDs: []string{parentID}, IncludeArchived: true, IncludeDeleted: true})
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

// assertDistinctRanks fails when two listed issues carry the same rank. An
// engine's rank encoding is its own business — this suite reads rank only
// through the order of a listing — but one rank per issue is the contract
// behind that order, and the same law VerifyCandidate applies to a rebuilt
// workspace. Two issues sharing a key are two issues whose order is decided by
// the id tiebreak instead of by anything a caller ranked.
func assertDistinctRanks(t *testing.T, issues []model.Issue) {
	t.Helper()
	holder := make(map[string]string, len(issues))
	for _, issue := range issues {
		if issue.Rank == "" {
			continue
		}
		if other, taken := holder[issue.Rank]; taken {
			t.Errorf("issues %s and %s both hold rank %q; a rank orders one issue", other, issue.ID, issue.Rank)
			continue
		}
		holder[issue.Rank] = issue.ID
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

// assertRefused pins a rejected value to the contract's refusal type: the CLI
// reads it as "change the input", where any other error reads as a fault worth
// retrying.
func assertRefused(t *testing.T, err error, what string) {
	t.Helper()
	var refused model.ValidationError
	if !errors.As(err, &refused) {
		t.Errorf("%s error = %v, want model.ValidationError", what, err)
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
