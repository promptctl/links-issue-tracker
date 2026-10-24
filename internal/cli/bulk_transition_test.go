package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// bulkFailures runs a bulk transition and returns its unlanded items' errors by
// id and the run's reason, failing the test unless some item did not land.
func bulkFailures(t *testing.T, h readyTestHarness, stdout io.Writer, spec transitionSpec, args ...string) (map[string]error, commandReason) {
	t.Helper()
	err := runBulkTransition(h.ctx, stdout, h.ap, args, spec)
	var bulk BulkError
	if !errors.As(err, &bulk) {
		t.Fatalf("bulk %s %v error = %v (%T), want a BulkError", spec.name, args, err, err)
	}
	if got := ExitCode(err); got == ExitOK {
		t.Fatalf("bulk %s ExitCode = %d, want non-zero", spec.name, got)
	}
	failed := map[string]error{}
	for _, f := range bulk.Unlanded {
		failed[f.IssueID] = f.Err
	}
	return failed, commandErrorReason(err)
}

// The ticket's acceptance, on a ticket `lit open` refuses: an archived one. A
// ticket already open is no refusal, since a same-state transition is a no-op
// for `lit open` too.
func TestBulkOpenReopensTheRestAndFailsTheTicketOpenRefuses(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "epic", Topic: "bulk", IssueType: "epic"})
	child := func(title string) string {
		return h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "bulk", IssueType: "task", ParentID: epic.ID}).ID
	}
	first, archived, second := child("first"), child("archived"), child("second")
	for _, id := range []string{first, archived, second} {
		h.applyAction(id, model.Done{}, "")
	}
	h.applyAction(archived, model.Archive{}, "")
	single := h.runTransitionErr(archived, openSpec)
	if single == nil {
		t.Fatalf("lit open on archived %s succeeded; the test needs a ticket it refuses", archived)
	}

	var out bytes.Buffer
	failed, _ := bulkFailures(t, h, &out, openSpec, "--ids", first+","+archived+","+second)

	if len(failed) != 1 || failed[archived] == nil {
		t.Fatalf("failures = %v, want exactly %s", failed, archived)
	}
	if failed[archived].Error() != single.Error() {
		t.Errorf("bulk open's error for %s = %q, want lit open's %q", archived, failed[archived], single)
	}
	if want := first + " ok\n" + second + " ok\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	for _, id := range []string{first, second} {
		if got := h.issueDetail(id).StatusValue(); got != string(model.StateOpen) {
			t.Errorf("%s status = %s, want open", id, got)
		}
	}
}

// Every bulk transition refuses what its single verb refuses, with the single
// verb's error and reason: on a container, and on frozen work.
func TestBulkTransitionsRefuseWhatTheSingleVerbRefusesInItsWords(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "bulk-parity-sess")
	for _, spec := range []transitionSpec{startSpec, doneSpec, closeSpec, openSpec} {
		for _, target := range []string{"epic", "archived"} {
			t.Run(spec.name+"/"+target, func(t *testing.T) {
				h := newReadyTestHarness(t)
				id := h.epicFixture(1, 0)
				if target == "archived" {
					id = h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "frozen", Topic: "bulk", IssueType: "task", ParentID: id}).ID
					h.applyAction(id, model.Archive{}, "")
				}
				single := h.runTransitionErr(id, spec, actionFlags[spec.name]...)
				if single == nil {
					t.Fatalf("lit %s on the %s succeeded; the test needs a refusal", spec.name, target)
				}
				failed, reason := bulkFailures(t, h, io.Discard, spec, append([]string{"--ids", id}, actionFlags[spec.name]...)...)
				got := failed[id]
				if got == nil {
					t.Fatalf("bulk %s failures = %v, want %s", spec.name, failed, id)
				}
				if want := commandErrorReason(single); reason != want {
					t.Errorf("bulk %s reason = %s, want lit %s's %s", spec.name, reason, spec.name, want)
				}
				if got.Error() != single.Error() || reflect.TypeOf(got) != reflect.TypeOf(single) {
					t.Errorf("bulk %s error = %q (%T), want lit %s's %q (%T)", spec.name, got, got, spec.name, single, single)
				}
			})
		}
	}
}

// Bulk start runs start's takeover gate on every item: a lane another
// checkout holds is refused without --take, the other items still start, and
// --take takes it.
func TestBulkStartRefusesAHeldLaneUnlessTaken(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "bulk-start-sess")
	h := newReadyTestHarness(t)
	held := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "held epic", Topic: "bulk", IssueType: "epic"})
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "theirs", Topic: "bulk", IssueType: "task", ParentID: held.ID})
	contested := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "contested", Topic: "bulk", IssueType: "task", ParentID: held.ID})
	free := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "free", Topic: "bulk", IssueType: "task", ParentID: h.epicFixture(0, 0)})
	h.asCheckout("otherstream01")
	h.transition(theirs.ID, model.Start{Assignee: "other"})
	h.asCheckout(h.ap.Stream.Value())

	var out bytes.Buffer
	failed, _ := bulkFailures(t, h, &out, startSpec, "--ids", contested.ID+","+free.ID)
	var refusal takeoverUnconfirmedError
	if len(failed) != 1 || !errors.As(failed[contested.ID], &refusal) {
		t.Fatalf("failures = %v, want only %s, refused by the takeover gate", failed, contested.ID)
	}
	if !strings.Contains(out.String(), free.ID+" ok\n") || strings.Contains(out.String(), contested.ID) {
		t.Errorf("stdout = %q, want %s ok and no mention of %s", out.String(), free.ID, contested.ID)
	}
	if got := h.issueDetail(contested.ID).StatusValue(); got != string(model.StateOpen) {
		t.Errorf("%s status = %s after a refused takeover, want open", contested.ID, got)
	}

	out.Reset()
	if err := runBulkTransition(h.ctx, &out, h.ap, []string{"--ids", contested.ID, "--take"}, startSpec); err != nil {
		t.Fatalf("bulk start --take error = %v", err)
	}
	if got := h.issueDetail(contested.ID).StatusValue(); got != string(model.StateInProgress) {
		t.Errorf("%s status = %s after --take, want in_progress", contested.ID, got)
	}
}

// Each bulk item's facts land on its own bulk.items entry, in id order, and
// none of them on the event itself, where the last item would replace the
// others'.
func TestBulkDoneRecordsEachItemsFactsOnItsOwnEntry(t *testing.T) {
	h := newReadyTestHarness(t)
	recorder := telemetry.Begin(telemetry.Event{})
	h.ctx = telemetry.With(h.ctx, recorder)
	epic := h.epicFixture(0, 0)
	unplanned := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "unplanned", Topic: "bulk", IssueType: "task", ParentID: epic}).ID
	closed := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "closed", Topic: "bulk", IssueType: "task", ParentID: epic}).ID
	h.applyAction(closed, model.Done{}, "")
	const missing = "test-missing"

	// runLeaf's phases under this test's own recorder, which runLeaf would
	// replace with a fresh one.
	var out bytes.Buffer
	done := bulkTransitionLeaf(doneSpec)()
	positional, err := parseLeaf(h.ctx, done, []string{"--ids", unplanned + "," + closed + "," + missing}, cwdInvocation(&out, &out, nil))
	if err != nil {
		t.Fatalf("parse bulk done error = %v", err)
	}
	if err := done.work(h.ctx, cwdInvocation(&out, &out, nil), h.ap, positional); err == nil {
		t.Fatalf("bulk done with %s returned nil, want its failure", missing)
	}

	// A done's plan warning prints as `lit done` prints it, ahead of the
	// item's ok line.
	warning := "warning: finished without an approved plan: "
	if w, ok := strings.Index(out.String(), warning), strings.Index(out.String(), unplanned+" ok\n"); w < 0 || ok < 0 || w > ok {
		t.Errorf("stdout = %q, want %q ahead of %q", out.String(), warning, unplanned+" ok")
	}

	ev := writtenEvent(t, recorder.Finish(telemetry.End{}))
	if _, leaked := ev.Facts["done.plan_standing"]; leaked {
		t.Errorf("event facts carry done.plan_standing = %v; each item's belongs on its own entry", ev.Facts["done.plan_standing"])
	}
	items := ev.Occurrences["bulk.items"]
	type item struct {
		id       string
		standing any
		failed   bool
	}
	var got []item
	for _, raw := range items {
		entry := raw.(map[string]any)
		facts := entry["bulk.item.facts"].(map[string]any)
		got = append(got, item{id: entry["bulk.item.id"].(string), standing: facts["done.plan_standing"], failed: entry["bulk.item.error"] != nil})
	}
	want := []item{
		{id: unplanned, standing: "no_plan"},
		{id: closed, standing: "already_closed"},
		{id: missing, standing: nil, failed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bulk.items = %+v, want %+v", got, want)
	}
}

// applyRefusingStore is a store whose every Apply fails with err, for a write
// that passes every gate ahead of it and then does not land.
type applyRefusingStore struct {
	storage.Store
	err error
}

func (s applyRefusingStore) Apply(context.Context, string, storage.Change) (storage.Applied, error) {
	return storage.Applied{}, s.err
}

// The takeover line is what a landed start owes: a --take whose start then
// fails at Apply prints none, and the same start landing prints it ahead of
// its ok line.
func TestBulkStartTakeAnnouncesOnlyATakeoverThatLanded(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "bulk-start-sess")
	h := newReadyTestHarness(t)
	held := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "held epic", Topic: "bulk", IssueType: "epic"})
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "theirs", Topic: "bulk", IssueType: "task", ParentID: held.ID})
	contested := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "contested", Topic: "bulk", IssueType: "task", ParentID: held.ID})
	h.asCheckout("otherstream01")
	h.transition(theirs.ID, model.Start{Assignee: "other"})
	h.asCheckout(h.ap.Stream.Value())
	const takeover = " — taking over (--take)\n"

	live := h.ap.Store
	refusal := errors.New("store refused the write")
	h.ap.Store = applyRefusingStore{Store: live, err: refusal}
	var out bytes.Buffer
	failed, _ := bulkFailures(t, h, &out, startSpec, "--ids", contested.ID, "--take")
	if !errors.Is(failed[contested.ID], refusal) {
		t.Fatalf("failures = %v, want %s failed with %v", failed, contested.ID, refusal)
	}
	if strings.Contains(out.String(), takeover) {
		t.Errorf("stdout = %q, want no takeover line for a start that did not land", out.String())
	}

	h.ap.Store = live
	out.Reset()
	if err := runBulkTransition(h.ctx, &out, h.ap, []string{"--ids", contested.ID, "--take"}, startSpec); err != nil {
		t.Fatalf("bulk start --take error = %v", err)
	}
	if line, ok := strings.Index(out.String(), takeover), strings.Index(out.String(), contested.ID+" ok\n"); line < 0 || ok < 0 || line > ok {
		t.Errorf("stdout = %q, want the takeover line ahead of %s ok", out.String(), contested.ID)
	}
}

// recordedItemError declares facts and counts about itself, as a store's
// typed errors do.
type recordedItemError struct{}

func (recordedItemError) Error() string { return "refused, with a record" }

func (recordedItemError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{"item.decision": "refused"}, map[string]int{"item.tries": 2}
}

// A failed item's entry carries what its error declared about itself, as a
// failed single command's event does, wrapped or not.
func TestBulkItemEntryCarriesWhatItsErrorDeclared(t *testing.T) {
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	err := runBulkOver(ctx, io.Discard, []string{"a"}, func(context.Context, string) (string, error) {
		return "", fmt.Errorf("apply: %w", recordedItemError{})
	})
	var bulk BulkError
	if !errors.As(err, &bulk) {
		t.Fatalf("runBulkOver error = %v (%T), want a BulkError", err, err)
	}
	entry := recorder.Finish(telemetry.End{}).Occurrences["bulk.items"][0].(map[string]any)
	facts, counts := entry["bulk.item.facts"].(map[string]any), entry["bulk.item.counts"].(map[string]int)
	if facts["item.decision"] != "refused" || counts["item.tries"] != 2 {
		t.Errorf("entry facts = %v, counts = %v, want the error's item.decision=refused and item.tries=2", facts, counts)
	}
}

// failingWriter refuses every write with err.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// An item whose change landed is never reported failed: a stdout that cannot
// take its report ends the run with the write's error, not with the item as a
// failure to re-run.
func TestBulkEndsAtAStdoutItCannotWriteWithoutFailingTheItem(t *testing.T) {
	ctx := telemetry.With(context.Background(), telemetry.Begin(telemetry.Event{}))
	closed := errors.New("stdout closed")
	err := runBulkOver(ctx, failingWriter{err: closed}, []string{"a"}, func(context.Context, string) (string, error) {
		return "claim transferred: x -> y\n", nil
	})
	var bulk BulkError
	if !errors.Is(err, closed) || errors.As(err, &bulk) {
		t.Errorf("runBulkOver error = %v (%T), want the write's %v and no item failure", err, err, closed)
	}
}

// Start's usage line names --take, singly and in bulk: it is the flag a
// refused takeover tells the caller to pass.
func TestStartUsageNamesTake(t *testing.T) {
	for name, usage := range map[string]string{
		"lit start":      transitionLeaf(startSpec).usage,
		"lit bulk start": bulkTransitionLeaf(startSpec)().usage,
	} {
		if !strings.Contains(usage, "[--take]") {
			t.Errorf("%s usage = %q, want it to name [--take]", name, usage)
		}
	}
}

// runBulkRecorded runs `lit bulk <spec.name>` over ids, resolved through the
// bulk family's own row, under a recorder of the test's own; it requires every
// item to land and returns the run's bulk.items entries as (id, error) pairs.
func runBulkRecorded(t *testing.T, h readyTestHarness, spec transitionSpec, ids []string) [][2]any {
	t.Helper()
	row, err := bulkFamily.resolve(underInvocation(h.ctx), []string{spec.name})
	if err != nil {
		t.Fatalf("lit bulk %s does not resolve: %v", spec.name, err)
	}
	var out bytes.Buffer
	ev, err := recordLeaf(row.declare(), h.ctx, cwdInvocation(&out, &out, nil), h.ap, []string{"--ids", strings.Join(ids, ",")})
	if err != nil {
		t.Fatalf("bulk %s error = %v", spec.name, err)
	}
	if want := strings.Join(ids, " ok\n") + " ok\n"; out.String() != want {
		t.Errorf("bulk %s stdout = %q, want %q", spec.name, out.String(), want)
	}
	var items [][2]any
	for _, raw := range writtenEvent(t, ev).Occurrences["bulk.items"] {
		entry := raw.(map[string]any)
		items = append(items, [2]any{entry["bulk.item.id"], entry["bulk.item.error"]})
	}
	return items
}

// The ticket's acceptance: a batch moved out of the flow by bulk archive or
// bulk delete comes back to its exact prior state through the inverse bulk
// verb, whether each live ticket was open, in progress or done, with its
// labels, edges and comments. Each run leaves one clean bulk.items entry per
// ticket. A live batch only: delete
// drops an archived ticket's archive stamp, so restore lands it live
// (lifecycle.Retain).
func TestBulkRetentionVerbsRoundTripTheirInverse(t *testing.T) {
	for _, pair := range []struct {
		out, back transitionSpec
		away      func(model.Retention) bool
	}{
		{archiveSpec, unarchiveSpec, func(r model.Retention) bool { _, ok := r.(model.Archived); return ok }},
		{deleteSpec, restoreSpec, func(r model.Retention) bool { _, ok := r.(model.Deleted); return ok }},
	} {
		t.Run(pair.out.name+"/"+pair.back.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			ids := h.bulkTickets(3)
			h.setLabels(ids[0], "keep")
			if _, err := h.ap.Store.AddRelation(h.ctx, storage.AddRelationInput{SrcID: ids[0], DstID: ids[1], Type: model.RelRelatedTo, CreatedBy: "ada"}); err != nil {
				t.Fatalf("AddRelation error = %v", err)
			}
			note, err := model.ParseCommentBody("kept through the round trip")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := h.ap.Store.AddComment(h.ctx, storage.AddCommentInput{IssueID: ids[2], Body: note, CreatedBy: "ada"}); err != nil {
				t.Fatalf("AddComment error = %v", err)
			}
			h.applyAction(ids[1], model.Start{Assignee: "ada"}, "")
			h.applyAction(ids[2], model.Done{}, "")
			state := func(id string) model.IssueDetail {
				t.Helper()
				d, err := h.ap.Store.GetIssueDetail(h.ctx, id)
				if err != nil {
					t.Fatalf("GetIssueDetail(%q) error = %v", id, err)
				}
				return model.IssueDetail{Issue: d.Issue, Relations: d.Relations, Comments: d.Comments}
			}
			prior := map[string]model.IssueDetail{}
			for _, id := range ids {
				prior[id] = state(id)
			}
			if len(prior[ids[0]].Relations) == 0 || len(prior[ids[2]].Comments) == 0 {
				t.Fatalf("fixture details = %+v; the round trip needs an edge and a comment to keep", prior)
			}
			wantItems := make([][2]any, len(ids))
			for i, id := range ids {
				wantItems[i] = [2]any{id, nil}
			}

			if got := runBulkRecorded(t, h, pair.out, ids); !reflect.DeepEqual(got, wantItems) {
				t.Errorf("bulk %s bulk.items = %v, want %v", pair.out.name, got, wantItems)
			}
			for _, id := range ids {
				if r := h.issueDetail(id).Retention(); !pair.away(r) {
					t.Fatalf("after bulk %s, %s retention = %#v", pair.out.name, id, r)
				}
			}
			if got := runBulkRecorded(t, h, pair.back, ids); !reflect.DeepEqual(got, wantItems) {
				t.Errorf("bulk %s bulk.items = %v, want %v", pair.back.name, got, wantItems)
			}
			for _, id := range ids {
				got := state(id)
				// The round trip is two writes, so the write clock moved; every
				// other field is the ticket's state and must be what it was.
				got.Issue.UpdatedAt = prior[id].Issue.UpdatedAt
				if !reflect.DeepEqual(got, prior[id]) {
					t.Errorf("%s after bulk %s then bulk %s = %+v, want %+v", id, pair.out.name, pair.back.name, got, prior[id])
				}
			}
		})
	}
}

// Each retention verb in bulk answers every ticket its single verb will not
// move — one the verb refuses, and one already where the verb would put it —
// with the single verb's error and the ticket untouched, and the other ticket
// in the run still lands. The run answers with the single verb's reason, so an
// already-held ticket is never a failure to re-run.
// The rows are every cell of lifecycle.Retain that does not move.
func TestBulkRetentionVerbsAnswerEachTicketTheyCannotMoveAsTheSingleVerbDoes(t *testing.T) {
	type retention struct {
		name string
		via  []model.Action // from a new live ticket
	}
	var (
		live     = retention{"live", nil}
		archived = retention{"archived", []model.Action{model.Archive{}}}
		deleted  = retention{"deleted", []model.Action{model.Delete{}}}
	)
	for _, tc := range []struct {
		spec         transitionSpec
		stuck, moves retention // one ticket where the verb will not move it, the other where it does
	}{
		{archiveSpec, archived, live},
		{archiveSpec, deleted, live},
		{unarchiveSpec, live, archived},
		{unarchiveSpec, deleted, archived},
		{deleteSpec, deleted, live},
		{restoreSpec, live, deleted},
		{restoreSpec, archived, deleted},
	} {
		t.Run(tc.spec.name+"/"+tc.stuck.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			ids := h.bulkTickets(2)
			stuck, landed := ids[0], ids[1]
			for _, a := range tc.stuck.via {
				h.applyAction(stuck, a, "")
			}
			for _, a := range tc.moves.via {
				h.applyAction(landed, a, "")
			}
			single := h.runTransitionErr(stuck, tc.spec)
			if single == nil {
				t.Fatalf("lit %s on %s succeeded; the test needs a ticket it does not move", tc.spec.name, stuck)
			}
			before := h.issueDetail(stuck)
			var out bytes.Buffer
			failed, reason := bulkFailures(t, h, &out, tc.spec, "--ids", stuck+","+landed)
			if want := commandErrorReason(single); reason != want {
				t.Errorf("bulk %s reason = %s (exit %d), want lit %s's %s (exit %d)", tc.spec.name, reason, reason.exit, tc.spec.name, want, want.exit)
			}
			if after := h.issueDetail(stuck); !reflect.DeepEqual(after, before) {
				t.Errorf("bulk %s wrote %s it did not move: %+v, was %+v", tc.spec.name, stuck, after, before)
			}
			if len(failed) != 1 || failed[stuck] == nil {
				t.Fatalf("bulk %s failures = %v, want exactly %s", tc.spec.name, failed, stuck)
			}
			if got := failed[stuck]; got.Error() != single.Error() || reflect.TypeOf(got) != reflect.TypeOf(single) {
				t.Errorf("bulk %s error = %q (%T), want lit %s's %q (%T)", tc.spec.name, got, got, tc.spec.name, single, single)
			}
			if want := landed + " ok\n"; out.String() != want {
				t.Errorf("bulk %s stdout = %q, want %q", tc.spec.name, out.String(), want)
			}
		})
	}
}
