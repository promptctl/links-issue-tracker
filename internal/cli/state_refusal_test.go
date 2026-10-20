package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A blank comment or comment id, a rank naming a deleted issue or an impossible
// frame, a rank set that would reverse a shared key, a retention move the
// table has no cell for, and an edge the graph already holds, a parent edge
// included, are each answered the same way on every run, so none may reach the
// "retry, then run lit doctor" default. A refusal exits ExitValidation with
// validation_refused; a retention move whose state already holds, and an edge
// already in place, exit ExitNoWork with state_already_holds, since the
// command ran correctly and had nothing to do.
//
// Each case drives the real command handler and asserts the exact reason and
// exit code, as TestFieldRefusalsAreDeterministicRefusals does.
func TestDeterministicAnswersAreNeverRetried(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	create := func(title string, issueType model.IssueType, parent string) string {
		t.Helper()
		issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "refusals", IssueType: issueType, ParentID: parent})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		return issue.ID
	}
	epic := create("an epic", "epic", "")
	live := create("live", "task", epic)
	archived := create("archived", "task", epic)
	gone := create("deleted", "task", epic)
	if err := runTransition(ctx, io.Discard, ap, []string{archived}, archiveSpec); err != nil {
		t.Fatalf("archive %s: %v", archived, err)
	}
	if err := runTransition(ctx, io.Discard, ap, []string{gone}, deleteSpec); err != nil {
		t.Fatalf("delete %s: %v", gone, err)
	}
	// Two siblings sharing a key, a state the contract cannot express, planted
	// through the test-support capability. Writing the later id above the
	// earlier one would reverse the order a listing gives them.
	tiedLow, tiedHigh := create("tied", "task", epic), create("tied", "task", epic)
	if tiedHigh < tiedLow {
		tiedLow, tiedHigh = tiedHigh, tiedLow
	}
	raw, err := storage.TestSupport.Of(ap.Store)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	if err := raw.ExecRawForTest(ctx, "UPDATE issues SET item_rank = 'M' WHERE id IN (?, ?)", tiedLow, tiedHigh); err != nil {
		t.Fatalf("plant a shared key: %v", err)
	}
	// The dependency sits in another epic: between two leaves of one epic,
	// rank orders the work and a blocked-by edge is refused.
	dependency := create("dependency", "task", create("upstream", "epic", ""))
	depAdd := func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runLeaf(depAddLeaf(), ctx, stdout, ap, args)
	}
	if err := depAdd(ctx, io.Discard, ap, []string{live, "--blocked-by", dependency}); err != nil {
		t.Fatalf("dep add %s --blocked-by %s: %v", live, dependency, err)
	}
	parentSet := func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runLeaf(parentSetLeaf(), ctx, stdout, ap, args)
	}
	commentAdd := func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runLeaf(commentAddLeaf(), ctx, stdout, ap, args)
	}
	commentRm := func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runLeaf(commentRmLeaf(), ctx, stdout, ap, args)
	}
	transition := func(spec transitionSpec) func(context.Context, io.Writer, *app.App, []string) error {
		return func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
			return runTransition(ctx, stdout, ap, args, spec)
		}
	}

	type runner func(context.Context, io.Writer, *app.App, []string) error
	cases := []struct {
		name     string
		run      runner
		args     []string
		wantCode int
		reason   commandReason
	}{
		{"comment add blank body", commentAdd, []string{live, "--body", "  "}, ExitValidation, reasonValidationRefused},
		{"comment rm blank id", commentRm, []string{"  "}, ExitValidation, reasonValidationRefused},
		{"rank a deleted issue to the top", runRank, []string{gone, "--top"}, ExitValidation, reasonValidationRefused},
		{"rank against a deleted issue", runRank, []string{live, "--above", gone}, ExitValidation, reasonValidationRefused},
		{"rank an issue against itself", runRank, []string{live, "--above", live}, ExitValidation, reasonValidationRefused},
		{"rank an issue against its own epic", runRank, []string{live, "--above", epic}, ExitValidation, reasonValidationRefused},
		{"rank set naming a deleted issue", runRankSet, []string{gone, live}, ExitValidation, reasonValidationRefused},
		{"rank set naming an id twice", runRankSet, []string{live, live}, ExitValidation, reasonValidationRefused},
		{"rank set naming an empty id", runRankSet, []string{live, ""}, ExitValidation, reasonValidationRefused},
		{"rank set reversing a shared key", runRankSet, []string{tiedHigh, tiedLow}, ExitValidation, reasonValidationRefused},
		{"rank set naming an issue and its epic", runRankSet, []string{live, epic}, ExitValidation, reasonValidationRefused},
		{"archive a deleted issue", transition(archiveSpec), []string{gone}, ExitValidation, reasonValidationRefused},
		{"unarchive a deleted issue", transition(unarchiveSpec), []string{gone}, ExitValidation, reasonValidationRefused},
		{"restore an archived issue", transition(restoreSpec), []string{archived}, ExitValidation, reasonValidationRefused},
		{"archive an archived issue", transition(archiveSpec), []string{archived}, ExitNoWork, reasonStateAlreadyHolds},
		{"unarchive a live issue", transition(unarchiveSpec), []string{live}, ExitNoWork, reasonStateAlreadyHolds},
		{"delete a deleted issue", transition(deleteSpec), []string{gone}, ExitNoWork, reasonStateAlreadyHolds},
		{"restore a live issue", transition(restoreSpec), []string{live}, ExitNoWork, reasonStateAlreadyHolds},
		{"dep add an edge that already exists", depAdd, []string{live, "--blocked-by", dependency}, ExitNoWork, reasonStateAlreadyHolds},
		{"parent set the parent it already has", parentSet, []string{"--child", live, "--parent", epic}, ExitNoWork, reasonStateAlreadyHolds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := tc.run(ctx, &stdout, ap, tc.args)
			if err == nil {
				t.Fatalf("%v succeeded; it should be answered without running", tc.args)
			}
			if code := ExitCode(err); code != tc.wantCode {
				t.Fatalf("%v exit code = %d, want %d (error: %v)", tc.args, code, tc.wantCode, err)
			}
			if reason := commandErrorReason(err); reason != tc.reason {
				t.Fatalf("%v reason = %q, want %q (error: %v)", tc.args, reason, tc.reason, err)
			}
		})
	}
}
