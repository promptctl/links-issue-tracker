package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
)

// childrenOutput runs `lit children <id>` and returns the captured stdout — the
// listing surface, alongside showOutput's detail surface.
func childrenOutput(t *testing.T, ap *app.App, id string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := runChildren(context.Background(), &buf, ap, []string{id}); err != nil {
		t.Fatalf("runChildren(%s) error = %v", id, err)
	}
	return buf.String()
}

// Epic membership has exactly one signal — the parent field — and a ticket
// reparented out of an epic is a member of no epic on every surface that
// reports membership: the children listing, the epic's own progress count and
// children block, and the reparented ticket's detail view.
//
// The trap this pins is a second signal: a child's id is minted under its
// parent's, so `<epic>.2` still *reads* like a member long after the parent
// edge is gone. A surface that derives membership from the id prefix — or from
// any edge other than parent-child — agrees with the parent field until the day
// someone reparents, and then every downstream number (children counts, N/M
// done, release gating) lies. Asserting the prefix survives the reparent keeps
// that premise live: without it this test would still pass against an id scheme
// that had stopped tempting anyone.
// [LAW:one-source-of-truth] Three surfaces, one authority.
func TestReparentedChildLeavesTheEpicOnEverySurface(t *testing.T) {
	f := newEpicFixture(t, "Membership epic", "# Why\nthe plan")
	stays := f.addChild("Child stays")
	leaves := f.addChild("Child leaves")

	if !strings.HasPrefix(leaves, f.epicID+".") {
		t.Fatalf("premise gone: child id %q no longer carries the epic prefix %q, so this test no longer pins prefix-derived membership", leaves, f.epicID)
	}

	if err := f.ap.Store.ClearParent(f.ctx, leaves); err != nil {
		t.Fatalf("ClearParent(%s) error = %v", leaves, err)
	}

	epicView := showOutput(t, f.ap, f.epicID)

	// The two surfaces that enumerate an epic's members answer the same
	// question and so take the same assertion; only the rendering differs.
	// [LAW:dataflow-not-control-flow]
	for _, surface := range []struct {
		name string
		out  string
	}{
		{"lit children <epic>", childrenOutput(t, f.ap, f.epicID)},
		{"lit show <epic>", epicView},
	} {
		if strings.Contains(surface.out, leaves) {
			t.Errorf("%s lists %s, which was reparented out of the epic:\n%s", surface.name, leaves, surface.out)
		}
		if !strings.Contains(surface.out, stays) {
			t.Errorf("%s dropped %s, which is still a child:\n%s", surface.name, stays, surface.out)
		}
	}

	// The progress count is the same membership question asked as a number, so
	// it moves with the listing rather than trailing it.
	if want := "children: 0 closed, 0 in_progress, 1 open (1 total)"; !strings.Contains(epicView, want) {
		t.Errorf("epic progress should count only the remaining child, want %q in:\n%s", want, epicView)
	}

	// The reparented ticket's own view carries no epic: no parent block, and no
	// plan slice appended. Its id still spells the old epic, so the assertion
	// names the markers rather than the epic id.
	out := showOutput(t, f.ap, leaves)
	if strings.Contains(out, "\nparent:\n") {
		t.Errorf("show %s prints a parent block after the reparent:\n%s", leaves, out)
	}
	if strings.Contains(out, "Epic: ") {
		t.Errorf("show %s prints an epic header after the reparent:\n%s", leaves, out)
	}
}
