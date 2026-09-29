package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// doctorWaitLoopLine runs lit doctor and returns its output and its one
// "wait loop:" line, failing unless the status line counts exactly one loop.
func (h readyTestHarness) doctorWaitLoopLine() (string, string) {
	h.t.Helper()
	var buf bytes.Buffer
	if err := runDoctor(h.ctx, &buf, h.ap, nil); err != nil {
		h.t.Fatalf("runDoctor() error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, " wait_loops=1\n") {
		h.t.Fatalf("doctor should count one wait loop on its status line; got:\n%s", got)
	}
	var line string
	for l := range strings.Lines(got) {
		if strings.HasPrefix(l, "wait loop: ") {
			line = l
		}
	}
	return got, line
}

// A leaf B ranked behind a nested epic E1 in their lane, and E1's child C
// depending on B: B waits on E1, E1 on C, C on B. lit next can start none of
// them, and lit doctor names the loop by each of its three links.
func TestDoctorNamesAWaitLoop(t *testing.T) {
	h := newReadyTestHarness(t)
	var buf bytes.Buffer
	if err := runDoctor(h.ctx, &buf, h.ap, nil); err != nil {
		t.Fatalf("runDoctor() on an empty workspace error = %v", err)
	}
	if !strings.Contains(buf.String(), " wait_loops=0\n") {
		t.Fatalf("doctor on an empty workspace should report wait_loops=0; got:\n%s", buf.String())
	}

	e0 := h.createIssue(storage.CreateIssueInput{Title: "E0", Topic: "loop", IssueType: "epic"})
	e1 := h.createIssue(storage.CreateIssueInput{Title: "E1", Topic: "loop", IssueType: "epic", ParentID: e0.ID})
	c := h.createIssue(storage.CreateIssueInput{Title: "C", Topic: "loop", IssueType: "task", ParentID: e1.ID})
	b := h.createIssue(storage.CreateIssueInput{Title: "B", Topic: "loop", IssueType: "task", ParentID: e0.ID})
	h.addDependency(c.ID, b.ID)

	got, line := h.doctorWaitLoopLine()
	for _, link := range []string{
		b.ID + " waits on earlier sibling " + e1.ID,
		e1.ID + " waits on its child " + c.ID,
		c.ID + " depends on " + b.ID,
	} {
		if !strings.Contains(line, link) {
			t.Fatalf("the wait loop line should name %q; got:\n%s", link, got)
		}
	}
}

// C stands behind B in their lane and also depends on B, so two links run from
// C to B. Removing either alone leaves C waiting on B, so the loop names both.
// The loop runs back through epic F's child, since the store refuses a loop
// of blocks edges alone.
func TestDoctorNamesEveryLinkBetweenAPair(t *testing.T) {
	h := newReadyTestHarness(t)
	e := h.createIssue(storage.CreateIssueInput{Title: "E", Topic: "loop", IssueType: "epic"})
	b := h.createIssue(storage.CreateIssueInput{Title: "B", Topic: "loop", IssueType: "task", ParentID: e.ID})
	c := h.createIssue(storage.CreateIssueInput{Title: "C", Topic: "loop", IssueType: "task", ParentID: e.ID})
	f := h.createIssue(storage.CreateIssueInput{Title: "F", Topic: "loop", IssueType: "epic"})
	x := h.createIssue(storage.CreateIssueInput{Title: "X", Topic: "loop", IssueType: "task", ParentID: f.ID})
	h.addDependency(c.ID, b.ID)
	h.addDependency(b.ID, f.ID)
	h.addDependency(x.ID, c.ID)

	got, line := h.doctorWaitLoopLine()
	for _, link := range []string{
		c.ID + " depends on " + b.ID + " and waits on earlier sibling " + b.ID,
		b.ID + " depends on " + f.ID,
		f.ID + " waits on its child " + x.ID,
		x.ID + " depends on " + c.ID,
	} {
		if !strings.Contains(line, link) {
			t.Fatalf("the wait loop line should name %q; got:\n%s", link, got)
		}
	}
}

// A hierarchy that loops has no walk over it that returns, so doctor reports
// the wait-loop check as not run instead of overflowing the stack on it.
func TestDoctorWaitLoopsUncheckedOnAStoredParentCycle(t *testing.T) {
	h := newReadyTestHarness(t)
	top := h.createIssue(storage.CreateIssueInput{Title: "E", Topic: "loop", IssueType: "epic"})
	sub := h.createIssue(storage.CreateIssueInput{Title: "S", Topic: "loop", IssueType: "epic", ParentID: top.ID})
	raw, err := storage.TestSupport.Of(h.ap.Store)
	if err != nil {
		t.Fatalf("TestSupport: %v", err)
	}
	// The write boundary refuses this edge, so it is planted the way older data
	// or a restore could have left it: top becomes a child of its own child.
	if err := raw.ExecRawForTest(h.ctx,
		`INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, 'import')`,
		top.ID, sub.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("plant parent cycle: %v", err)
	}
	var buf bytes.Buffer
	err = runDoctor(h.ctx, &buf, h.ap, nil)
	if !errors.As(err, new(CorruptionError)) {
		t.Fatalf("runDoctor() error = %v, want the parent cycle's CorruptionError", err)
	}
	if !strings.Contains(buf.String(), " wait_loops=unchecked\n") {
		t.Fatalf("doctor should report wait_loops=unchecked on a parent cycle; got:\n%s", buf.String())
	}
}

// loopsIn names every issue on a loop, each loop by its own hops alone and
// never a tail leading into it or a branch leading out: a is on a>b>a, and c,
// which that loop misses, gets c>a>b>c; nothing names a loop twice.
func TestLoopsInNamesEveryIssueOnALoop(t *testing.T) {
	graph := map[string][]waitHop{}
	for _, pair := range [][2]string{
		{"a", "b"}, {"b", "a"}, {"b", "c"}, {"c", "a"},
		{"tail", "a"}, {"a", "out"},
		{"y", "x"}, {"x", "y"},
		{"lone", "x"},
	} {
		hop := waitHop{waiter: pair[0], prereq: pair[1], kinds: []waitKind{waitDependency}}
		graph[hop.waiter] = append(graph[hop.waiter], hop)
	}
	var got []string
	for _, loop := range loopsIn(graph) {
		var hops []string
		for _, hop := range loop {
			hops = append(hops, hop.waiter+">"+hop.prereq)
		}
		got = append(got, strings.Join(hops, " "))
	}
	want := []string{"a>b b>a", "c>a a>b b>c", "x>y y>x"}
	if !slices.Equal(got, want) {
		t.Fatalf("loopsIn() = %q, want %q", got, want)
	}
}
