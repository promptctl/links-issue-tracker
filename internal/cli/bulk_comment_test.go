package cli

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
)

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
		if _, _, err := h.ap.Store.AddComment(h.ctx, storage.AddCommentInput{IssueID: id, Body: storagetest.CommentBody(t, "earlier note on "+id), CreatedBy: "ada"}); err != nil {
			t.Fatalf("AddComment(%s) error = %v", id, err)
		}
		prior[id] = h.comments(id)
	}

	var out bytes.Buffer
	ev, err := recordLeaf(bulkCommentLeaf(), h.ctx, cwdInvocation(&out, io.Discard, nil), h.ap, []string{"--ids", strings.Join(ids, ","), "--body", "the epic's premise is falsified"})
	if err != nil {
		t.Fatalf("bulk comment error = %v", err)
	}

	var wantOut strings.Builder
	items := ev.Occurrences["bulk.items"]
	if len(items) != len(ids) {
		t.Fatalf("bulk.items has %d entries, want %d", len(items), len(ids))
	}
	// The new comment is the one its item recorded; comments compare as sets,
	// so the check holds whatever order the store lists them in.
	for i, id := range ids {
		addedID, _ := items[i].(map[string]any)["bulk.item.facts"].(map[string]any)["comment.id"].(string)
		want := map[string]string{addedID: "the epic's premise is falsified"}
		for _, c := range prior[id] {
			want[c.ID] = c.Body
		}
		got := map[string]string{}
		for _, c := range h.comments(id) {
			got[c.ID] = c.Body
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s comments = %v, want its prior ones and the one bulk.items[%d] records, %v", id, got, i, want)
		}
		wantOut.WriteString(id + " " + addedID + "\n" + id + " ok\n")
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
	if _, err := recordLeaf(bulkCommentLeaf(), h.ctx, inv, h.ap, []string{"--ids", strings.Join(ids, ","), "--body-file", "-"}); err != nil {
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
// the run answers with the missing ticket's own reason, on its item's entry and
// on the run.
func TestBulkCommentFailsAMissingTicketAndCommentsOnTheRest(t *testing.T) {
	h := newReadyTestHarness(t)
	ids := h.bulkTickets(2)
	missing := "test-nope"
	var out bytes.Buffer
	ev, err := recordLeaf(bulkCommentLeaf(), h.ctx, cwdInvocation(&out, io.Discard, nil), h.ap, []string{"--ids", ids[0] + "," + missing + "," + ids[1], "--body", "note"})
	var bulk BulkError
	if !errors.As(err, &bulk) || len(bulk.Unlanded) != 1 || bulk.Unlanded[0].IssueID != missing {
		t.Fatalf("error = %v (%T), want one unlanded item, %s", err, err, missing)
	}
	if got := commandErrorReason(err); got != reasonEntityNotFound {
		t.Errorf("reason = %s, want entity_not_found", got)
	}
	if ev.Reason != "entity_not_found" {
		t.Errorf("event reason = %q, want entity_not_found", ev.Reason)
	}
	reasons := []any{}
	for _, raw := range ev.Occurrences["bulk.items"] {
		reasons = append(reasons, raw.(map[string]any)["bulk.item.reason"])
	}
	if len(reasons) != 3 || reasons[0].(*string) != nil || *reasons[1].(*string) != "entity_not_found" || reasons[2].(*string) != nil {
		t.Errorf("bulk.item.reason per entry = %v, want nil, entity_not_found, nil", reasons)
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

// A blank body is one refusal of the whole invocation, the one `lit comment
// add` gives, before any ticket is touched: no item runs, so a missing id among
// them does not mix its own error in, and no ticket gets a comment.
func TestBulkCommentRefusesABlankBodyBeforeAnyTicket(t *testing.T) {
	h := newReadyTestHarness(t)
	ids := h.bulkTickets(2)
	inv := cwdInvocation(io.Discard, io.Discard, nil)
	singleEv, single := recordLeaf(commentAddLeaf(), h.ctx, inv, h.ap, []string{ids[0], "--body", "  "})
	if single == nil || singleEv.ExitCode != ExitValidation {
		t.Fatalf("lit comment add with a blank body: error = %v, exit = %d, want a refusal with exit %d", single, singleEv.ExitCode, ExitValidation)
	}
	var out bytes.Buffer
	ev, err := recordLeaf(bulkCommentLeaf(), h.ctx, cwdInvocation(&out, io.Discard, nil), h.ap, []string{"--ids", ids[0] + ",test-nope," + ids[1], "--body", " \t\n"})
	if err == nil || err.Error() != single.Error() || ev.ExitCode != singleEv.ExitCode {
		t.Fatalf("bulk comment error = %v, exit = %d, want lit comment add's %q, exit %d", err, ev.ExitCode, single, singleEv.ExitCode)
	}
	if items := ev.Occurrences["bulk.items"]; len(items) != 0 || out.Len() != 0 {
		t.Errorf("bulk.items = %v, stdout = %q, want no item run", items, out.String())
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
	ev, err := recordLeaf(commentAddLeaf(), h.ctx, cwdInvocation(&out, io.Discard, nil), h.ap, []string{id, "--body", "note"})
	if err != nil {
		t.Fatalf("comment add error = %v", err)
	}
	written := h.comments(id)[0].ID
	if ev.Facts["comment.id"] != written || out.String() != id+" "+written+"\n" {
		t.Errorf("comment.id = %v, stdout = %q, want %s on both", ev.Facts["comment.id"], out.String(), written)
	}
}
