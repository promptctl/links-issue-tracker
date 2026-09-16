package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func (h readyTestHarness) epic(title, parent string) model.Issue {
	h.t.Helper()
	return h.createIssue(storage.CreateIssueInput{Title: title, Topic: "dep", IssueType: "epic", ParentID: parent})
}

func (h readyTestHarness) task(title, parent string) model.Issue {
	h.t.Helper()
	return h.createIssue(storage.CreateIssueInput{Title: title, Topic: "dep", IssueType: "task", ParentID: parent})
}

// depAdd runs `lit dep add --type blocks --from from --to to`.
func (h readyTestHarness) depAdd(from, to string) error {
	var stdout bytes.Buffer
	return runAppFamily(depFamily, h.ctx, &stdout, h.ap, []string{"add", "--type", "blocks", "--from", from, "--to", to})
}

func (h readyTestHarness) mustDepAdd(from, to string) {
	h.t.Helper()
	if err := h.depAdd(from, to); err != nil {
		h.t.Fatalf("dep add --from %s --to %s error = %v", from, to, err)
	}
}

// parentSet runs `lit parent set --child child --parent parent`.
func (h readyTestHarness) parentSet(child, parent string) error {
	var stdout bytes.Buffer
	return runAppFamily(parentFamily, h.ctx, &stdout, h.ap, []string{"set", "--child", child, "--parent", parent})
}

// requireWaitLoopRefusal fails unless err is the validation refusal naming every
// step of the loop.
func requireWaitLoopRefusal(t *testing.T, err error, steps ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("edge closing a wait loop was accepted")
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

// Each edge below closes a loop through a link the store's blocks-only cycle
// check cannot see, so the issues in it would wait on each other forever. Each
// is refused at the edge that closes the loop, naming every link.
func TestDepAddRefusesAWaitLoop(t *testing.T) {
	t.Run("gate first", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		c := h.task("C", e.ID)
		g := h.task("G", "")
		h.mustDepAdd(g.ID, e.ID)
		requireWaitLoopRefusal(t, h.depAdd(c.ID, g.ID), c.ID+" depends on "+g.ID+" (via epic)", g.ID+" depends on "+c.ID)
	})
	t.Run("child edge first", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		c := h.task("C", e.ID)
		g := h.task("G", "")
		h.mustDepAdd(c.ID, g.ID)
		requireWaitLoopRefusal(t, h.depAdd(g.ID, e.ID), g.ID+" depends on "+c.ID, c.ID+" depends on "+g.ID+" (via epic)")
	})
	t.Run("two epics each blocked by a child of the other", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		c := h.task("C", e.ID)
		e2 := h.epic("E2", "")
		c2 := h.task("C2", e2.ID)
		h.mustDepAdd(c2.ID, e.ID)
		requireWaitLoopRefusal(t, h.depAdd(c.ID, e2.ID), c.ID+" depends on "+c2.ID+" (via epic)", c2.ID+" depends on "+c.ID+" (via epic)")
	})
	// An epic finishes only when its children do, so an issue that waits on
	// the epic cannot block one of them.
	t.Run("an epic blocks the issue that blocks its child", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		c := h.task("C", e.ID)
		g := h.task("G", "")
		h.mustDepAdd(e.ID, g.ID)
		requireWaitLoopRefusal(t, h.depAdd(g.ID, c.ID), g.ID+" depends on "+e.ID, "epic "+e.ID+" waits on its child "+c.ID, c.ID+" depends on "+g.ID)
	})
	// B waits on the nested epic E1 ahead of it in their lane, so B blocking E1
	// would hold back E1's child C, which E1 waits on.
	t.Run("a leaf blocks the nested epic ahead of it in its lane", func(t *testing.T) {
		h := newReadyTestHarness(t)
		outer := h.epic("E0", "")
		e1 := h.epic("E1", outer.ID)
		c := h.task("C", e1.ID)
		b := h.task("B", outer.ID)
		requireWaitLoopRefusal(t, h.depAdd(b.ID, e1.ID), b.ID+" waits on its earlier lane-mate "+e1.ID, "epic "+e1.ID+" waits on its child "+c.ID, c.ID+" depends on "+b.ID+" (via epic)")
	})
}

// Putting an issue under an epic makes it wait on the epic's blockers and makes
// the epic wait on it, so `lit parent set` closes the same loops.
func TestParentSetRefusesAWaitLoop(t *testing.T) {
	t.Run("the child already blocks the epic's blocker", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		g := h.task("G", "")
		f := h.task("F", "")
		h.mustDepAdd(g.ID, e.ID)
		h.mustDepAdd(f.ID, g.ID)
		requireWaitLoopRefusal(t, h.parentSet(f.ID, e.ID), f.ID+" depends on "+g.ID+" (via epic)", g.ID+" depends on "+f.ID)
	})
	t.Run("two epics under each other", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e1 := h.epic("E1", "")
		e2 := h.epic("E2", e1.ID)
		h.task("leaf", e2.ID)
		requireWaitLoopRefusal(t, h.parentSet(e1.ID, e2.ID), "epic "+e1.ID+" waits on its child "+e2.ID, "epic "+e2.ID+" waits on its child "+e1.ID)
	})
	// The loop runs through the moved epic's child D and never through the
	// moved epic X itself.
	t.Run("a moved epic's child already blocks the new blocker", func(t *testing.T) {
		h := newReadyTestHarness(t)
		p := h.epic("P", "")
		g := h.task("G", "")
		x := h.epic("X", "")
		d := h.task("D", x.ID)
		h.mustDepAdd(g.ID, p.ID)
		h.mustDepAdd(d.ID, g.ID)
		requireWaitLoopRefusal(t, h.parentSet(x.ID, p.ID), g.ID+" depends on "+d.ID, d.ID+" depends on "+g.ID+" (via epic)")
	})
}

// The check refuses only loops readiness would deadlock in and that the edge
// itself closes. A loop of blocks edges alone stays the store's refusal, in the
// store's words.
func TestWaitLoopCheckAcceptsEdgesThatCloseNoLoop(t *testing.T) {
	t.Run("an issue placed under a blocked epic", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		g := h.task("G", "")
		f := h.task("F", "")
		free := h.task("free", "")
		h.mustDepAdd(g.ID, e.ID)
		h.mustDepAdd(f.ID, g.ID)
		if err := h.parentSet(free.ID, e.ID); err != nil {
			t.Fatalf("parent set under a blocked epic error = %v", err)
		}
		// A parent that is not an epic holds nothing back: F blocks G, and G is
		// a task, so F under G is no loop.
		if err := h.parentSet(f.ID, g.ID); err != nil {
			t.Fatalf("parent set under a task error = %v", err)
		}
		err := h.depAdd(g.ID, f.ID)
		if err == nil || !strings.Contains(err.Error(), "close a dependency cycle") {
			t.Fatalf("a loop of blocks edges alone: error = %v, want the store's dependency-cycle refusal", err)
		}
	})
	// A closed issue waits on nothing and holds nothing back.
	t.Run("a loop through a closed child", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		c := h.task("C", e.ID)
		h.task("C2", e.ID)
		g := h.task("G", "")
		h.mustDepAdd(c.ID, g.ID)
		h.closeIssue(c.ID, "done")
		if err := h.depAdd(g.ID, e.ID); err != nil {
			t.Fatalf("dep add onto an epic whose closed child blocks the gate error = %v", err)
		}
	})
	// A blocker inside the epic it blocks holds nothing back, so moving the gate
	// into its epic closes no loop.
	t.Run("a gate moved into the epic it blocks", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		h.task("C", e.ID)
		g := h.task("G", "")
		h.mustDepAdd(g.ID, e.ID)
		if err := h.parentSet(g.ID, e.ID); err != nil {
			t.Fatalf("parent set of a gate into the epic it blocks error = %v", err)
		}
	})
	// B ahead of the nested epic E1 in their lane waits on nothing in E1.
	t.Run("a leaf blocks the nested epic behind it in its lane", func(t *testing.T) {
		h := newReadyTestHarness(t)
		outer := h.epic("E0", "")
		b := h.task("B", outer.ID)
		e1 := h.epic("E1", outer.ID)
		h.task("C", e1.ID)
		if err := h.depAdd(b.ID, e1.ID); err != nil {
			t.Fatalf("dep add onto the nested epic behind the blocker error = %v", err)
		}
	})
	// An epic's children never wait on the epic's lane-mates, so E1 standing
	// behind B in their lane holds nothing back: B waits on Z, Z on E1, E1 on
	// C, and C on nothing.
	t.Run("an epic behind a leaf in its lane", func(t *testing.T) {
		h := newReadyTestHarness(t)
		outer := h.epic("E0", "")
		b := h.task("B", outer.ID)
		e1 := h.epic("E1", outer.ID)
		h.task("C", e1.ID)
		z := h.task("Z", "")
		h.mustDepAdd(e1.ID, z.ID)
		if err := h.depAdd(z.ID, b.ID); err != nil {
			t.Fatalf("dep add behind an epic's lane-mate error = %v", err)
		}
	})
	// G is already in a loop the store holds (written past the CLI). An edge
	// that adds nothing to that loop is not refused for it.
	t.Run("an edge from an issue already in a loop", func(t *testing.T) {
		h := newReadyTestHarness(t)
		e := h.epic("E", "")
		c := h.task("C", e.ID)
		g := h.task("G", "")
		y := h.task("Y", "")
		h.addDependency(g.ID, e.ID)
		h.addDependency(c.ID, g.ID)
		if err := h.depAdd(g.ID, y.ID); err != nil {
			t.Fatalf("dep add from an issue already in a loop error = %v", err)
		}
	})
}
