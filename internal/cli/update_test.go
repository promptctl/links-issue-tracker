package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
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

// driveTransition runs a transition to completion through the real CLI,
// transparently satisfying the two-phase contract for actions whose default
// pre-guidance template gates them (e.g. `done`): if the first run prints a
// preview carrying an `--apply=<token>`, it re-runs with that token to apply.
// Single-phase actions (start/close/reopen) complete on the first run.
func driveTransition(t *testing.T, ctx context.Context, ap *app.App, id string, action model.ActionName, extra ...string) {
	t.Helper()
	var first bytes.Buffer
	args := append([]string{id}, extra...)
	if err := runTransition(ctx, &first, ap, args, action); err != nil {
		t.Fatalf("runTransition(%s %s) error = %v", action, id, err)
	}
	if m := applyTokenRE.FindStringSubmatch(first.String()); m != nil {
		applyArgs := append([]string{id, "--apply=" + m[1]}, extra...)
		if err := runTransition(ctx, &bytes.Buffer{}, ap, applyArgs, action); err != nil {
			t.Fatalf("runTransition(%s %s --apply) error = %v", action, id, err)
		}
	}
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
	if _, err := ap.Store.StartIssue(ctx, store.StartIssueInput{IssueID: issue.ID, Assignee: "tester", CreatedBy: "tester"}); err != nil {
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
	if _, err := ap.Store.StartIssue(ctx, store.StartIssueInput{IssueID: issue.ID, Assignee: "tester", CreatedBy: "tester"}); err != nil {
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
	if _, err := ap.Store.StartIssue(ctx, store.StartIssueInput{IssueID: issue.ID, Assignee: "tester", CreatedBy: "tester"}); err != nil {
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
	if _, err := ap.Store.StartIssue(ctx, store.StartIssueInput{IssueID: issue.ID, Assignee: "tester", CreatedBy: "tester"}); err != nil {
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
	if _, err := ap.Store.StartIssue(ctx, store.StartIssueInput{IssueID: issue.ID, Assignee: "tester", CreatedBy: "tester"}); err != nil {
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
	if _, err := ap.Store.StartIssue(ctx, store.StartIssueInput{IssueID: issue.ID, Assignee: "tester", CreatedBy: "tester"}); err != nil {
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
	if _, err := ap.Store.StartIssue(ctx, store.StartIssueInput{IssueID: issue.ID, Assignee: "tester", CreatedBy: "tester"}); err != nil {
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

// TestRunTransitionTargetStateMatrix drives every (verb, from-state) cell of
// the target-state model through the CLI verb path. Each verb is sugar for
// "set target state to X": every cell succeeds and lands on the verb's target
// state; diagonal cells (target == current state) are no-ops that record
// nothing; off-diagonal cells record exactly one event named after the verb's
// action. Assertions are behavioral — resulting state and event-log content —
// never output wording.
func TestRunTransitionTargetStateMatrix(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	ctx := context.Background()

	const owner = "matrix-owner"
	fromStates := []model.State{model.StateOpen, model.StateInProgress, model.StateClosed}
	verbs := []struct {
		action model.ActionName
		target model.State
	}{
		{action: "start", target: model.StateInProgress},
		{action: "done", target: model.StateClosed},
		{action: "close", target: model.StateClosed},
		{action: "reopen", target: model.StateOpen},
	}

	for _, verb := range verbs {
		for _, from := range fromStates {
			t.Run(string(verb.action)+"_from_"+string(from), func(t *testing.T) {
				ap := newTestCLIApp(t)
				issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
					Prefix: "test", Title: "matrix " + string(verb.action), Topic: "lifecycle", IssueType: "task", Priority: 0,
				})
				if err != nil {
					t.Fatalf("CreateIssue() error = %v", err)
				}
				// Drive the issue to the from-state. The claim is always held by
				// `owner` so the start diagonal (start on an in_progress issue by
				// the same owner) is a pure no-op rather than a claim transfer.
				switch from {
				case model.StateInProgress:
					if _, err := ap.Store.StartIssue(ctx, store.StartIssueInput{
						IssueID: issue.ID, Assignee: owner, CreatedBy: owner,
					}); err != nil {
						t.Fatalf("setup start error = %v", err)
					}
				case model.StateClosed:
					if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
						IssueID: issue.ID, Action: "close", CreatedBy: owner,
					}); err != nil {
						t.Fatalf("setup close error = %v", err)
					}
				}
				before, err := ap.Store.GetIssueDetail(ctx, issue.ID)
				if err != nil {
					t.Fatalf("GetIssueDetail(before) error = %v", err)
				}

				var extra []string
				if verb.action == "start" {
					extra = []string{"--assignee", owner}
				}
				// `lit close` requires a resolution at its command boundary; supply
				// a valid one so the cell exercises the transition, not the parse
				// rejection (which has its own dedicated test).
				if verb.action == "close" {
					extra = []string{"--resolution", "wontfix"}
				}
				driveTransition(t, ctx, ap, issue.ID, verb.action, extra...)

				after, err := ap.Store.GetIssueDetail(ctx, issue.ID)
				if err != nil {
					t.Fatalf("GetIssueDetail(after) error = %v", err)
				}
				if got := after.Issue.State(); got != verb.target {
					t.Fatalf("state after %s from %s = %q, want %q", verb.action, from, got, verb.target)
				}
				added := after.Events[len(before.Events):]
				if from == verb.target {
					if len(added) != 0 {
						t.Fatalf("diagonal cell %s from %s recorded %d events, want 0 (no-ops record nothing): %#v", verb.action, from, len(added), added)
					}
					return
				}
				if len(added) != 1 {
					t.Fatalf("cell %s from %s recorded %d events, want exactly 1: %#v", verb.action, from, len(added), added)
				}
				if added[0].Action != string(verb.action) {
					t.Fatalf("cell %s from %s recorded action %q, want %q (done-vs-close distinction lives in event history)", verb.action, from, added[0].Action, verb.action)
				}
			})
		}
	}
}

// TestRunTransitionMatrixContainerCell pins the one rejection that survives
// the target-state model: acting on a container, whose state derives from its
// children. The assertion is the typed shape from links-lifecycle-2wz.3 —
// errors.As + the live unfinished count — never the message prose.
func TestRunTransitionMatrixContainerCell(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Matrix epic", Topic: "lifecycle", IssueType: "epic", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Open child", Topic: "lifecycle", IssueType: "task", Priority: 0, ParentID: epic.ID,
	}); err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}

	for _, action := range []model.ActionName{"start", "done", "close", "reopen"} {
		t.Run(string(action), func(t *testing.T) {
			// Guided actions (e.g. `done`) preview before they act, so the
			// container rejection only surfaces at the apply phase; drive past
			// the preview when a token is offered.
			// `close` requires a resolution at its command boundary; supply a valid
			// one so the cell reaches the deeper container rejection rather than the
			// parse rejection. The other actions take no resolution.
			baseArgs := []string{epic.ID}
			if action == "close" {
				baseArgs = append(baseArgs, "--resolution", "wontfix")
			}
			var preview bytes.Buffer
			err := runTransition(ctx, &preview, ap, baseArgs, action)
			if err == nil {
				m := applyTokenRE.FindStringSubmatch(preview.String())
				if m == nil {
					t.Fatalf("runTransition(%s epic) = nil with no apply token, want container rejection; output %q", action, preview.String())
				}
				err = runTransition(ctx, &bytes.Buffer{}, ap, append(append([]string{}, baseArgs...), "--apply="+m[1]), action)
			}
			if err == nil {
				t.Fatalf("runTransition(%s epic) = nil, want container rejection", action)
			}
			var containerErr model.ContainerActionError
			if !errors.As(err, &containerErr) {
				t.Fatalf("runTransition(%s epic) error = %q, want model.ContainerActionError", action, err)
			}
			if got := containerErr.Unfinished(); got != 1 {
				t.Fatalf("ContainerActionError.Unfinished() = %d, want 1 (one open child)", got)
			}
		})
	}
}

// TestRunTransitionStartClaimTransferEmitsNotice pins the no-silent-failure
// half of the reclaim path: taking an in_progress issue over from another
// owner succeeds, but announces the old and new owner on human output. The
// assertion names both identities rather than pinning wording.
func TestRunTransitionStartClaimTransferEmitsNotice(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Contested claim", Topic: "lifecycle", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := runTransition(ctx, &bytes.Buffer{}, ap, []string{issue.ID, "--assignee", "agent-alice"}, "start"); err != nil {
		t.Fatalf("runTransition(first start) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runTransition(ctx, &stdout, ap, []string{issue.ID, "--assignee", "agent-bob"}, "start"); err != nil {
		t.Fatalf("runTransition(reclaim) error = %v, want success with notice", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "agent-alice") || !strings.Contains(out, "agent-bob") {
		t.Fatalf("reclaim output %q does not name both the old and new owner", out)
	}
	reclaimed, err := ap.Store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got := reclaimed.AssigneeValue(); got != "agent-bob" {
		t.Fatalf("AssigneeValue() = %q, want agent-bob (transfer must still succeed)", got)
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
	stdout.Reset()
	if err := runTransition(ctx, &stdout, ap, []string{leaf.ID, "--assignee", "tester"}, "start"); err != nil {
		t.Fatalf("runTransition(start leaf) error = %v", err)
	}
	started, err := ap.Store.GetIssue(ctx, leaf.ID)
	if err != nil {
		t.Fatalf("GetIssue(leaf) error = %v", err)
	}
	if started.State() != model.StateInProgress {
		t.Fatalf("started.State() = %q, want in_progress", started.State())
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
	if err := runUpdate(ctx, &stdout, ap, []string{created.ID, "--status", "in_progress", "--assignee", "tester"}); err != nil {
		t.Fatalf("runUpdate(--status in_progress) error = %v", err)
	}

	updated, err := ap.Store.GetIssue(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
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
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
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
	if err := runUpdate(ctx, &stdout, ap, []string{created.ID, "--priority", "1", "--assignee", "alice", "--labels", "api,urgent"}); err != nil {
		t.Fatalf("runUpdate(field flags) error = %v", err)
	}

	updated, err := ap.Store.GetIssue(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
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
	}); err != nil {
		t.Fatalf("runNew(--prompt) error = %v", err)
	}
	createdID := firstIssueID(t, newOut.String())
	created, err := ap.Store.GetIssue(ctx, createdID)
	if err != nil {
		t.Fatalf("GetIssue(new) error = %v", err)
	}
	if created.Prompt != "Render at 1024x768 and verify no NaNs." {
		t.Fatalf("created.Prompt = %q, want trimmed prompt body", created.Prompt)
	}

	var upOut bytes.Buffer
	if err := runUpdate(ctx, &upOut, ap, []string{created.ID, "--prompt", "Run --headless instead."}); err != nil {
		t.Fatalf("runUpdate(--prompt) error = %v", err)
	}
	updated, err := ap.Store.GetIssue(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssue(update) error = %v", err)
	}
	if updated.Prompt != "Run --headless instead." {
		t.Fatalf("updated.Prompt = %q, want updated value", updated.Prompt)
	}
}

func TestRunUpdateRejectsReasonWithNoChanges(t *testing.T) {
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

	// --reason alone (no field flags and no --status) must still be rejected
	// because there is nothing to record the reason on.
	var stdout bytes.Buffer
	err = runUpdate(ctx, &stdout, ap, []string{created.ID, "--reason", "no fields here"})
	if err == nil {
		t.Fatal("runUpdate(--reason with no fields) error = nil, want validation error")
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
	if err := runUpdate(ctx, &stdout, ap, []string{epic.ID, "--title", "Renamed epic", "--description", "New body"}); err != nil {
		t.Fatalf("runUpdate(epic --title --description) error = %v", err)
	}

	updated, err := ap.Store.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
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
	err = runUpdate(ctx, &stdout, ap, []string{created.ID, "--status="})
	if err == nil {
		t.Fatal("runUpdate(--status=) error = nil, want validation error")
	}
	if err.Error() != "--status requires a non-empty value" {
		t.Fatalf("runUpdate error = %q, want %q", err.Error(), "--status requires a non-empty value")
	}
}

func TestResolveIdentity(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		env      string
		want     string
	}{
		{name: "env wins over explicit", explicit: "alice", env: "sess-1", want: "claude_sess-1"},
		{name: "env wins over empty explicit", explicit: "", env: "sess-2", want: "claude_sess-2"},
		{name: "no env, explicit passes through", explicit: "alice", env: "", want: "alice"},
		{name: "no env, no explicit, empty result", explicit: "", env: "", want: ""},
		{name: "whitespace env treated as empty", explicit: "alice", env: "   ", want: "alice"},
		{name: "whitespace explicit trimmed", explicit: "  alice  ", env: "", want: "alice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.env)
			got := resolveIdentity(tc.explicit)
			if got != tc.want {
				t.Fatalf("resolveIdentity(%q) with env=%q = %q, want %q", tc.explicit, tc.env, got, tc.want)
			}
		})
	}
}

func TestRunTransitionStartWithoutAssigneeSucceeds(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "No assignee start", Topic: "lifecycle", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runTransition(ctx, &stdout, ap, []string{issue.ID}, "start"); err != nil {
		t.Fatalf("runTransition(start without --assignee) error = %v", err)
	}
	started, err := ap.Store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if started.State() != model.StateInProgress {
		t.Fatalf("started.State() = %q, want in_progress", started.State())
	}
	if got := started.AssigneeValue(); got != "" {
		t.Fatalf("started.AssigneeValue() = %q, want empty (no env, no flag)", got)
	}
}

func TestRunTransitionStartStampsAssigneeFromSessionEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-abc")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Env-stamped start", Topic: "lifecycle", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runTransition(ctx, &stdout, ap, []string{issue.ID}, "start"); err != nil {
		t.Fatalf("runTransition(start) error = %v", err)
	}
	started, err := ap.Store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got, want := started.AssigneeValue(), "claude_sess-abc"; got != want {
		t.Fatalf("started.AssigneeValue() = %q, want %q", got, want)
	}
}

// lastEventActorForAction returns the actor recorded on the most recent event
// with the given action — the durable "who performed this transition" signal.
func lastEventActorForAction(t *testing.T, ap *app.App, ctx context.Context, id, action string) string {
	t.Helper()
	detail, err := ap.Store.GetIssueDetail(ctx, id)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	actor := ""
	found := false
	for _, e := range detail.Events {
		if e.Action == action {
			actor = e.Actor
			found = true
		}
	}
	if !found {
		t.Fatalf("no event with action %q recorded for %s", action, id)
	}
	return actor
}

// TestRunTransitionActorFromSessionEnv pins the attribution fix: with
// CLAUDE_CODE_SESSION_ID set, the event actor (not just the assignee) resolves
// to claude_<session>, so history shows the agent performed the transition.
func TestRunTransitionActorFromSessionEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-actor")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Actor from session", Topic: "attribution", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	driveTransition(t, ctx, ap, issue.ID, "start")
	driveTransition(t, ctx, ap, issue.ID, "done")
	if got, want := lastEventActorForAction(t, ap, ctx, issue.ID, "start"), "claude_sess-actor"; got != want {
		t.Fatalf("start event actor = %q, want %q", got, want)
	}
	if got, want := lastEventActorForAction(t, ap, ctx, issue.ID, "done"), "claude_sess-actor"; got != want {
		t.Fatalf("done event actor = %q, want %q", got, want)
	}
}

// TestRunTransitionActorFallsBackToByFlag pins the no-agent path: without the
// session env, the actor keeps the existing --by/$USER behavior.
func TestRunTransitionActorFallsBackToByFlag(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Actor from by flag", Topic: "attribution", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	driveTransition(t, ctx, ap, issue.ID, "start", "--by=alice")
	if got, want := lastEventActorForAction(t, ap, ctx, issue.ID, "start"), "alice"; got != want {
		t.Fatalf("start event actor = %q, want %q", got, want)
	}
}

func TestRunUpdateStatusInProgressUsesSessionEnvAssignee(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-xyz")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Update stamps assignee", Topic: "lifecycle", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{issue.ID, "--status", "in_progress"}); err != nil {
		t.Fatalf("runUpdate(--status in_progress) error = %v", err)
	}
	updated, err := ap.Store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got, want := updated.AssigneeValue(), "claude_sess-xyz"; got != want {
		t.Fatalf("updated.AssigneeValue() = %q, want %q", got, want)
	}
}

// The session env is deliberately set in the clear/verbatim tests below: the
// bug being pinned was claim-time session resolution leaking into `update`,
// where it silently rewrote an explicit clear (or third-party assignee) into
// a self-assignment. [LAW:no-silent-failure]
func TestRunUpdateClearAssigneeLeavesOpenIssueUnassigned(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-grooming")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Stale claim", Topic: "lifecycle", IssueType: "task", Priority: 0,
		Assignee: "claude_abandoned-session",
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{issue.ID, "--assignee", ""}); err != nil {
		t.Fatalf("runUpdate(--assignee \"\") error = %v", err)
	}
	updated, err := ap.Store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got := updated.AssigneeValue(); got != "" {
		t.Fatalf("updated.AssigneeValue() = %q, want empty: explicit clear must never self-assign", got)
	}
	if got := updated.State(); got != model.StateOpen {
		t.Fatalf("updated.State() = %q, want open", got)
	}
}

func TestRunUpdateExplicitAssigneeHonoredVerbatim(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-me")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Hand off", Topic: "lifecycle", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{issue.ID, "--assignee", "claude_other-session"}); err != nil {
		t.Fatalf("runUpdate(--assignee other) error = %v", err)
	}
	updated, err := ap.Store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got, want := updated.AssigneeValue(), "claude_other-session"; got != want {
		t.Fatalf("updated.AssigneeValue() = %q, want %q: update must not rewrite an explicit assignee to the caller", got, want)
	}
}

func TestRunUpdateClearAssigneeWithStartStaysCleared(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-me")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Start unclaimed", Topic: "lifecycle", IssueType: "task", Priority: 0,
		Assignee: "claude_abandoned-session",
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{issue.ID, "--status", "in_progress", "--assignee", ""}); err != nil {
		t.Fatalf("runUpdate(--status in_progress --assignee \"\") error = %v", err)
	}
	updated, err := ap.Store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got := updated.State(); got != model.StateInProgress {
		t.Fatalf("updated.State() = %q, want in_progress", got)
	}
	if got := updated.AssigneeValue(); got != "" {
		t.Fatalf("updated.AssigneeValue() = %q, want empty: explicit clear wins over the bare-status claim convenience", got)
	}
}
