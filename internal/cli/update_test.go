package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

// extractApplyToken pulls the 8-hex-char token out of the preview output.
// The preview line looks like: ... `lit done <id> --apply=abcd1234` ...
var applyTokenRE = regexp.MustCompile(`--apply=([0-9a-f]{8})`)

func extractApplyToken(t *testing.T, previewOutput string) string {
	t.Helper()
	m := applyTokenRE.FindStringSubmatch(previewOutput)
	if m == nil {
		t.Fatalf("preview output missing --apply=<token>: %q", previewOutput)
	}
	return m[1]
}

func TestRunTransitionDonePreGuidancePrintsWithoutTransitioning(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title: "Guidance test", Topic: "guidance", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: issue.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runTransition(ctx, &stdout, ap, []string{issue.ID}, "done"); err != nil {
		t.Fatalf("runTransition(done without --apply) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Double check the ticket") {
		t.Fatalf("expected pre-guidance output, got %q", stdout.String())
	}

	detail, err := ap.Store.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if detail.Issue.State() != model.StateInProgress {
		t.Fatalf("issue should still be in_progress after bare done, got %q", detail.Issue.State())
	}
}

func TestRunTransitionDoneApplyTransitionsAndPrintsPostGuidance(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test",
		Title: "Guidance apply test", Topic: "guidance", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: issue.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	// Run the preview phase to obtain the apply token, mirroring how an agent
	// is forced to discover it.
	var preview bytes.Buffer
	if err := runTransition(ctx, &preview, ap, []string{issue.ID}, "done"); err != nil {
		t.Fatalf("runTransition(preview) error = %v", err)
	}
	token := extractApplyToken(t, preview.String())

	var stdout bytes.Buffer
	if err := runTransition(ctx, &stdout, ap, []string{issue.ID, "--apply=" + token}, "done"); err != nil {
		t.Fatalf("runTransition(done --apply=<token>) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "has been closed") {
		t.Fatalf("expected post-guidance output, got %q", stdout.String())
	}

	detail, err := ap.Store.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if detail.Issue.State() != model.StateClosed {
		t.Fatalf("issue should be closed after --apply, got %q", detail.Issue.State())
	}
}

func TestRunTransitionDoneApplyWithoutTokenRefusesWithShortMessage(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test",
		Title: "Token-required test", Topic: "guidance", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: issue.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	var stdout bytes.Buffer
	err = runTransition(ctx, &stdout, ap, []string{issue.ID, "--apply"}, "done")
	if err == nil {
		t.Fatal("runTransition(done --apply) returned nil; expected refusal")
	}
	want := "run `lit done " + issue.ID + "` first"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}

	detail, err := ap.Store.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if detail.Issue.State() != model.StateInProgress {
		t.Fatalf("issue should still be in_progress after refusal, got %q", detail.Issue.State())
	}
}

func TestRunTransitionDoneApplyEmptyValueIsRefused(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test",
		Title: "Empty-value test", Topic: "guidance", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: issue.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	// `--apply=` (explicit empty value) is an apply attempt with a malformed
	// token, not a missing flag — it must refuse just like `--apply` and
	// `--apply=deadbeef`.
	var stdout bytes.Buffer
	err = runTransition(ctx, &stdout, ap, []string{issue.ID, "--apply="}, "done")
	if err == nil {
		t.Fatal("runTransition(done --apply=) returned nil; expected refusal")
	}
	want := "run `lit done " + issue.ID + "` first"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestRunTransitionDonePreGuidanceMissingTokenPlaceholderRefusesAtLoad(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	// Project override that omits the required <token> placeholder. Without
	// the placeholder, the agent could not discover the apply token, so the
	// command refuses at load time and names the file to fix.
	overridePath := filepath.Join(ap.Workspace.RootDir, ".lit", "templates", "guidance-done-pre.md")
	if err := os.MkdirAll(filepath.Dir(overridePath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(overridePath, []byte("Run `lit done <id> --apply` to close.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test",
		Title: "Missing-token test", Topic: "guidance", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: issue.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	var stdout bytes.Buffer
	err = runTransition(ctx, &stdout, ap, []string{issue.ID}, "done")
	if err == nil {
		t.Fatal("runTransition(done) returned nil; expected template-validation error")
	}
	if !strings.Contains(err.Error(), "<token>") {
		t.Fatalf("error = %q, want mention of <token>", err.Error())
	}
	if !strings.Contains(err.Error(), overridePath) {
		t.Fatalf("error = %q, want override path %q", err.Error(), overridePath)
	}
}

func TestRunTransitionDoneApplyWithWrongTokenRefusesWithShortMessage(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test",
		Title: "Wrong-token test", Topic: "guidance", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: issue.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	var stdout bytes.Buffer
	err = runTransition(ctx, &stdout, ap, []string{issue.ID, "--apply=deadbeef"}, "done")
	if err == nil {
		t.Fatal("runTransition(done --apply=deadbeef) returned nil; expected refusal")
	}
	want := "run `lit done " + issue.ID + "` first"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestRunTransitionDoneTokenInvalidatedByDriftBetweenPreviewAndApply(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test",
		Title: "Drift test", Topic: "guidance", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: issue.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	var preview bytes.Buffer
	if err := runTransition(ctx, &preview, ap, []string{issue.ID}, "done"); err != nil {
		t.Fatalf("runTransition(preview) error = %v", err)
	}
	stale := extractApplyToken(t, preview.String())

	// Mutate the issue between preview and apply — this changes UpdatedAt and
	// must invalidate the previously-printed token.
	newTitle := "Drift test — updated"
	if _, err := ap.Store.UpdateIssue(ctx, issue.ID, store.UpdateIssueInput{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	err = runTransition(ctx, &stdout, ap, []string{issue.ID, "--apply=" + stale}, "done")
	if err == nil {
		t.Fatal("runTransition with stale token returned nil; expected refusal after drift")
	}
	want := "run `lit done " + issue.ID + "` first"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestRunTransitionRefusesEpicAndStartsLeaf(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Epic container",
		Topic:     "lifecycle",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	leaf, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Leaf work",
		Topic:     "lifecycle",
		IssueType: "task",
		Priority:  0,
		ParentID:  epic.ID,
	})
	if err != nil {
		t.Fatalf("CreateIssue(leaf) error = %v", err)
	}
	var stdout bytes.Buffer
	err = runTransition(ctx, &stdout, ap, []string{epic.ID, "--assignee", "tester"}, "start")
	if err == nil {
		t.Fatal("runTransition(start epic) returned nil; want refusal")
	}
	if !strings.Contains(err.Error(), "no start action available") {
		t.Fatalf("runTransition(start epic) error = %q, want no start action available", err.Error())
	}
	stdout.Reset()
	if err := runTransition(ctx, &stdout, ap, []string{leaf.ID, "--assignee", "tester", "--json"}, "start"); err != nil {
		t.Fatalf("runTransition(start leaf) error = %v", err)
	}
	var started model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &started); err != nil {
		t.Fatalf("json.Unmarshal(start output) error = %v", err)
	}
	if started.State() != model.StateInProgress {
		t.Fatalf("started.State() = %q, want in_progress", started.State())
	}
}

func TestRunShowEpicJSONOmitsProgressAndStatus(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Epic container",
		Topic:     "show",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Open child",
		Topic:     "show",
		IssueType: "task",
		Priority:  0,
		ParentID:  epic.ID,
	}); err != nil {
		t.Fatalf("CreateIssue(open child) error = %v", err)
	}
	closedChild, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Closed child",
		Topic:     "show",
		IssueType: "task",
		Priority:  0,
		ParentID:  epic.ID,
	})
	if err != nil {
		t.Fatalf("CreateIssue(closed child) error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: closedChild.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: closedChild.ID, Action: "done", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(done) error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runShow(ctx, &stdout, ap, []string{epic.ID, "--json"}); err != nil {
		t.Fatalf("runShow(epic --json) error = %v", err)
	}
	var payload struct {
		Issue map[string]json.RawMessage `json:"issue"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal(show output) error = %v", err)
	}
	if _, ok := payload.Issue["status"]; ok {
		t.Fatalf("epic JSON issue has status field: %s", stdout.String())
	}
	if _, ok := payload.Issue["progress"]; ok {
		t.Fatalf("epic JSON issue has progress field: %s", stdout.String())
	}
}

func TestRunUpdateSupportsStatusTransitionWithoutExplicitReason(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Update status",
		Topic:     "status",
		IssueType: "task",
		Priority:  0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{created.ID, "--status", "in_progress", "--assignee", "tester", "--json"}); err != nil {
		t.Fatalf("runUpdate(--status in_progress --json) error = %v", err)
	}

	var updated model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		t.Fatalf("json.Unmarshal(update output) error = %v", err)
	}
	if updated.State() != model.StateInProgress {
		t.Fatalf("updated.State() = %q, want in_progress", updated.State())
	}

	detail, err := ap.Store.GetIssueDetail(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if len(detail.Events) < 2 {
		t.Fatalf("len(detail.Events) = %d, want >= 2", len(detail.Events))
	}
	last := detail.Events[len(detail.Events)-1]
	if last.Action != "start" {
		t.Fatalf("last.Action = %q, want start", last.Action)
	}
	if !strings.Contains(last.Reason, "status update via lit update") {
		t.Fatalf("last.Reason = %q, want default update reason", last.Reason)
	}
}

func TestRunUpdateSupportsFieldMutations(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Update fields",
		Topic:     "fields",
		IssueType: "task",
		Priority:  0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{created.ID, "--priority", "1", "--assignee", "alice", "--labels", "api,urgent", "--json"}); err != nil {
		t.Fatalf("runUpdate(field flags --json) error = %v", err)
	}

	var updated model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		t.Fatalf("json.Unmarshal(update output) error = %v", err)
	}
	if updated.Priority != 1 {
		t.Fatalf("updated.Priority = %d, want 1", updated.Priority)
	}
	if updated.AssigneeValue() != "alice" {
		t.Fatalf("updated.AssigneeValue() = %q, want alice", updated.AssigneeValue())
	}
	if len(updated.Labels) != 2 || updated.Labels[0] != "api" || updated.Labels[1] != "urgent" {
		t.Fatalf("updated.Labels = %#v, want [api urgent]", updated.Labels)
	}
}

func TestRunNewAndUpdateCarryPromptField(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	var newOut bytes.Buffer
	if err := runNew(ctx, &newOut, ap, []string{
		"--title", "Wire prompt field",
		"--topic", "prompts",
		"--type", "task",
		"--priority", "1",
		"--prompt", "Render at 1024x768 and verify no NaNs.",
		"--json",
	}); err != nil {
		t.Fatalf("runNew(--prompt) error = %v", err)
	}
	var created model.Issue
	if err := json.Unmarshal(newOut.Bytes(), &created); err != nil {
		t.Fatalf("json.Unmarshal(new) error = %v", err)
	}
	if created.Prompt != "Render at 1024x768 and verify no NaNs." {
		t.Fatalf("created.Prompt = %q, want trimmed prompt body", created.Prompt)
	}

	var upOut bytes.Buffer
	if err := runUpdate(ctx, &upOut, ap, []string{created.ID, "--prompt", "Run --headless instead.", "--json"}); err != nil {
		t.Fatalf("runUpdate(--prompt) error = %v", err)
	}
	var updated model.Issue
	if err := json.Unmarshal(upOut.Bytes(), &updated); err != nil {
		t.Fatalf("json.Unmarshal(update) error = %v", err)
	}
	if updated.Prompt != "Run --headless instead." {
		t.Fatalf("updated.Prompt = %q, want updated value", updated.Prompt)
	}
}

func TestRunUpdateRejectsReasonWithoutStatus(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Validation",
		Topic:     "validation",
		IssueType: "task",
		Priority:  0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	err = runUpdate(ctx, &stdout, ap, []string{created.ID, "--reason", "not allowed", "--json"})
	if err == nil {
		t.Fatal("runUpdate(--reason without --status) error = nil, want validation error")
	}
	if err.Error() != "--reason requires --status" {
		t.Fatalf("runUpdate error = %q, want %q", err.Error(), "--reason requires --status")
	}
}

func TestRunUpdateContainerFieldsWithoutStatusFlag(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix:    "test",
		Title:     "Original epic title",
		Topic:     "container-update",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{epic.ID, "--title", "Renamed epic", "--description", "New body", "--json"}); err != nil {
		t.Fatalf("runUpdate(epic --title --description) error = %v", err)
	}

	var updated model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		t.Fatalf("json.Unmarshal(update output) error = %v", err)
	}
	if updated.Title != "Renamed epic" {
		t.Fatalf("updated.Title = %q, want %q", updated.Title, "Renamed epic")
	}
	if updated.Description != "New body" {
		t.Fatalf("updated.Description = %q, want %q", updated.Description, "New body")
	}
	if updated.StatusValue() != "" {
		t.Fatalf("updated.StatusValue() = %q, want empty (container has no own status)", updated.StatusValue())
	}

	detail, err := ap.Store.GetIssueDetail(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	for _, h := range detail.Events {
		switch h.Action {
		case "start", "done", "close", "reopen":
			t.Fatalf("field-only update on container produced transition action %q; events: %#v", h.Action, detail.Events)
		}
	}
}

func TestRunUpdateRejectsEmptyStatusValue(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Empty status",
		Topic:     "status",
		IssueType: "task",
		Priority:  0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	err = runUpdate(ctx, &stdout, ap, []string{created.ID, "--status=", "--json"})
	if err == nil {
		t.Fatal("runUpdate(--status= --json) error = nil, want validation error")
	}
	if err.Error() != "--status requires a non-empty value" {
		t.Fatalf("runUpdate error = %q, want %q", err.Error(), "--status requires a non-empty value")
	}
}
