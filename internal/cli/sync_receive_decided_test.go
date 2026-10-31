package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// receiveEvent runs one automatic receive (receiveOnce) on ws and returns the
// event it wrote and the one value it put on receive.outcomes.
func receiveEvent(t *testing.T, ws workspace.Info) (telemetry.Event, map[string]any) {
	t.Helper()
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	receiveOnce(ctx, testStderr(t), testEnv(), ws)
	ev := writtenEvent(t, recorder.Finish(invocationEnd(time.Now(), nil, nil)))
	outcomes := ev.Occurrences["receive.outcomes"]
	if len(outcomes) != 1 {
		t.Fatalf("receive.outcomes = %#v; want the one receive's value", outcomes)
	}
	value, _ := outcomes[0].(map[string]any)
	return ev, value
}

// decidedReceive runs one automatic receive on ws and returns the one value
// it put on receive.outcomes and the decisions of the receive traces it left.
func decidedReceive(t *testing.T, ws workspace.Info) (map[string]any, []string) {
	t.Helper()
	_, value := receiveEvent(t, ws)
	var decisions []string
	for _, record := range readSyncTraces(t, ws) {
		if record.Command == receiveTraceCommand {
			decisions = append(decisions, record.Decision)
		}
	}
	return value, decisions
}

// receiveWorkspace is a git checkout with the given remotes, each a bare
// repository holding the given refs/dolt/* refs.
func receiveWorkspace(t *testing.T, doltRefs []string, remotes ...string) workspace.Info {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "checkout")
	runGit(t, base, "init", "checkout")
	runGit(t, root, "config", "user.email", "a@a.co")
	runGit(t, root, "config", "user.name", "a")
	runGit(t, root, "commit", "--allow-empty", "-m", "seed")
	for _, name := range remotes {
		bare := filepath.Join(base, name+".git")
		runGit(t, base, "init", "--bare", bare)
		for _, ref := range doltRefs {
			runGit(t, root, "push", bare, "HEAD:"+ref)
		}
		runGit(t, root, "remote", "add", name, bare)
	}
	return workspace.Info{Location: workspace.LocationFromStorageDir(filepath.Join(base, "storage")), RootDir: root, Remotes: workspace.NewRemotes(testEnv(), repositoryOf(t, root), checkoutRoot(t, root)), Project: testProject(checkoutRoot(t, root))}
}

// Every way the automatic receive ends before it fetches is a decision, and
// each puts its one value on receive.outcomes with the trace saying the same.
// [LAW:nothing-unseen]
func TestAReceiveThatEndsBeforeItFetchesSaysWhy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ws       func(t *testing.T) workspace.Info
		decision string
		remote   string
	}{
		{"git remotes unreadable", func(t *testing.T) workspace.Info {
			root := t.TempDir()
			return workspace.Info{Location: workspace.LocationFromStorageDir(filepath.Join(t.TempDir(), "storage")), RootDir: root, Remotes: workspace.NewRemotes(testEnv(), repositoryOf(t, root), checkoutRoot(t, root)), Project: testProject(checkoutRoot(t, root))}
		}, "error", ""},
		{"no git remote", func(t *testing.T) workspace.Info {
			return receiveWorkspace(t, nil)
		}, string(syncTargetNoRemote), ""},
		{"several remotes and no upstream", func(t *testing.T) workspace.Info {
			return receiveWorkspace(t, []string{"refs/dolt/data"}, "origin", "backup")
		}, string(syncTargetNoRemote), ""},
		{"a remote advertising no refs/dolt/*", func(t *testing.T) workspace.Info {
			return receiveWorkspace(t, []string{"refs/heads/main"}, "origin")
		}, string(syncTargetRemoteEmpty), "origin"},
		{"the receive lock held by another worker", func(t *testing.T) workspace.Info {
			ws := receiveWorkspace(t, []string{"refs/dolt/data"}, "origin")
			release, acquired, err := store.TryAcquireReceiveLock(ws.DatabasePath)
			if err != nil || !acquired {
				t.Fatalf("TryAcquireReceiveLock() = %t, %v; want the lock", acquired, err)
			}
			t.Cleanup(func() { _ = release() })
			return ws
		}, receiveDecisionLockHeld, "origin"},
		{"the remote unmoved since the last receive", func(t *testing.T) workspace.Info {
			ws := receiveWorkspace(t, []string{"refs/dolt/data"}, "origin")
			remote, ok, err := workspaceSyncRemote(underInvocation(context.Background()), ws, "origin")
			if err != nil || !ok {
				t.Fatalf("workspaceSyncRemote(origin) = %v, %v", ok, err)
			}
			observed, err := advertise(context.Background(), testEnv(), ws, remote)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.WriteReceivedRefs(ws.DatabasePath, observed.record()); err != nil {
				t.Fatal(err)
			}
			return ws
		}, receiveDecisionRemoteUnmoved, "origin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			value, decisions := decidedReceive(t, c.ws(t))
			if value["receive.decision"] != c.decision || value["receive.remote"] != c.remote {
				t.Fatalf("receive.outcomes value = %v; want decision %q, remote %q", value, c.decision, c.remote)
			}
			// The error is present exactly when the receive failed, null
			// otherwise: every value carries the key. [LAW:nothing-unseen]
			if errText, present := value["receive.error"]; !present || (errText != nil) != (c.decision == "error") {
				t.Fatalf("receive.outcomes value = %v; want receive.error set exactly when the decision is error", value)
			}
			if len(decisions) != 1 || decisions[0] != c.decision {
				t.Fatalf("receive traces' decisions = %q; want the one %q", decisions, c.decision)
			}
		})
	}
}

// A receive says whether its remote answered the question of whether it moved:
// null when it did, and why when it could not tell, after which it goes on to
// fetch. [LAW:nothing-unseen]
func TestAReceiveSaysWhetherItsRemoteAnswered(t *testing.T) {
	t.Parallel()
	answered := receiveWorkspace(t, []string{"refs/heads/main"}, "origin")
	if ev, _ := receiveEvent(t, answered); ev.Facts["receive.remote_check_error"] != nil {
		t.Fatalf("receive.remote_check_error = %v; want null for a remote that answered", ev.Facts["receive.remote_check_error"])
	}
	unanswered := receiveWorkspace(t, nil, "origin")
	runGit(t, unanswered.RootDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	ev, value := receiveEvent(t, unanswered)
	if checkErr, present := ev.Facts["receive.remote_check_error"]; !present || checkErr == nil {
		t.Fatalf("receive.remote_check_error = %v (present %t); want why the remote could not be asked", checkErr, present)
	}
	if value["receive.decision"] != "error" || value["receive.error"] == nil {
		t.Fatalf("receive.outcomes value = %v; want the fetch that followed to fail with its error", value)
	}
}

// A remote holding code but no lit data answers the receive's question, and
// that answer confirms the record lit started: it is not a failing remote.
func TestAReceiveOfARemoteWithNoLitDataConfirmsIt(t *testing.T) {
	t.Parallel()
	ws := receiveWorkspace(t, []string{"refs/heads/main"}, "origin")
	origin := remoteName(t, "origin")
	if err := startFetchRecord(ws, origin); err != nil {
		t.Fatal(err)
	}
	decidedReceive(t, ws)
	if fetch, _ := readFetchRecord(testStderr(t), ws, origin, time.Now()); fetch == nil || fetch.basis != lastFetchSucceeded {
		t.Fatalf("record after the receive = %+v; want lastFetchSucceeded", fetch)
	}
}
