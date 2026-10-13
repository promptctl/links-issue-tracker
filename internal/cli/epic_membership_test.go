package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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
// reparented out of an epic into another is a member of only the new one on
// every surface that reports membership: the old epic's children listing, its
// progress count and children block, and the reparented ticket's detail view.
//
// The trap this pins is a second signal: a child's id is minted under its
// parent's, so `<epic>.2` still *reads* like a member long after the parent
// edge has moved. A surface that derives membership from the id prefix — or
// from any edge other than parent-child — agrees with the parent field until
// the day someone reparents, and then every downstream number (children
// counts, N/M done, release gating) lies. Asserting the prefix survives the
// reparent keeps that premise live: without it this test would still pass
// against an id scheme that had stopped tempting anyone.
// [LAW:one-source-of-truth] Three surfaces, one authority.
func TestReparentedChildLeavesTheEpicOnEverySurface(t *testing.T) {
	f := newEpicFixture(t, "Membership epic", "# Why\nthe plan")
	stays := f.addChild("Child stays")
	leaves := f.addChild("Child leaves")
	other := seedEpicRaw(t, f.ctx, f.ap, "Other epic", "other")

	if !strings.HasPrefix(leaves, f.epicID+".") {
		t.Fatalf("premise gone: child id %q no longer carries the epic prefix %q, so this test no longer pins prefix-derived membership", leaves, f.epicID)
	}

	// A leaf lives inside an epic, so leaving one is arriving in another: the
	// move is one `lit parent set`, never a detach.
	if _, err := f.ap.Store.SetParent(f.ctx, storage.SetParentInput{ChildID: leaves, ParentID: other, CreatedBy: "test"}); err != nil {
		t.Fatalf("SetParent(%s -> %s) error = %v", leaves, other, err)
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

	// The reparented ticket's own view carries the new epic and only it: the
	// parent block and the plan slice both name the epic it moved to. Its id
	// still spells the old epic, so the old epic's absence is asserted on the
	// header that names an epic by id, where the old id would stand alone.
	out := showOutput(t, f.ap, leaves)
	if !strings.Contains(out, "\nparent:\n- "+other) {
		t.Errorf("show %s does not name %s as its parent after the reparent:\n%s", leaves, other, out)
	}
	if !strings.Contains(out, "Epic: "+other+" ") {
		t.Errorf("show %s does not render the epic header for %s after the reparent:\n%s", leaves, other, out)
	}
	if strings.Contains(out, "Epic: "+f.epicID+" ") {
		t.Errorf("show %s still renders the old epic's header after the reparent:\n%s", leaves, out)
	}
}
