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
			run  func(t *testing.T, dir string) (string, error)
			// fails is whether the step exits with the refusal: `lit sync push`
			// answers the terminal, the mirror has no terminal to answer.
			fails bool
			// liveStoreSteps is the mirror's live-store step: a refused push
			// landed nothing, so the live store waits for the next receive.
			liveStoreSteps []string
			// reason and failureClass are how the step's event ends: `lit sync
			// push` on the push_diverged sync-failure block, with or without a
			// receive, since its caller asked for the push to land now.
			reason       string
			failureClass any
		}{
			{name: "on-change mirror", run: func(t *testing.T, dir string) (string, error) {
				return runWorkerInDirAllowError(t, dir, backgroundMirrorSubcommand)
			}, liveStoreSteps: []string{"wait"}},
			{name: "lit sync push", run: func(t *testing.T, dir string) (string, error) { return runCLIInDirAllowError(t, dir, "sync", "push") }, fails: true, reason: "sync_divergence", failureClass: "push_diverged"},
		} {
			t.Run(receive.name+"/"+step.name, func(t *testing.T) {
				r, base := newSharedCheckouts(t)
				finishSharedCheckouts(t, r, base)
				writeOwnerNotifyConfig(t, r.bravo, r.sink, receive.on)
				runCLIInDir(t, r.alpha, "update", r.ticket, "--description", "alpha-description")
				runCLIInDir(t, r.bravo, "update", r.ticket, "--title", "bravo-title")
				runWorkerInDir(t, r.alpha, backgroundMirrorSubcommand)
				assertPushDecision(t, lastEvent(t), pushDecisionPushed)
				assertLiveStoreSteps(t, lastEvent(t), "record")

				if _, err := step.run(t, r.bravo); (err != nil) != step.fails {
					t.Fatalf("bravo's %s error = %v; want an error: %t", step.name, err, step.fails)
				}
				ev := lastEvent(t)
				assertPushDecision(t, ev, receive.decision)
				if ev.Reason != step.reason || ev.Facts["sync.failure_class"] != step.failureClass {
					t.Fatalf("bravo's %s event ended reason=%q sync.failure_class=%v; want %q %v", step.name, ev.Reason, ev.Facts["sync.failure_class"], step.reason, step.failureClass)
				}
				if step.liveStoreSteps != nil {
					assertLiveStoreSteps(t, ev, step.liveStoreSteps...)
				}
				if merges := ev.Occurrences["push.outcomes"][0].(map[string]any)["push.receive_merges"]; merges != receive.on {
					t.Fatalf("bravo's push.receive_merges = %v; want %t", merges, receive.on)
				}
				ws, err := workspace.Resolve(testEnv(), r.bravo)
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
					// The receive merges alpha's commits and says it owes the
					// merge a push; on-push cadence spawns no mirror here.
					runWorkerInDir(t, r.bravo, backgroundReceiveSubcommand)
					if owed := lastEvent(t).Facts["receive.push_owed"]; owed != true {
						t.Fatalf("bravo's receive.push_owed = %v; want true: its merge is not on the remote", owed)
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
