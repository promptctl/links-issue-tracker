package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// runCommentLeaf drives a comment leaf as runLeaf does, over inv, and returns
// the event the run recorded beside its error.
func runCommentLeaf(t *testing.T, h readyTestHarness, l appLeaf, inv Invocation, args ...string) (telemetry.Event, error) {
	t.Helper()
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(h.ctx, recorder)
	positional, err := parseLeaf(ctx, l, args, inv)
	if err == nil {
		err = l.work(ctx, inv, h.ap, positional)
	}
	return recorder.Finish(telemetry.End{}), err
}

func (h readyTestHarness) comments(issueID string) []model.Comment {
	h.t.Helper()
	detail, err := h.ap.Store.GetIssueDetail(h.ctx, issueID)
	if err != nil {
		h.t.Fatalf("GetIssueDetail(%q) error = %v", issueID, err)
	}
	return detail.Comments
}

func (h readyTestHarness) bulkTickets(n int) []string {
	h.t.Helper()
	epic := h.createIssue(storage.CreateIssueInput{Title: "epic", Topic: "bulk", IssueType: "epic"})
	ids := make([]string, n)
	for i := range ids {
		ids[i] = h.createIssue(storage.CreateIssueInput{Title: "ticket", Topic: "bulk", IssueType: "task", ParentID: epic.ID}).ID
	}
	return ids
}

// The ticket's acceptance: one invocation comments on five tickets, and each
// keeps the comments it already had.
func TestBulkCommentAddsToEveryTicketAndKeepsItsComments(t *testing.T) {
	h := newReadyTestHarness(t)
	ids := h.bulkTickets(5)
	prior := map[string][]model.Comment{}
	for _, id := range ids[1:3] {
		if _, _, err := h.ap.Store.AddComment(h.ctx, storage.AddCommentInput{IssueID: id, Body: "earlier note on " + id, CreatedBy: "ada"}); err != nil {
			t.Fatalf("AddComment(%s) error = %v", id, err)
		}
		prior[id] = h.comments(id)
	}

	var out bytes.Buffer
	ev, err := runCommentLeaf(t, h, bulkCommentLeaf(), cwdInvocation(&out, io.Discard, nil), "--ids", strings.Join(ids, ","), "--body", "the epic's premise is falsified")
	if err != nil {
		t.Fatalf("bulk comment error = %v", err)
	}

	var wantOut strings.Builder
	items := ev.Occurrences["bulk.items"]
	if len(items) != len(ids) {
		t.Fatalf("bulk.items has %d entries, want %d", len(items), len(ids))
	}
	for i, id := range ids {
		got := h.comments(id)
		if len(got) != len(prior[id])+1 {
			t.Fatalf("%s has %d comments, want its %d prior ones and the new one", id, len(got), len(prior[id]))
		}
		for j, c := range prior[id] {
			if got[j].ID != c.ID || got[j].Body != c.Body {
				t.Errorf("%s comment %d = %s %q, want the prior %s %q kept", id, j, got[j].ID, got[j].Body, c.ID, c.Body)
			}
		}
		added := got[len(got)-1]
		if added.Body != "the epic's premise is falsified" {
			t.Errorf("%s new comment body = %q", id, added.Body)
		}
		wantOut.WriteString(id + " " + added.ID + "\n" + id + " ok\n")
		facts := items[i].(map[string]any)["bulk.item.facts"].(map[string]any)
		if facts["comment.id"] != added.ID {
			t.Errorf("bulk.items[%d] comment.id = %v, want %s", i, facts["comment.id"], added.ID)
		}
	}
	if out.String() != wantOut.String() {
		t.Errorf("stdout = %q, want %q", out.String(), wantOut.String())
	}
}

// --body-file - reads stdin once, and every ticket gets that text.
func TestBulkCommentReadsStdinOnceForEveryTicket(t *testing.T) {
	h := newReadyTestHarness(t)
	ids := h.bulkTickets(3)
	inv := cwdInvocation(io.Discard, io.Discard, nil)
	inv.Stdin = strings.NewReader("deferred until\nthe store lands\n")
	if _, err := runCommentLeaf(t, h, bulkCommentLeaf(), inv, "--ids", strings.Join(ids, ","), "--body-file", "-"); err != nil {
		t.Fatalf("bulk comment error = %v", err)
	}
	for _, id := range ids {
		got := h.comments(id)
		if len(got) != 1 || got[0].Body != "deferred until\nthe store lands" {
			t.Errorf("%s comments = %+v, want one with the stdin text", id, got)
		}
	}
}

// A ticket that does not exist fails alone: the others get their comment, and
// the run exits as a partial bulk failure.
func TestBulkCommentFailsAMissingTicketAndCommentsOnTheRest(t *testing.T) {
	h := newReadyTestHarness(t)
	ids := h.bulkTickets(2)
	missing := "test-nope"
	var out bytes.Buffer
	ev, err := runCommentLeaf(t, h, bulkCommentLeaf(), cwdInvocation(&out, io.Discard, nil), "--ids", ids[0]+","+missing+","+ids[1], "--body", "note")
	var bulk BulkFailureError
	if !errors.As(err, &bulk) || len(bulk.Failures) != 1 || bulk.Failures[0].IssueID != missing {
		t.Fatalf("error = %v (%T), want one failure, for %s", err, err, missing)
	}
	if got := commandErrorReason(err); got != reasonBulkPartialFailure {
		t.Errorf("reason = %s, want bulk_partial_failure", got)
	}
	for _, id := range ids {
		if n := len(h.comments(id)); n != 1 {
			t.Errorf("%s has %d comments, want 1", id, n)
		}
		if !strings.Contains(out.String(), id+" ok\n") {
			t.Errorf("stdout = %q, want %s ok", out.String(), id)
		}
	}
	failed := ev.Occurrences["bulk.items"][1].(map[string]any)
	if _, ok := failed["bulk.item.facts"].(map[string]any)["comment.id"]; ok || failed["bulk.item.error"] == nil {
		t.Errorf("failed item entry = %v, want its error and no comment.id", failed)
	}
}

// A blank body is refused for every item with the error `lit comment add`
// gives, and no ticket gets a comment.
func TestBulkCommentRefusesABlankBodyOnEveryTicket(t *testing.T) {
	h := newReadyTestHarness(t)
	ids := h.bulkTickets(2)
	_, single := runCommentLeaf(t, h, commentAddLeaf(), cwdInvocation(io.Discard, io.Discard, nil), ids[0], "--body", "  ")
	if single == nil {
		t.Fatal("lit comment add with a blank body succeeded")
	}
	_, err := runCommentLeaf(t, h, bulkCommentLeaf(), cwdInvocation(io.Discard, io.Discard, nil), "--ids", strings.Join(ids, ","), "--body", "  ")
	var bulk BulkFailureError
	if !errors.As(err, &bulk) || len(bulk.Failures) != len(ids) {
		t.Fatalf("error = %v (%T), want every item failed", err, err)
	}
	for _, f := range bulk.Failures {
		if f.Err.Error() != single.Error() {
			t.Errorf("%s error = %q, want lit comment add's %q", f.IssueID, f.Err, single)
		}
	}
	for _, id := range ids {
		if n := len(h.comments(id)); n != 0 {
			t.Errorf("%s has %d comments, want none", id, n)
		}
	}
}

// `lit comment add` records the id of the comment it wrote, the one it prints.
func TestCommentAddRecordsTheCommentID(t *testing.T) {
	h := newReadyTestHarness(t)
	id := h.bulkTickets(1)[0]
	var out bytes.Buffer
	ev, err := runCommentLeaf(t, h, commentAddLeaf(), cwdInvocation(&out, io.Discard, nil), id, "--body", "note")
	if err != nil {
		t.Fatalf("comment add error = %v", err)
	}
	written := h.comments(id)[0].ID
	if ev.Facts["comment.id"] != written || out.String() != id+" "+written+"\n" {
		t.Errorf("comment.id = %v, stdout = %q, want %s on both", ev.Facts["comment.id"], out.String(), written)
	}
}
