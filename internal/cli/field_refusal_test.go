package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A value a rule refuses is refused identically on every run, so each refusal
// on the issue-writing, relation and listing commands must exit ExitValidation
// with the validation_refused remediation. The one exception is `update`
// naming no field at all: that is a malformed invocation rather than a rejected
// value, so it is a usage error.
//
// Each case drives the real command handler, and asserts the exact reason and
// exit code rather than the absence of retry advice: "does not say retry"
// holds of a wrong classification too.
func TestFieldRefusalsAreDeterministicRefusals(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	task, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "a task", Topic: "fields", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(task) error = %v", err)
	}
	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "an epic", Topic: "fields", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	blocked, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "blocked by the task", Topic: "fields", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(blocked) error = %v", err)
	}
	if err := runDepAdd(ctx, io.Discard, ap, []string{"--from", task.ID, "--to", blocked.ID}); err != nil {
		t.Fatalf("dep add %s blocks %s: %v", task.ID, blocked.ID, err)
	}
	runLs := func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runListWithStore(ctx, stdout, ap.Store, workspaceReadyPolicy(ap), args)
	}

	type runner func(context.Context, io.Writer, *app.App, []string) error
	cases := []struct {
		name     string
		run      runner
		args     []string
		wantCode int
		reason   string
	}{
		{"new topic below the floor", runNew, []string{"--title", "t", "--topic", "ci"}, ExitValidation, "validation_refused"},
		{"new topic above the cap", runNew, []string{"--title", "t", "--topic", strings.Repeat("a", 31)}, ExitValidation, "validation_refused"},
		{"new topic missing", runNew, []string{"--title", "t"}, ExitValidation, "validation_refused"},
		{"new title missing", runNew, []string{"--topic", "fields"}, ExitValidation, "validation_refused"},
		{"update title blanked", runUpdate, []string{task.ID, "--title", "  "}, ExitValidation, "validation_refused"},
		{"update leaf to container", runUpdate, []string{task.ID, "--type", "epic"}, ExitValidation, "validation_refused"},
		{"update container to leaf", runUpdate, []string{epic.ID, "--type", "task"}, ExitValidation, "validation_refused"},
		{"update naming no field", runUpdate, []string{task.ID}, ExitUsage, "usage_error"},
		{"label add blank label", runLabelAdd, []string{task.ID, " "}, ExitValidation, "validation_refused"},
		{"dep add unknown relation type", runDepAdd, []string{"--from", task.ID, "--to", epic.ID, "--type", "bogus"}, ExitValidation, "validation_refused"},
		{"dep add self-loop", runDepAdd, []string{"--from", task.ID, "--to", task.ID}, ExitValidation, "validation_refused"},
		{"dep add closing a blocks cycle", runDepAdd, []string{"--from", blocked.ID, "--to", task.ID}, ExitValidation, "validation_refused"},
		{"ls unsupported sort field", runLs, []string{"--sort", "bogus"}, ExitValidation, "validation_refused"},
		{"ls unknown state in --status", runLs, []string{"--status", "bogus"}, ExitValidation, "validation_refused"},
		{"ls query term refused", runLs, []string{"--query", "has:nope"}, ExitValidation, "validation_refused"},
		{"ls query timestamp refused", runLs, []string{"--query", "updated>=junk"}, ExitValidation, "validation_refused"},
		{"ls query term contradicting a flag", runLs, []string{"--updated-after", "2026-02-01T00:00:00Z", "--query", "updated>=2026-01-01T00:00:00Z"}, ExitValidation, "validation_refused"},
		{"ls query window inverted", runLs, []string{"--query", "updated>=2026-02-01T00:00:00Z updated<=2026-01-01T00:00:00Z"}, ExitValidation, "validation_refused"},
		{"ls --updated-after not a timestamp", runLs, []string{"--updated-after", "junk"}, ExitValidation, "validation_refused"},
		{"ls --updated-before not a timestamp", runLs, []string{"--updated-before", "junk"}, ExitValidation, "validation_refused"},
		{"ls flag window inverted", runLs, []string{"--updated-after", "2026-02-01T00:00:00Z", "--updated-before", "2026-01-01T00:00:00Z"}, ExitValidation, "validation_refused"},
		{"ls negative --limit", runLs, []string{"--limit", "-1"}, ExitValidation, "validation_refused"},
		{"ls negative query limit", runLs, []string{"--query", "limit:-1"}, ExitValidation, "validation_refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := tc.run(ctx, &stdout, ap, tc.args)
			if err == nil {
				t.Fatalf("%v succeeded; the rule should refuse it", tc.args)
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
