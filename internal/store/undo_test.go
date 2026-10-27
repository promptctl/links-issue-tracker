package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func newUndoStore(t *testing.T) (*Store, string) {
	t.Helper()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	st, err := Open(context.Background(), testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, doltRoot
}

func mustCreate(t *testing.T, st *Store, in storage.CreateIssueInput) model.Issue {
	t.Helper()
	in.Prefix, in.Topic = "test", "undo"
	issue, err := st.CreateIssue(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateIssue(%s) error = %v", in.Title, err)
	}
	return issue
}

// headWrite is the newest write's id.
func headWrite(t *testing.T, st *Store) string {
	t.Helper()
	head, err := readDoltHead(context.Background(), st.db)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	return head
}

func parentsNow(t *testing.T, st *Store, child string) []string {
	t.Helper()
	parents, err := st.edgeTargetsAt(context.Background(), liveRevision, model.RelParentChild, []string{child})
	if err != nil {
		t.Fatalf("read parent of %s: %v", child, err)
	}
	return parents[child]
}

// The ticket's own case: a run of reparents moved tickets under the wrong
// epic. Reverting the run puts every ticket back under the epic it had, keeps
// the history the run wrote, and names the writes that make up the undo, which
// revert in turn.
func TestRevertWritesPutsARunOfReparentsBack(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	from := mustCreate(t, st, storage.CreateIssueInput{Title: "from", IssueType: "epic"})
	to := mustCreate(t, st, storage.CreateIssueInput{Title: "to", IssueType: "epic"})
	var tasks, run []string
	for _, title := range []string{"one", "two", "three"} {
		tasks = append(tasks, mustCreate(t, st, storage.CreateIssueInput{Title: title, ParentID: from.ID}).ID)
	}
	events, err := st.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents() error = %v", err)
	}
	for _, task := range tasks {
		if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: task, ParentID: to.ID}); err != nil {
			t.Fatalf("SetParent(%s) error = %v", task, err)
		}
		run = append(run, headWrite(t, st))
	}

	listed, err := st.ListWrites(ctx, 3)
	if err != nil {
		t.Fatalf("ListWrites() error = %v", err)
	}
	for i, write := range listed {
		if write.ID != run[len(run)-1-i] || len(write.Unrevertable) != 0 || len(write.Moves) != 1 || write.Moves[0].Slot.Kind != storage.ParentSlot {
			t.Fatalf("listed write %d = %+v, want the reparent %s with one parent move and nothing unrevertable", i, write, run[len(run)-1-i])
		}
	}

	result, err := st.RevertWrites(ctx, run, "undoer")
	if err != nil {
		t.Fatalf("RevertWrites() error = %v", err)
	}
	for _, task := range tasks {
		if got := parentsNow(t, st, task); !slices.Equal(got, []string{from.ID}) {
			t.Errorf("%s parent = %v, want %s back", task, got, from.ID)
		}
	}
	after, err := st.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents() error = %v", err)
	}
	if len(after) != len(events) {
		t.Errorf("events = %d after the undo, want the %d there were: a reparent records none and the undo removes none", len(after), len(events))
	}
	if len(result.Moves) != 3 || len(result.Landed) == 0 {
		t.Fatalf("result = %+v, want three moves and the commits it landed", result)
	}

	if _, err := st.RevertWrites(ctx, result.Landed, "undoer"); err != nil {
		t.Fatalf("RevertWrites(the undo's own writes) error = %v", err)
	}
	for _, task := range tasks {
		if got := parentsNow(t, st, task); !slices.Equal(got, []string{to.ID}) {
			t.Errorf("after undoing the undo, %s parent = %v, want %s", task, got, to.ID)
		}
	}
}

// A field edit, labels among the fields, reverts; the undo records its own
// field change with the undo's reason, and itself reverts.
func TestRevertWritesPutsAFieldEditBack(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "epic", IssueType: "epic"})
	task := mustCreate(t, st, storage.CreateIssueInput{Title: "before", ParentID: epic.ID, Labels: []string{"keep"}})
	title, labels := "after", []string{"keep", "new"}
	if _, err := st.Apply(ctx, task.ID, storage.Change{Actor: "editor", Fields: storage.UpdateIssueInput{Title: &title, Labels: &labels}}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	edit := headWrite(t, st)
	result, err := st.RevertWrites(ctx, []string{edit}, "undoer")
	if err != nil {
		t.Fatalf("RevertWrites() error = %v", err)
	}
	got, err := st.GetIssue(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got.Title != "before" || !slices.Equal(got.Labels, []string{"keep"}) {
		t.Errorf("after the undo: title %q, labels %v; want before, [keep]", got.Title, got.Labels)
	}
	detail, err := st.GetIssueDetail(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	last := detail.Events[len(detail.Events)-1]
	if last.Reason != "undo "+edit || last.Actor != "undoer" {
		t.Errorf("the undo's event = %+v, want reason %q by undoer", last, "undo "+edit)
	}
	if _, err := st.RevertWrites(ctx, result.Landed, "undoer"); err != nil {
		t.Fatalf("RevertWrites(the undo) error = %v", err)
	}
	if got, _ := st.GetIssue(ctx, task.ID); got.Title != "after" {
		t.Errorf("after undoing the undo, title = %q, want after", got.Title)
	}
}

// A label verb is an edit of the labels field, so its write reverts like any
// edit: `lit label add` and `lit label rm` are each put back.
func TestRevertWritesPutsALabelVerbBack(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "epic", IssueType: "epic"})
	task := mustCreate(t, st, storage.CreateIssueInput{Title: "task", ParentID: epic.ID, Labels: []string{"keep"}})
	for _, tc := range []struct {
		verb storage.ChangePlan
		want []string
	}{
		{storage.AddLabel{Name: "new", Actor: "editor"}, []string{"keep"}},
		{storage.RemoveLabel{Name: "keep", Actor: "editor"}, []string{"keep"}},
	} {
		if _, err := st.Apply(ctx, task.ID, tc.verb); err != nil {
			t.Fatalf("Apply(%+v) error = %v", tc.verb, err)
		}
		if _, err := st.RevertWrites(ctx, []string{headWrite(t, st)}, "undoer"); err != nil {
			t.Fatalf("RevertWrites(%+v) error = %v", tc.verb, err)
		}
		got, err := st.GetIssue(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetIssue() error = %v", err)
		}
		if !slices.Equal(got.Labels, tc.want) {
			t.Errorf("after undoing %+v, labels = %v, want %v", tc.verb, got.Labels, tc.want)
		}
	}
}

// What no document can put back refuses the undo by name, and nothing is
// written: a lifecycle write, whose claim is derived from its event; a label
// write a lit before label verbs went through Apply made, which moved the
// labels field with no change row; a ticket's creation; a purge.
func TestRevertWritesRefusesWhatNoDocumentCanPutBack(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "epic", IssueType: "epic"})
	task := mustCreate(t, st, storage.CreateIssueInput{Title: "task", ParentID: epic.ID})
	created := headWrite(t, st)
	if _, err := st.Apply(ctx, task.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply(start) error = %v", err)
	}
	started := headWrite(t, st)
	// The label row an older lit's `lit label add` wrote, with no event.
	if err := st.withMutation(ctx, "add label", func(ctx context.Context, tx *sql.Tx) error {
		return insertLabelTx(ctx, tx, model.Label{IssueID: task.ID, Name: "x", CreatedAt: time.Now(), CreatedBy: "ada"})
	}); err != nil {
		t.Fatalf("insert label row error = %v", err)
	}
	labeled := headWrite(t, st)
	doomed := mustCreate(t, st, storage.CreateIssueInput{Title: "doomed", ParentID: epic.ID})
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: doomed.ID, Actor: "ada"}); err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	purged := headWrite(t, st)
	for _, tc := range []struct {
		write, want string
	}{
		{started, "`lit start`"},
		{labeled, "no field-change record"},
		{created, "created"},
		{purged, "removed"},
	} {
		head := headWrite(t, st)
		_, err := st.RevertWrites(ctx, []string{tc.write}, "undoer")
		var refused storage.RevertRefusedError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("RevertWrites(%s) = %v, want a refusal naming %s", tc.want, err, tc.want)
		}
		if headWrite(t, st) != head {
			t.Errorf("a refused undo of %s wrote a commit", tc.want)
		}
	}
}

// A write that makes or removes a ticket lists that one fact: the ticket's
// labels, edges, and events are its making or removal, not moves of their own
// or refusals beside it.
func TestListWritesNamesAMadeOrRemovedTicketOnce(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "epic", IssueType: "epic"})
	doomed := mustCreate(t, st, storage.CreateIssueInput{Title: "doomed", ParentID: epic.ID, Labels: []string{"x"}})
	created := headWrite(t, st)
	// Edges with the removed ticket at the far end are its removal too.
	other := mustCreate(t, st, storage.CreateIssueInput{Title: "other", IssueType: "epic"})
	blocked := mustCreate(t, st, storage.CreateIssueInput{Title: "blocked", ParentID: other.ID})
	for _, in := range []storage.AddRelationInput{
		{SrcID: blocked.ID, DstID: doomed.ID, Type: model.RelBlockedBy},
		{SrcID: blocked.ID, DstID: doomed.ID, Type: model.RelRelatedTo},
	} {
		if _, err := st.AddRelation(ctx, in); err != nil {
			t.Fatalf("AddRelation(%s) error = %v", in.Type, err)
		}
	}
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: doomed.ID, Actor: "ada"}); err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	purged := headWrite(t, st)
	listed, err := st.ListWrites(ctx, 7)
	if err != nil {
		t.Fatalf("ListWrites() error = %v", err)
	}
	// A purge also writes its own record, which is a fact of its own.
	want := map[string][]string{purged: {doomed.ID + " removed", "the purges table changed"}, created: {doomed.ID + " created"}}
	for _, w := range listed {
		if want[w.ID] == nil {
			continue
		}
		var what []string
		for _, u := range w.Unrevertable {
			what = append(what, u.What)
		}
		if len(w.Moves) != 0 || !slices.Equal(what, want[w.ID]) {
			t.Errorf("write %s lists moves %v and %q, want no moves and only %q", w.ID, w.Moves, what, want[w.ID])
		}
		delete(want, w.ID)
	}
	if len(want) != 0 {
		t.Errorf("writes %v not listed", want)
	}
}

// A write that rewrites the change rows of an event it did not add moved
// history, and lists it, rather than reading as a write that moved nothing.
func TestListWritesNamesAnEventChangeRewritten(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	task := mustCreate(t, st, storage.CreateIssueInput{Title: "before", IssueType: "epic"})
	title := "after"
	if _, err := st.Apply(ctx, task.ID, storage.Change{Actor: "editor", Fields: storage.UpdateIssueInput{Title: &title}}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := st.withMutation(ctx, "rewrite history", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE issue_event_changes SET to_value = 'tampered' WHERE field = 'title' AND to_value = 'after'`)
		return err
	}); err != nil {
		t.Fatalf("rewrite history: %v", err)
	}
	listed, err := st.ListWrites(ctx, 1)
	if err != nil {
		t.Fatalf("ListWrites() error = %v", err)
	}
	if len(listed[0].Unrevertable) != 1 || !strings.Contains(listed[0].Unrevertable[0].What, "its title change rewritten") {
		t.Errorf("listed = %+v, want the rewritten change named", listed[0])
	}
}

// One rank write that re-keys several tickets is one thing no document can put
// back, counted, not a line per re-keyed ticket.
func TestListWritesNamesARankWriteOnce(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "epic", IssueType: "epic"})
	var tasks []string
	for _, title := range []string{"one", "two", "three"} {
		tasks = append(tasks, mustCreate(t, st, storage.CreateIssueInput{Title: title, ParentID: epic.ID}).ID)
	}
	// Reversed, the first and last swap keys and the middle keeps its own.
	if _, err := st.RankSet(ctx, []string{tasks[2], tasks[1], tasks[0]}); err != nil {
		t.Fatalf("RankSet() error = %v", err)
	}
	listed, err := st.ListWrites(ctx, 1)
	if err != nil {
		t.Fatalf("ListWrites() error = %v", err)
	}
	if len(listed) != 1 || len(listed[0].Unrevertable) != 1 || listed[0].Unrevertable[0].What != "ranks moved, tickets re-keyed: 2" {
		t.Fatalf("listed = %+v, want one write naming its two re-keyed ranks once", listed)
	}
}

// A slot a later write moved again is a conflict: reverting the first reparent
// alone would erase the second. Nothing is written.
func TestRevertWritesRefusesWhatALaterWriteMovedAgain(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	a := mustCreate(t, st, storage.CreateIssueInput{Title: "a", IssueType: "epic"})
	b := mustCreate(t, st, storage.CreateIssueInput{Title: "b", IssueType: "epic"})
	c := mustCreate(t, st, storage.CreateIssueInput{Title: "c", IssueType: "epic"})
	task := mustCreate(t, st, storage.CreateIssueInput{Title: "task", ParentID: a.ID})
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: task.ID, ParentID: b.ID}); err != nil {
		t.Fatalf("SetParent(b) error = %v", err)
	}
	first := headWrite(t, st)
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: task.ID, ParentID: c.ID}); err != nil {
		t.Fatalf("SetParent(c) error = %v", err)
	}
	head := headWrite(t, st)
	_, err := st.RevertWrites(ctx, []string{first}, "undoer")
	var conflict storage.ConflictError
	if !errors.As(err, &conflict) || len(conflict.Conflicts) != 1 {
		t.Fatalf("RevertWrites(first) = %v, want one conflict", err)
	}
	if headWrite(t, st) != head || !slices.Equal(parentsNow(t, st, task.ID), []string{c.ID}) {
		t.Errorf("a conflicted undo wrote: parent %v", parentsNow(t, st, task.ID))
	}
}

// An id the history does not hold is not found, and the refusal says why it
// may not be there.
func TestRevertWritesRefusesAWriteNotInHistory(t *testing.T) {
	st, _ := newUndoStore(t)
	_, err := st.RevertWrites(context.Background(), []string{strings.Repeat("a", 32)}, "undoer")
	var notFound storage.NotFoundError
	if !errors.As(err, &notFound) || !strings.Contains(err.Error(), "sync replay") {
		t.Fatalf("RevertWrites(unknown) = %v, want a not-found naming the window and the replay", err)
	}
}

// A write staged and never committed by an earlier process is committed by the
// batch before its own writes, so it is not among the undo's writes.
func TestRevertWritesLandedExcludesAStrandedWrite(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	a := mustCreate(t, st, storage.CreateIssueInput{Title: "a", IssueType: "epic"})
	b := mustCreate(t, st, storage.CreateIssueInput{Title: "b", IssueType: "epic"})
	task := mustCreate(t, st, storage.CreateIssueInput{Title: "task", ParentID: a.ID})
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: task.ID, ParentID: b.ID}); err != nil {
		t.Fatalf("SetParent() error = %v", err)
	}
	reparent := headWrite(t, st)
	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET lane = 'stranded' WHERE id = ?`, a.ID); err != nil {
		t.Fatalf("stage a write: %v", err)
	}
	result, err := st.RevertWrites(ctx, []string{reparent}, "undoer")
	if err != nil {
		t.Fatalf("RevertWrites() error = %v", err)
	}
	writes, err := st.ListWrites(ctx, 2+len(result.Landed))
	if err != nil {
		t.Fatalf("ListWrites() error = %v", err)
	}
	stranded := writes[len(result.Landed)]
	if stranded.ID == reparent || slices.Contains(result.Landed, stranded.ID) {
		t.Fatalf("the write below the undo's is %s; want the stranded write, not the reparent or one of the undo's %v", stranded.ID, result.Landed)
	}
	if len(stranded.Moves) != 1 || stranded.Moves[0].Slot.Name != "lane" {
		t.Errorf("the write below the undo's moved %v; want the stranded lane alone", stranded.Moves)
	}
}

// Listing writes reads history as of commits and takes no commit lock, so it
// serves under a held lock on a read-only open, and the store's earliest
// commits, which no document can put back, list rather than fail.
func TestListWritesTakesNoCommitLock(t *testing.T) {
	ctx := context.Background()
	st, doltRoot := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "epic", IssueType: "epic"})
	mustCreate(t, st, storage.CreateIssueInput{Title: "task", ParentID: epic.ID})
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	release, err := LockCommitPath(ctx, testStderr(), doltRoot)
	if err != nil {
		t.Fatalf("LockCommitPath() error = %v", err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release commit lock: %v", err)
		}
	}()
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reader, err := OpenForRead(readCtx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	defer reader.Close()
	writes, err := reader.ListWrites(readCtx, 100)
	if err != nil {
		t.Fatalf("ListWrites() under a held commit lock error = %v", err)
	}
	first := writes[len(writes)-1]
	if len(first.Unrevertable) != 1 || !strings.Contains(first.Unrevertable[0].What, "first commit") {
		t.Errorf("the store's first commit lists %v, want it unrevertable as the first commit", first.Unrevertable)
	}
	migration := slices.IndexFunc(writes, func(w storage.WriteSummary) bool { return strings.HasPrefix(w.Message, "migrate: ") })
	if migration < 0 || len(writes[migration].Unrevertable) != 1 || !strings.Contains(writes[migration].Unrevertable[0].What, "schema versions") {
		t.Errorf("a migration lists %+v, want it unrevertable as written across schema versions", writes[max(migration, 0)])
	}
}

// A row stored before the field rows normalized it reads alike on both sides
// of an edit of another field, so the edit stays revertable.
func TestListWritesTakesAnEditBesideAnUnnormalizedRowAsRevertable(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "epic", IssueType: "epic"})
	if err := st.withMutation(ctx, "an older title", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE issues SET title = 'epic  ' WHERE id = ?`, epic.ID)
		return err
	}); err != nil {
		t.Fatalf("store an unnormalized title: %v", err)
	}
	description := "now described"
	if _, err := st.Apply(ctx, epic.ID, storage.Change{Actor: "editor", Fields: storage.UpdateIssueInput{Description: &description}}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	listed, err := st.ListWrites(ctx, 1)
	if err != nil {
		t.Fatalf("ListWrites() error = %v", err)
	}
	if len(listed[0].Unrevertable) != 0 || len(listed[0].Moves) != 1 {
		t.Errorf("listed = %+v, want the one description move and nothing unrevertable", listed[0])
	}
}

// A rank move does not hide another column the same write moved.
func TestListWritesNamesARowMoveBesideARankMove(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "epic", IssueType: "epic"})
	if err := st.withMutation(ctx, "rank and topic", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE issues SET item_rank = CONCAT(item_rank, 'z'), topic = 'elsewhere' WHERE id = ?`, epic.ID)
		return err
	}); err != nil {
		t.Fatalf("move rank and topic: %v", err)
	}
	listed, err := st.ListWrites(ctx, 1)
	if err != nil {
		t.Fatalf("ListWrites() error = %v", err)
	}
	var what []string
	for _, u := range listed[0].Unrevertable {
		what = append(what, u.What)
	}
	if want := []string{epic.ID + ": its row moved outside its editable fields", "ranks moved, tickets re-keyed: 1"}; !slices.Equal(what, want) {
		t.Errorf("unrevertable = %q, want %q", what, want)
	}
}

// The observers hear an undo's changes once its commit-lock hold is released,
// as a lone batch's observers do: an observer that takes the lock gets it.
func TestRevertWritesIsHeardOnceItsLockIsReleased(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: "before", IssueType: "epic"})
	title := "after"
	if _, err := st.Apply(ctx, epic.ID, storage.Change{Actor: "editor", Fields: storage.UpdateIssueInput{Title: &title}}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	edit := headWrite(t, st)
	var heard []error
	st.ObserveChanges(func(storage.Change, model.Issue) {
		locked, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		heard = append(heard, st.withCommitLock(locked, func(context.Context) error { return nil }))
	})
	if _, err := st.RevertWrites(ctx, []string{edit}, "undoer"); err != nil {
		t.Fatalf("RevertWrites() error = %v", err)
	}
	if len(heard) != 1 || heard[0] != nil {
		t.Errorf("observer took the lock with %v, want one change heard with the lock free", heard)
	}
}
