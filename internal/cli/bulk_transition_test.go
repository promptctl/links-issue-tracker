package cli

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// bulkFailures runs a bulk transition and returns its per-item failures by id,
// failing the test unless the run failed as a bulk run fails.
func bulkFailures(t *testing.T, h readyTestHarness, stdout io.Writer, spec transitionSpec, args ...string) map[string]error {
	t.Helper()
	err := runBulkTransition(h.ctx, stdout, h.ap, args, spec)
	var bulk BulkFailureError
	if !errors.As(err, &bulk) {
		t.Fatalf("bulk %s %v error = %v (%T), want a BulkFailureError", spec.name, args, err, err)
	}
	if got := ExitCode(err); got == ExitOK {
		t.Fatalf("bulk %s ExitCode = %d, want non-zero", spec.name, got)
	}
	failed := map[string]error{}
	for _, f := range bulk.Failures {
		failed[f.IssueID] = f.Err
	}
	return failed
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
	failed := bulkFailures(t, h, &out, openSpec, "--ids", first+","+archived+","+second)

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
// verb's error: on a container, and on frozen work.
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
				failed := bulkFailures(t, h, io.Discard, spec, append([]string{"--ids", id}, actionFlags[spec.name]...)...)
				got := failed[id]
				if got == nil {
					t.Fatalf("bulk %s failures = %v, want %s", spec.name, failed, id)
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
	failed := bulkFailures(t, h, &out, startSpec, "--ids", contested.ID+","+free.ID)
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
	positional, err := parseLeaf(h.ctx, done, []string{"--ids", unplanned + "," + closed + "," + missing}, &out)
	if err != nil {
		t.Fatalf("parse bulk done error = %v", err)
	}
	if err := done.work(h.ctx, &out, h.ap, positional); err == nil {
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
