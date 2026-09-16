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

// A children listing with no parent id is a usage error answered before any
// store opens, so none of these cases has a workspace to open.
func TestChildrenRefusesAMissingPositional(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"--format", "table"}} {
		var out bytes.Buffer
		err := runList(context.Background(), &out, childrenSurface, args)
		var usage UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "usage: lit children <parent-id>") {
			t.Fatalf("children %v error = %#v, want UsageError naming the usage line", args, err)
		}
	}
}

// An explicitly empty --parent names no parent; it is refused rather than
// dropped, because dropping it would widen the listing to every issue.
// [LAW:no-silent-failure]
func TestLsRefusesAnEmptyParent(t *testing.T) {
	f := newEpicFixture(t, "Listing epic", "# Why\nthe plan")
	for _, args := range [][]string{{"--parent="}, {"--parent", " , "}} {
		var out bytes.Buffer
		err := runListWithStore(f.ctx, &out, f.ap.Store, noReadyPolicy, args)
		var usage UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "--parent needs an issue id") {
			t.Fatalf("ls %v error = %#v, want UsageError naming the empty --parent", args, err)
		}
		if out.Len() != 0 {
			t.Fatalf("ls %v emitted %q; want no output on the error path", args, out.String())
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
