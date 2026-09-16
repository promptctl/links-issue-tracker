package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// waitCycleWorkspace is epic E holding child C, with a gate G and a free issue F
// outside it.
type waitCycleWorkspace struct {
	ap         *app.App
	E, C, G, F string
}

func newWaitCycleWorkspace(t *testing.T) waitCycleWorkspace {
	t.Helper()
	ctx := context.Background()
	ap := newTestCLIApp(t)
	create := func(in storage.CreateIssueInput) string {
		t.Helper()
		in.Prefix, in.Topic = "test", "dep"
		issue, err := ap.Store.CreateIssue(ctx, in)
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", in.Title, err)
		}
		return issue.ID
	}
	w := waitCycleWorkspace{ap: ap}
	w.E = create(storage.CreateIssueInput{Title: "E", IssueType: "epic"})
	w.C = create(storage.CreateIssueInput{Title: "C", IssueType: "task", ParentID: w.E})
	w.G = create(storage.CreateIssueInput{Title: "G", IssueType: "task"})
	w.F = create(storage.CreateIssueInput{Title: "F", IssueType: "task"})
	return w
}

func (w waitCycleWorkspace) blocks(from, to string) error {
	var stdout bytes.Buffer
	return runAppFamily(depFamily, context.Background(), &stdout, w.ap, []string{"add", "--type", "blocks", "--from", from, "--to", to})
}

func (w waitCycleWorkspace) mustBlock(t *testing.T, from, to string) {
	t.Helper()
	if err := w.blocks(from, to); err != nil {
		t.Fatalf("dep add --from %s --to %s error = %v", from, to, err)
	}
}

// requireWaitCycleRefusal fails unless err is the validation refusal naming
// every step of the cycle.
func requireWaitCycleRefusal(t *testing.T, err error, steps ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("edge closing a wait cycle through an epic was accepted")
	}
	if code := ExitCode(err); code != ExitValidation {
		t.Fatalf("ExitCode(%v) = %d, want %d (ExitValidation)", err, code, ExitValidation)
	}
	for _, step := range steps {
		if !strings.Contains(err.Error(), step) {
			t.Fatalf("refusal %q does not name the step %q", err.Error(), step)
		}
	}
}

// A blocks edge onto an epic holds back every issue under it, so a gate that
// itself waits on one of those issues can never start, and neither can the
// issue. The store's cycle check sees blocks edges only, so it accepted both
// edges in either order. Each order is refused now, at the edge that closes the
// loop.
func TestDepAddRefusesAWaitCycleThroughAnEpic(t *testing.T) {
	t.Run("gate first", func(t *testing.T) {
		w := newWaitCycleWorkspace(t)
		w.mustBlock(t, w.G, w.E)
		requireWaitCycleRefusal(t, w.blocks(w.C, w.G), w.G+" blocks "+w.E, "epic "+w.E+" holds back "+w.C)
	})
	t.Run("child edge first", func(t *testing.T) {
		w := newWaitCycleWorkspace(t)
		w.mustBlock(t, w.C, w.G)
		requireWaitCycleRefusal(t, w.blocks(w.G, w.E), "epic "+w.E+" holds back "+w.C, w.C+" blocks "+w.G)
	})
	t.Run("two epics each blocked by a child of the other", func(t *testing.T) {
		w := newWaitCycleWorkspace(t)
		other, err := w.ap.Store.CreateIssue(context.Background(), storage.CreateIssueInput{Prefix: "test", Topic: "dep", Title: "E2", IssueType: "epic"})
		if err != nil {
			t.Fatalf("CreateIssue(E2) error = %v", err)
		}
		otherChild, err := w.ap.Store.CreateIssue(context.Background(), storage.CreateIssueInput{Prefix: "test", Topic: "dep", Title: "C2", IssueType: "task", ParentID: other.ID})
		if err != nil {
			t.Fatalf("CreateIssue(C2) error = %v", err)
		}
		w.mustBlock(t, otherChild.ID, w.E)
		requireWaitCycleRefusal(t, w.blocks(w.C, other.ID), "epic "+w.E+" holds back "+w.C)
	})
}

// Putting an issue under an epic makes it wait on the epic's blockers, so
// `lit parent set` closes the same loop when the issue is already one of them.
func TestParentSetRefusesAWaitCycleThroughAnEpic(t *testing.T) {
	w := newWaitCycleWorkspace(t)
	w.mustBlock(t, w.G, w.E)
	w.mustBlock(t, w.F, w.G)
	var stdout bytes.Buffer
	err := runAppFamily(parentFamily, context.Background(), &stdout, w.ap, []string{"set", "--child", w.F, "--parent", w.E})
	requireWaitCycleRefusal(t, err, w.F+" blocks "+w.G, w.G+" blocks "+w.E)
}

// The check refuses only loops through an epic's hold. A gate that waits on an
// issue outside the epic, and an issue placed under a blocked epic, are ordinary
// edges, and so is any edge under a parent that is not an epic. A loop of
// blocks edges alone stays the store's refusal, in the store's words.
func TestDepAddAcceptsEpicEdgesThatCloseNoLoop(t *testing.T) {
	w := newWaitCycleWorkspace(t)
	w.mustBlock(t, w.G, w.E)
	w.mustBlock(t, w.F, w.G)
	free, err := w.ap.Store.CreateIssue(context.Background(), storage.CreateIssueInput{Prefix: "test", Topic: "dep", Title: "free", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(free) error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runAppFamily(parentFamily, context.Background(), &stdout, w.ap, []string{"set", "--child", free.ID, "--parent", w.E}); err != nil {
		t.Fatalf("parent set under a blocked epic error = %v", err)
	}

	// A parent that is not an epic holds nothing back: F blocks G, and G is a
	// task, so F under G is no loop.
	if err := runAppFamily(parentFamily, context.Background(), &stdout, w.ap, []string{"set", "--child", w.F, "--parent", w.G}); err != nil {
		t.Fatalf("parent set under a task error = %v", err)
	}

	err = w.blocks(w.G, w.F)
	if err == nil || !strings.Contains(err.Error(), "close a dependency cycle") {
		t.Fatalf("a loop of blocks edges alone: error = %v, want the store's dependency-cycle refusal", err)
	}
}
