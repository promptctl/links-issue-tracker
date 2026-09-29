package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

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

	buf.Reset()
	if err := runDoctor(h.ctx, &buf, h.ap, nil); err != nil {
		t.Fatalf("runDoctor() error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, " wait_loops=1\n") {
		t.Fatalf("doctor should count one wait loop on its status line; got:\n%s", got)
	}
	var line string
	for l := range strings.Lines(got) {
		if strings.HasPrefix(l, "wait loop: ") {
			line = l
		}
	}
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

// A hierarchy that loops has no walk over it that returns, so the wait-loop
// check is reported as not run rather than run and clean.
func TestDoctorWaitLoopsUncheckedOnAParentCycle(t *testing.T) {
	h := newReadyTestHarness(t)
	count, loops, err := doctorWaitLoops(h.ctx, h.ap.Store, storage.HealthReport{ParentCycle: []string{"E", "S"}})
	if err != nil {
		t.Fatalf("doctorWaitLoops() error = %v", err)
	}
	if count != "unchecked" || loops != nil {
		t.Fatalf("doctorWaitLoops() on a parent cycle = %q, %v; want unchecked and no loops", count, loops)
	}
}

// loopsIn names every issue on a loop, each loop by its own links alone and
// never a tail leading into it or a branch leading out: a is on a>b>a, and c,
// which that loop misses, gets c>a>b>c; nothing names a loop twice.
func TestLoopsInNamesEveryIssueOnALoop(t *testing.T) {
	dep := func(waiter, prereq string) waitLink {
		return waitLink{waiter: waiter, prereq: prereq, holds: true, kind: waitDependency}
	}
	graph := map[string][]waitLink{}
	for _, link := range []waitLink{
		dep("a", "b"), dep("b", "c"), dep("c", "a"), dep("b", "a"),
		dep("tail", "a"), dep("a", "out"),
		dep("y", "x"), dep("x", "y"),
		dep("lone", "x"),
	} {
		graph[link.waiter] = append(graph[link.waiter], link)
	}
	var got []string
	for _, loop := range loopsIn(graph) {
		var hops []string
		for _, link := range loop {
			hops = append(hops, link.waiter+">"+link.prereq)
		}
		got = append(got, strings.Join(hops, " "))
	}
	want := []string{"a>b b>a", "c>a a>b b>c", "x>y y>x"}
	if !slices.Equal(got, want) {
		t.Fatalf("loopsIn() = %q, want %q", got, want)
	}
}
