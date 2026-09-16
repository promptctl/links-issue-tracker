package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// `lit children <id>` is `lit ls --parent <id>`: for every flag combination the
// two surfaces print the same bytes. The cases are the ones an agent that learned
// ls reaches for first — the output shapers and the status set — plus the
// default, which is where two listings with two vocabularies used to disagree
// (links-children-flags-31xu). The fixture closes one child and nests a
// grandchild, so a surface that ignored --status, or listed descendants instead
// of direct children, prints a different row set than ls does.
// [LAW:behavior-not-structure] the assertion is the rendered output, not which
// leaf ran.
func TestChildrenIsLsWithAParent(t *testing.T) {
	f := newEpicFixture(t, "Listing epic", "# Why\nthe plan")
	first := f.addChild("First child")
	second := f.addChild("Second child")
	f.transition(first, model.Done{})
	grandchild, err := f.ap.Store.CreateIssue(f.ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Grandchild", Topic: "epic-view", IssueType: "task", ParentID: second,
	})
	if err != nil {
		t.Fatalf("CreateIssue(grandchild) error = %v", err)
	}

	for _, flags := range [][]string{
		nil,
		{"--format", "table", "--columns", "id,state,title"},
		{"--status", "open,closed"},
		{"--query", "status:closed", "--columns", "id,parent"},
	} {
		var children, ls bytes.Buffer
		if err := runChildren(f.ctx, &children, f.ap, append([]string{f.epicID}, flags...)); err != nil {
			t.Fatalf("children %s %v error = %v", f.epicID, flags, err)
		}
		if err := runListWithStore(f.ctx, &ls, f.ap.Store, workspaceReadyPolicy(f.ap), append([]string{"--parent", f.epicID}, flags...)); err != nil {
			t.Fatalf("ls --parent %s %v error = %v", f.epicID, flags, err)
		}
		if children.String() != ls.String() {
			t.Fatalf("children %v printed\n%s\nls --parent printed\n%s", flags, children.String(), ls.String())
		}
		if strings.Contains(children.String(), grandchild.ID) {
			t.Fatalf("children %v listed grandchild %s; want direct children only:\n%s", flags, grandchild.ID, children.String())
		}
	}

	// The flags reach the listing rather than being accepted and dropped: the
	// closed child is present only when the status set asks for it.
	closedOnly := runLs(t, f.ap, "--parent", f.epicID, "--status", "closed")
	if !strings.Contains(closedOnly, first) || strings.Contains(closedOnly, second) {
		t.Fatalf("ls --parent %s --status closed =\n%s\nwant only the closed child %s", f.epicID, closedOnly, first)
	}
}

// An unknown parent is refused on both surfaces rather than answered with the
// empty listing a childless parent gets; the two are different facts.
// [LAW:no-silent-failure]
func TestListingUnderAMissingParentIsNotFound(t *testing.T) {
	f := newEpicFixture(t, "Listing epic", "# Why\nthe plan")
	var out bytes.Buffer
	err := runChildren(f.ctx, &out, f.ap, []string{"test-nope"})
	var notFound storage.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("children test-nope error = %v, want storage.NotFoundError", err)
	}
	err = runListWithStore(f.ctx, &out, f.ap.Store, noReadyPolicy, []string{"--query", "parent:test-nope"})
	if !errors.As(err, &notFound) {
		t.Fatalf("ls --query parent:test-nope error = %v, want storage.NotFoundError", err)
	}
	if out.Len() != 0 {
		t.Fatalf("missing-parent listings emitted %q; want no output on the error path", out.String())
	}
}

// A listing given the wrong number of positionals is a usage error answered
// before any store opens, so none of these cases has a workspace to open. Too
// many is refused like too few: `children a b` listing only a's children, with
// exit 0, would be a wrong answer shaped like a right one. [LAW:no-silent-failure]
func TestListingRefusesTheWrongPositionalCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		surface listSurface
		args    []string
	}{
		{childrenSurface, nil},
		{childrenSurface, []string{"--format", "table"}},
		{childrenSurface, []string{"test-a", "test-b"}},
		{childrenSurface, []string{"test-a", "--status", "open", "test-b"}},
		{lsSurface, []string{"stray"}},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		err := runList(context.Background(), &out, tc.surface, tc.args)
		var usage UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "usage: lit "+tc.surface.name) {
			t.Fatalf("%s %v error = %#v, want a UsageError naming the usage line", tc.surface.name, tc.args, err)
		}
	}
}

// The parent id is found wherever it sits among the flags. A boolean flag takes
// no value, so `--include-archived <id>` must leave the id a positional, and `--`
// ends the flags without consuming it.
func TestChildrenFindsTheParentAmongFlags(t *testing.T) {
	f := newEpicFixture(t, "Listing epic", "# Why\nthe plan")
	child := f.addChild("Only child")
	for _, args := range [][]string{
		{"--include-archived", f.epicID},
		{"--has-comments=false", "--include-deleted", f.epicID, "--format", "table"},
		{"--", f.epicID},
	} {
		var out bytes.Buffer
		if err := runChildren(f.ctx, &out, f.ap, args); err != nil {
			t.Fatalf("children %v error = %v", args, err)
		}
		if !strings.Contains(out.String(), child) {
			t.Fatalf("children %v =\n%s\nwant the child %s", args, out.String(), child)
		}
	}
}

// An explicitly empty --parent names no parent; it is refused rather than
// dropped, because dropping it would widen the listing to every issue — or, next
// to a real id or under `children`, silently ignore part of the request. The
// refusal is the same on both surfaces. [LAW:no-silent-failure]
func TestListingRefusesAnEmptyParent(t *testing.T) {
	f := newEpicFixture(t, "Listing epic", "# Why\nthe plan")
	cases := []struct {
		surface listSurface
		args    []string
	}{
		{lsSurface, []string{"--parent="}},
		{lsSurface, []string{"--parent", " , "}},
		{lsSurface, []string{"--parent", f.epicID, "--parent="}},
		{childrenSurface, []string{f.epicID, "--parent="}},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		err := runListLeaf(f.ctx, &out, tc.surface, listScope{store: f.ap.Store, policy: noReadyPolicy}, tc.args)
		var usage UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "--parent needs an issue id") {
			t.Fatalf("%s %v error = %#v, want UsageError naming the empty --parent", tc.surface.name, tc.args, err)
		}
		if out.Len() != 0 {
			t.Fatalf("%s %v emitted %q; want no output on the error path", tc.surface.name, tc.args, out.String())
		}
	}
}

// A repeated --parent widens the parent set like a repeated --status widens the
// status set; the second occurrence never replaces the first.
func TestLsRepeatedParentIsAUnion(t *testing.T) {
	f := newEpicFixture(t, "Listing epic", "# Why\nthe plan")
	first := f.addChild("First child")
	second := f.addChild("Second child")
	grandchild, err := f.ap.Store.CreateIssue(f.ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Grandchild", Topic: "epic-view", IssueType: "task", ParentID: second,
	})
	if err != nil {
		t.Fatalf("CreateIssue(grandchild) error = %v", err)
	}
	got := runLs(t, f.ap, "--parent", f.epicID, "--parent", second)
	for _, id := range []string{first, second, grandchild.ID} {
		if !strings.Contains(got, id) {
			t.Fatalf("ls --parent %s --parent %s =\n%s\nwant %s listed", f.epicID, second, got, id)
		}
	}
}

// `lit children --help` advertises the ls flag surface, so the flags an agent
// learned on ls are discoverable where it expects them.
func TestChildrenHelpListsTheLsFlags(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runList(context.Background(), &out, childrenSurface, []string{"--help"}); !errors.Is(err, errHelpHandled) {
		t.Fatalf("children --help error = %v, want errHelpHandled", err)
	}
	for _, flag := range []string{"--format", "--columns", "--status", "--sort", "--parent", "--query"} {
		if !strings.Contains(out.String(), flag) {
			t.Fatalf("children --help omits %s:\n%s", flag, out.String())
		}
	}
}
