package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestRunHistoryPrintsMultiEditTransitionTrail exercises `lit history` on a
// ticket edited several times through the real CLI update path. The trail must
// carry every field-level `from → to` transition those edits produced — the
// full history `lit show` deliberately does not render. This is a behavioral
// test [LAW:behavior-not-structure]: it drives genuine edits and asserts on the
// transitions a reader sees, not on how the events are stored.
func TestRunHistoryPrintsMultiEditTransitionTrail(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Original title", Topic: "history", IssueType: "task", Priority: 0,
		ParentID: seedEpicRaw(t, ctx, ap, "History", "history"),
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	// Three real edits: two title renames plus a priority bump, so the trail has
	// several distinct field transitions to render.
	edits := [][]string{
		{issue.ID, "--title", "Renamed once", "--by", "editor"},
		{issue.ID, "--title", "Renamed twice", "--by", "editor"},
		{issue.ID, "--priority", "1", "--by", "editor"},
	}
	for _, args := range edits {
		if err := runUpdate(ctx, &bytes.Buffer{}, ap, args); err != nil {
			t.Fatalf("runUpdate(%v) error = %v", args, err)
		}
	}

	var stdout bytes.Buffer
	if err := runHistory(ctx, &stdout, ap, []string{issue.ID}); err != nil {
		t.Fatalf("runHistory() error = %v", err)
	}
	out := stdout.String()

	// Header identifies the ticket by id + final title, then the labelled trail.
	for _, want := range []string{issue.ID, "Renamed twice", "\nhistory:\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lit history output missing %q in:\n%s", want, out)
		}
	}

	// Every edit's field transition appears as a `field: from → to` line.
	wantTransitions := []string{
		"title: Original title → Renamed once",
		"title: Renamed once → Renamed twice",
		"priority: 0 → 1",
	}
	for _, want := range wantTransitions {
		if !strings.Contains(out, want) {
			t.Fatalf("lit history output missing transition %q in:\n%s", want, out)
		}
	}
	// No long text field changed, so the trail is the whole output: it ends at the
	// last change it records.
	if !strings.HasSuffix(out, "    priority: 0 → 1\n") {
		t.Fatalf("lit history output does not end at its last transition:\n%s", out)
	}
}

// seedProseHistory creates a ticket and edits its long text fields through the
// real update path: the description three times (so it holds versions 1–3,
// one of them multi-line), the prompt once, and the title once beside them.
func seedProseHistory(t *testing.T, ctx context.Context, ap *app.App) string {
	t.Helper()
	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Original title", Description: "first draft", Prompt: "old prompt",
		Topic: "history", IssueType: "task", Priority: 0,
		ParentID: seedEpicRaw(t, ctx, ap, "History", "history"),
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	edits := [][]string{
		{issue.ID, "--description", "second draft\nline two", "--by", "editor"},
		{issue.ID, "--description", "third draft", "--title", "Renamed", "--by", "editor"},
		{issue.ID, "--prompt", "new prompt", "--by", "editor"},
	}
	for _, args := range edits {
		if err := runUpdate(ctx, &bytes.Buffer{}, ap, args); err != nil {
			t.Fatalf("runUpdate(%v) error = %v", args, err)
		}
	}
	return issue.ID
}

// TestRunHistoryShowsLongTextChangesAsVersions is the trail an agent reads: a
// change to a long text field is a step between numbered versions, and no
// superseded text — nor the current one — is printed in it. A short field
// beside it keeps its from → to line. [LAW:behavior-not-structure]
func TestRunHistoryShowsLongTextChangesAsVersions(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedProseHistory(t, ctx, ap)

	var stdout bytes.Buffer
	if err := runHistory(ctx, &stdout, ap, []string{id}); err != nil {
		t.Fatalf("runHistory() error = %v", err)
	}
	out := stdout.String()

	for _, want := range []string{
		"    description: v1 → v2\n",
		"    description: v2 → v3\n",
		"    prompt: v1 → v2\n",
		"    title: Original title → Renamed\n",
		"\nversions:\n    description: 3 versions — lit history " + id + " --version description@<n> prints one\n" +
			"    prompt: 2 versions — lit history " + id + " --version prompt@<n> prints one\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("lit history output missing %q in:\n%s", want, out)
		}
	}
	for _, text := range []string{"first draft", "second draft", "line two", "third draft", "old prompt", "new prompt"} {
		if strings.Contains(out, text) {
			t.Fatalf("lit history printed long text %q in its trail:\n%s", text, out)
		}
	}
}

// TestRunHistoryVersionPrintsTheTextThatVersionHeld: every version the trail
// numbers is retrievable by that number, bare, so it round-trips into
// `lit update --description`. Version n+1 is what the n-th change wrote.
func TestRunHistoryVersionPrintsTheTextThatVersionHeld(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedProseHistory(t, ctx, ap)

	for ref, want := range map[string]string{
		"description@1": "first draft\n",
		"description@2": "second draft\nline two\n",
		"description@3": "third draft\n",
		"prompt@1":      "old prompt\n",
		"prompt@2":      "new prompt\n",
	} {
		var stdout bytes.Buffer
		if err := runHistory(ctx, &stdout, ap, []string{id, "--version", ref}); err != nil {
			t.Fatalf("runHistory(--version %s) error = %v", ref, err)
		}
		if got := stdout.String(); got != want {
			t.Fatalf("runHistory(--version %s) = %q, want %q", ref, got, want)
		}
	}
}

// TestRunHistoryVersionPastTheRecordIsNotFound: a field never changed holds
// one version, its current text, and a version past the ones a field holds is
// a lookup miss (exit 4) that says how many it does hold.
func TestRunHistoryVersionPastTheRecordIsNotFound(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedProseHistory(t, ctx, ap)
	untouched, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Never edited", Description: "only draft", Topic: "history", IssueType: "task",
		ParentID: seedEpicRaw(t, ctx, ap, "Untouched", "history"),
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runHistory(ctx, &stdout, ap, []string{untouched.ID, "--version", "description@1"}); err != nil || stdout.String() != "only draft\n" {
		t.Fatalf("runHistory(%s --version description@1) = %q, %v; want the current text", untouched.ID, stdout.String(), err)
	}
	for _, tc := range []struct{ id, ref, want string }{
		{id, "description@4", "holds 3 versions of description"},
		{untouched.ID, "description@2", "holds 1 version of description"},
		{untouched.ID, "prompt@1", "holds 0 versions of prompt"},
	} {
		err := runHistory(ctx, &bytes.Buffer{}, ap, []string{tc.id, "--version", tc.ref})
		if ExitCode(err) != ExitNotFound || commandErrorReason(err) != reasonVersionNotFound || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("runHistory(%s --version %s) = exit %d %v, want exit %d naming %q", tc.id, tc.ref, ExitCode(err), err, ExitNotFound, tc.want)
		}
	}
}

// TestHistoryNumbersEachTextOnce covers the trails a single machine's edits
// never produce: sync merges two machines' edits from one text as a union, a
// change can go back to an earlier text or empty the field, and a write can
// change the text without recording a change (a reconciled merge, a prompt
// edited before prompt changes were recorded). Each text has one number, so each
// of those reads true: both merged edits step from the text they started
// from, the revert returns to its number, the emptied field shows -, and the
// unrecorded current text is still a version --version prints.
func TestHistoryNumbersEachTextOnce(t *testing.T) {
	change := func(from, to string) model.IssueEvent {
		return model.IssueEvent{Actor: "editor", Reason: "edit", Changes: []model.FieldChange{{Field: "description", From: from, To: to}}}
	}
	detail := model.IssueDetail{
		Issue: model.Issue{ID: "test-1", Title: "Merged", Description: "D", Prompt: "P"},
		Events: []model.IssueEvent{
			change("", "A"),
			change("A", "B"), // this machine's edit
			change("A", "C"), // the other machine's, merged in by sync
			change("C", "A"),
			change("A", ""),
		},
	}

	var stdout bytes.Buffer
	if err := printIssueHistory(&stdout, detail); err != nil {
		t.Fatalf("printIssueHistory() error = %v", err)
	}
	var steps []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "    description: v") || strings.HasPrefix(line, "    description: -") {
			steps = append(steps, strings.TrimPrefix(line, "    description: "))
		}
	}
	if want := []string{"- → v1", "v1 → v2", "v1 → v3", "v3 → v1", "v1 → -"}; !slices.Equal(steps, want) {
		t.Fatalf("trail steps = %q, want %q in:\n%s", steps, want, stdout.String())
	}
	// The prompt never held another text, so the versions block leaves it out.
	if want := "\nversions:\n    description: 4 versions — lit history test-1 --version description@<n> prints one\n"; !strings.HasSuffix(stdout.String(), want) {
		t.Fatalf("versions block: want suffix %q in:\n%s", want, stdout.String())
	}

	for ref, want := range map[string]string{"description@1": "A\n", "description@3": "C\n", "description@4": "D\n", "prompt@1": "P\n"} {
		parsed, err := parseVersionRef(ref)
		if err != nil {
			t.Fatalf("parseVersionRef(%s) error = %v", ref, err)
		}
		var out bytes.Buffer
		if err := printFieldVersion(&out, detail, parsed); err != nil || out.String() != want {
			t.Fatalf("printFieldVersion(%s) = %q, %v; want %q", ref, out.String(), err, want)
		}
	}
}

// TestHistoryHoldsEachEventToItsRows pins the view's shape against the text
// an issue and its events carry: the id and title each one row, each event one
// row and each change one row beneath it. Every value a row prints holds one of
// the break forms a reader may take for a new line, or an ESC that a terminal
// can turn into one, and the first is a title shaped like an event row, which
// printed at column 0 would read as one. A value that already holds the break
// token prints it defused, so the token on a row means a break.
// [LAW:behavior-not-structure]
func TestHistoryHoldsEachEventToItsRows(t *testing.T) {
	t.Parallel()
	detail := model.IssueDetail{
		Issue: model.Issue{ID: "test\x1fb-1", Title: "T\nhistory:"},
		Events: []model.IssueEvent{
			{Actor: "alice", Action: "close\vx", Reason: "why\nsecond\fthird", Changes: []model.FieldChange{
				{Field: "title", From: "first‹newline›", To: "line one\n- [forged @ now] update"},
			}},
			{Actor: "bob\r\neve", Changes: []model.FieldChange{
				{Field: "lane\u0085z", From: "a\rb", To: "c\u2028d\u2029e\x1bE- [forged @ now] close"},
			}},
		},
	}

	var stdout bytes.Buffer
	if err := printIssueHistory(&stdout, detail); err != nil {
		t.Fatalf("printIssueHistory() error = %v", err)
	}
	stamp, br := formatHistoryTimestamp(time.Time{}), quotedBreak
	want := "test\uFFFDb-1\nT" + br + "history:\n\nhistory:\n" +
		"- [alice @ " + stamp + "] close" + br + "x why" + br + "second" + br + "third\n" +
		"    title: first" + defusedBreak + " → line one" + br + "- [forged @ now] update\n" +
		"- [bob" + br + "eve @ " + stamp + "] update \n" +
		"    lane" + br + "z: a" + br + "b → c" + br + "d" + br + "e\uFFFDE- [forged @ now] close\n"
	if got := stdout.String(); got != want {
		t.Fatalf("history view:\n got %q\nwant %q", got, want)
	}
}

// TestRunHistoryVersionRejectsAMalformedReference: --version names a long
// text field and a version counted from 1, written as plain decimal digits,
// and anything else is a usage error —
// decided before the id is looked up, so it holds for an id that names nothing.
func TestRunHistoryVersionRejectsAMalformedReference(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedProseHistory(t, ctx, ap)

	for _, tc := range []struct{ id, ref string }{
		{id, "title@1"},
		{id, "description"},
		{id, "description@0"},
		{id, "description@x"},
		{id, "description@+1"},
		{id, "description@01"},
		{id, "@1"},
		{id, ""},
		{"test-missing", "status@1"},
	} {
		err := runHistory(ctx, &bytes.Buffer{}, ap, []string{tc.id, "--version", tc.ref})
		if ExitCode(err) != ExitUsage {
			t.Fatalf("runHistory(%s --version %q) = exit %d %v, want exit %d", tc.id, tc.ref, ExitCode(err), err, ExitUsage)
		}
	}
}
