package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAPushRefusedOverAPeersPushNotifiesOnlyWhenNothingMergesIt has alpha and
// bravo each edit one ticket without syncing. alpha's mirror lands first, so
// the remote refuses bravo's push, through either push step. With bravo's
// automatic receive on, its event, push-outcome marker and sync trace all
// record the refusal as diverged and its owner hears nothing: the receive
// merges alpha's commits and owes the merge a push. With the receive off,
// nothing merges them, so the refusal is an error and the owner is paged
// push_failed.
func TestAPushRefusedOverAPeersPushNotifiesOnlyWhenNothingMergesIt(t *testing.T) {
	for _, receive := range []struct {
		name     string
		on       bool
		decision string
	}{
		{name: "receive on", on: true, decision: pushDecisionDiverged},
		{name: "receive off", on: false, decision: pushDecisionError},
	} {
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
			t.Run(receive.name+"/"+step.name, func(t *testing.T) {
				r, base := newSharedCheckouts(t)
				finishSharedCheckouts(t, r, base)
				writeOwnerNotifyConfig(t, r.bravo, r.sink, receive.on)
				runCLIInDir(t, r.alpha, "update", r.ticket, "--description", "alpha-description")
				runCLIInDir(t, r.bravo, "update", r.ticket, "--title", "bravo-title")
				runCLIInDir(t, r.alpha, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
				assertPushDecision(t, lastEvent(t), pushDecisionPushed)

				if _, err := runCLIInDirAllowError(t, r.bravo, step.args...); (err != nil) != step.fails {
					t.Fatalf("bravo's %s error = %v; want an error: %t", step.name, err, step.fails)
				}
				ev := lastEvent(t)
				assertPushDecision(t, ev, receive.decision)
				if merges := ev.Facts["push.outcomes"].([]any)[0].(map[string]any)["push.receive_merges"]; merges != receive.on {
					t.Fatalf("bravo's push.receive_merges = %v; want %t", merges, receive.on)
				}
				ws, err := workspace.Resolve(r.bravo)
				if err != nil {
					t.Fatalf("resolve bravo: %v", err)
				}
				if rec, _, ok := lastPushOutcome(ws, time.Now()); !ok || rec.Decision != receive.decision {
					t.Fatalf("bravo's push-outcome marker = %+v (ok=%t); want %s", rec, ok, receive.decision)
				}
				if trace := lastPushTrace(t, ws); trace.Decision != receive.decision {
					t.Fatalf("bravo's push trace decision = %q; want %s", trace.Decision, receive.decision)
				}
				payload, err := os.ReadFile(r.sink)
				if receive.on {
					if !os.IsNotExist(err) {
						t.Fatalf("bravo's owner was notified: %q (err=%v)", payload, err)
					}
					return
				}
				if err != nil || strings.Count(string(payload), "push_failed origin/master") != 1 {
					t.Fatalf("bravo's owner notifications = %q (err=%v); want one push_failed origin/master", payload, err)
				}
			})
		}
	}
}

// assertPushDecision checks the event carries one push attempt, ended with
// decision.
func assertPushDecision(t *testing.T, ev telemetry.Event, decision string) {
	t.Helper()
	outcomes := ev.Occurrences["push.outcomes"]
	if len(outcomes) != 1 {
		t.Fatalf("event %q push.outcomes = %v; want the one attempt", ev.Event, ev.Occurrences["push.outcomes"])
	}
	if outcome, _ := outcomes[0].(map[string]any); outcome["push.decision"] != decision || outcome["push.remote"] != "origin" || outcome["push.branch"] != "master" {
		t.Fatalf("event %q push outcome = %v; want decision %s on origin/master", ev.Event, outcome, decision)
	}
}
