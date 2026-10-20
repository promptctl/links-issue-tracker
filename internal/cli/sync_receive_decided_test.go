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

// decidedReceive runs one automatic receive (receiveOnce) on ws and returns
// the one value it put on receive.outcomes and the decisions of the receive
// traces it left.
func decidedReceive(t *testing.T, ws workspace.Info) (map[string]any, []string) {
	t.Helper()
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	receiveOnce(ctx, ws)
	outcomes := writtenEvent(t, recorder.Finish(invocationEnd(time.Now(), nil, nil))).Occurrences["receive.outcomes"]
	if len(outcomes) != 1 {
		t.Fatalf("receive.outcomes = %#v; want the one receive's value", outcomes)
	}
	value, _ := outcomes[0].(map[string]any)
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
	return workspace.Info{Location: workspace.LocationFromStorageDir(filepath.Join(base, "storage")), RootDir: root}
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
		remote   any
	}{
		{"no git remote", func(t *testing.T) workspace.Info {
			return receiveWorkspace(t, nil)
		}, string(syncTargetNoRemote), nil},
		{"several remotes and no upstream", func(t *testing.T) workspace.Info {
			return receiveWorkspace(t, []string{"refs/dolt/data"}, "origin", "backup")
		}, string(syncTargetNoRemote), nil},
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
			gitRemotes, err := workspace.GitRemotes(context.Background(), ws.RootDir)
			if err != nil {
				t.Fatal(err)
			}
			observed, err := advertise(context.Background(), ws, remoteName(t, "origin"), gitRemotes)
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
				t.Fatalf("receive.outcomes value = %v; want decision %q, remote %v", value, c.decision, c.remote)
			}
			if len(decisions) != 1 || decisions[0] != c.decision {
				t.Fatalf("receive traces' decisions = %q; want the one %q", decisions, c.decision)
			}
		})
	}
}
