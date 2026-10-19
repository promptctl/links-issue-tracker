package cli

import (
	"os"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAPushRefusedOverAPeersPushNotifiesNobody has alpha and bravo each edit
// one ticket without syncing. alpha's mirror lands first, so the remote
// refuses bravo's push, through either push step. bravo's event, push-outcome
// marker and sync trace all record the refusal as diverged, and its owner
// hears nothing: the receive's merge is what lets bravo's next push land.
func TestAPushRefusedOverAPeersPushNotifiesNobody(t *testing.T) {
	for _, step := range []struct {
		name string
		args []string
		// fails is whether the step exits with the refusal: `lit sync push`
		// answers the terminal, the mirror has no terminal to answer.
		fails bool
	}{
		{name: "on-change mirror", args: []string{"sync", backgroundMirrorSubcommand, "--parent-pid", "0"}},
		{name: "lit sync push", args: []string{"sync", "push"}, fails: true},
	} {
		t.Run(step.name, func(t *testing.T) {
			r, base := newSharedCheckouts(t)
			finishSharedCheckouts(t, r, base)
			runCLIInDir(t, r.alpha, "update", r.ticket, "--description", "alpha-description")
			runCLIInDir(t, r.bravo, "update", r.ticket, "--title", "bravo-title")
			runCLIInDir(t, r.alpha, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
			assertPushDecision(t, lastEvent(t), pushDecisionPushed)

			if _, err := runCLIInDirAllowError(t, r.bravo, step.args...); (err != nil) != step.fails {
				t.Fatalf("bravo's %s error = %v; want an error: %t", step.name, err, step.fails)
			}
			assertPushDecision(t, lastEvent(t), pushDecisionDiverged)
			ws, err := workspace.Resolve(r.bravo)
			if err != nil {
				t.Fatalf("resolve bravo: %v", err)
			}
			if rec, _, ok := lastPushOutcome(ws, time.Now()); !ok || rec.Decision != pushDecisionDiverged {
				t.Fatalf("bravo's push-outcome marker = %+v (ok=%t); want diverged", rec, ok)
			}
			if trace := lastPushTrace(t, ws); trace.Decision != pushDecisionDiverged {
				t.Fatalf("bravo's push trace decision = %q; want diverged", trace.Decision)
			}
			if payload, err := os.ReadFile(r.sink); !os.IsNotExist(err) {
				t.Fatalf("bravo's owner was notified: %q (err=%v)", payload, err)
			}
		})
	}
}

// assertPushDecision checks the event carries one push attempt, ended with
// decision.
func assertPushDecision(t *testing.T, ev telemetry.Event, decision string) {
	t.Helper()
	outcomes, _ := ev.Facts["push.outcomes"].([]any)
	if len(outcomes) != 1 {
		t.Fatalf("event %q push.outcomes = %v; want the one attempt", ev.Event, ev.Facts["push.outcomes"])
	}
	if outcome, _ := outcomes[0].(map[string]any); outcome["push.decision"] != decision || outcome["push.remote"] != "origin" || outcome["push.branch"] != "master" {
		t.Fatalf("event %q push outcome = %v; want decision %s on origin/master", ev.Event, outcome, decision)
	}
}
