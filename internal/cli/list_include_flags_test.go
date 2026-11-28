package cli

import (
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestListIncludeFlagsAdmitArchivedAndDeleted pins that an archived or deleted
// issue is listed only when its flag admits it, and that the flags list it.
func TestListIncludeFlagsAdmitArchivedAndDeleted(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	// Three plainly-visible issues plus one archived and one deleted, so every
	// dimension has an observable effect: sort orders them, limit truncates,
	// and the visibility flags decide whether the last two appear at all.
	epic := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("sup"), Lane: storage.SharedLane, IssueType: model.TypeEpic, Topic: "sup"})
	a := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("alpha"), Lane: storage.SharedLane, IssueType: model.TypeTask, Topic: "sup", ParentID: epic.ID})
	b := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("bravo"), Lane: storage.SharedLane, IssueType: model.TypeTask, Topic: "sup", ParentID: epic.ID})
	c := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("charlie"), Lane: storage.SharedLane, IssueType: model.TypeTask, Topic: "sup", ParentID: epic.ID})
	arch := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("archived one"), Lane: storage.SharedLane, IssueType: model.TypeTask, Topic: "sup", ParentID: epic.ID})
	del := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("deleted one"), Lane: storage.SharedLane, IssueType: model.TypeTask, Topic: "sup", ParentID: epic.ID})
	if _, err := h.ap.Store.Apply(h.ctx, arch.ID, storage.Change{Action: model.Archive{}, Actor: "tester", Reason: "shelve"}); err != nil {
		t.Fatalf("Apply(archive) error = %v", err)
	}
	if _, err := h.ap.Store.Apply(h.ctx, del.ID, storage.Change{Action: model.Delete{}, Actor: "tester", Reason: "drop"}); err != nil {
		t.Fatalf("Apply(delete) error = %v", err)
	}

	included := runLs(t, h.ap, "--include-archived", "--include-deleted")
	for _, id := range []string{arch.ID.Raw(), del.ID.Raw()} {
		if !strings.Contains(included, id) {
			t.Fatalf("ls --include-archived --include-deleted omitted %q; output:\n%s", id, included)
		}
	}

	// Baseline: without the flags the archived and deleted issues are hidden,
	// proving the flags are what surfaced them.
	bare := runLs(t, h.ap)
	for _, id := range []string{a.ID.Raw(), b.ID.Raw(), c.ID.Raw()} {
		if !strings.Contains(bare, id) {
			t.Fatalf("bare ls omitted active issue %q; output:\n%s", id, bare)
		}
	}
	for _, id := range []string{arch.ID.Raw(), del.ID.Raw()} {
		if strings.Contains(bare, id) {
			t.Fatalf("bare ls leaked hidden issue %q; output:\n%s", id, bare)
		}
	}
}
