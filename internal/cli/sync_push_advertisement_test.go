package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAPushRecordsTheAdvertisementItLeft is links-scale-om3r.1tg's DONE-WHEN
// driven through the real CLI over a real git remote: a push this checkout
// makes (the explicit `lit sync push`, then the on-change mirror, run
// in-process) records what it left the remote advertising, so the next lapsed
// receive answers "unmoved" and fetches nothing. A push from the other clone
// still makes the next receive fetch. And the race the proof exists for (a
// peer pushing after this checkout's push, before the proof asks) yields no
// proof, because this checkout's git mirror has never seen the peer's head.
// [LAW:behavior-not-structure] every assertion reads a trace or a record on
// disk.
func TestAPushRecordsTheAdvertisementItLeft(t *testing.T) {
	remote, producer, consumer, ws := twoClonesOverOneDoltRemote(t)
	advertised := func() string {
		return "origin " + remote + "\n" + remoteDoltHead(t, remote) + "\trefs/dolt/data\n"
	}
	// Receives run only where a step asks for one; a write here must not
	// start a detached mirror of its own.
	backlogWithReceive := func() {
		t.Helper()
		t.Setenv(DisableAutoSyncEnvVar, "0")
		defer t.Setenv(DisableAutoSyncEnvVar, "1")
		runCLIInDir(t, consumer, "backlog")
	}
	receive := func() syncTraceRecord {
		t.Helper()
		lapseReceiveDebounce(t, ws)
		backlogWithReceive()
		return lastReceiveTrace(t, ws)
	}
	backlogWithReceive()

	// (a) The explicit push: it proves and records what it left, and the next
	// receive is answered without a fetch.
	runCLIInDir(t, consumer, "new", "--title", "pushed-by-hand", "--topic", "demo", "--type", "task")
	runCLIInDir(t, consumer, "sync", "push")
	if got, want := string(readReceivedRefs(ws)), advertised(); got != want {
		t.Fatalf("received-refs record after `lit sync push` = %q, want the remote's advertisement %q", got, want)
	}
	if got := lastPushTrace(t, ws).Metadata["advertisement"]; got != "proven" {
		t.Fatalf("push trace advertisement = %q, want proven", got)
	}
	if got := receive(); got.Decision != receiveDecisionRemoteUnmoved {
		t.Fatalf("receive after this checkout's own push decision = %q, want %q", got.Decision, receiveDecisionRemoteUnmoved)
	}

	// (b) The on-change mirror: it pushes from a clone and records on the
	// live store, with the same effect.
	runCLIInDir(t, consumer, "new", "--title", "pushed-by-the-mirror", "--topic", "demo", "--type", "task")
	runCLIInDir(t, consumer, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	if got, want := string(readReceivedRefs(ws)), advertised(); got != want {
		t.Fatalf("received-refs record after the mirror = %q, want the remote's advertisement %q", got, want)
	}
	if got := receive(); got.Decision != receiveDecisionRemoteUnmoved {
		t.Fatalf("receive after the mirror's push decision = %q, want %q", got.Decision, receiveDecisionRemoteUnmoved)
	}

	// (c) The race: the peer pushes after this checkout's push landed. Asked
	// now, the proof finds a head the consumer's mirror has never seen and
	// proves nothing, so the peer's push can never be recorded as received.
	runCLIInDir(t, producer, "sync", "pull")
	runCLIInDir(t, producer, "new", "--title", "pushed-by-the-peer", "--topic", "demo", "--type", "task")
	runCLIInDir(t, producer, "sync", "push")
	resolved, err := workspace.Resolve(consumer)
	if err != nil {
		t.Fatalf("resolve consumer workspace: %v", err)
	}
	session, closeStore, err := openSyncSession(context.Background(), resolved)
	if err != nil {
		t.Fatalf("open consumer sync session: %v", err)
	}
	gitRemotes, err := workspace.GitRemotes(context.Background(), resolved.RootDir)
	if err != nil {
		t.Fatalf("read consumer git remotes: %v", err)
	}
	proven, unproven := provePushedAdvertisement(context.Background(), session.syncer, resolved, "origin", gitRemotes)
	// A push to a remote the receive never asks proves nothing either: its
	// record would replace one the receive can match with one it cannot.
	_, otherRemote := provePushedAdvertisement(context.Background(), session.syncer, resolved, "backup", gitRemotes)
	if closeErr := closeStore(); closeErr != nil {
		t.Fatalf("close consumer sync session: %v", closeErr)
	}
	if proven != (remoteAdvertisement{}) || !strings.Contains(unproven, "does not hold") {
		t.Fatalf("provePushedAdvertisement() after a peer's push = %q, %q; want nothing proven because the mirror lacks the peer's head", proven.refs, unproven)
	}
	if !strings.Contains(otherRemote, "not the remote the automatic receive asks") {
		t.Fatalf("provePushedAdvertisement() for a remote the receive does not ask: unproven = %q", otherRemote)
	}

	// (d) So the peer's push still reaches this checkout on the next receive.
	if got := receive(); got.Decision != string(storage.SyncReceiveFastForwarded) {
		t.Fatalf("receive after the peer's push decision = %q, want %q", got.Decision, storage.SyncReceiveFastForwarded)
	}
	if backlog := runCLIInDir(t, consumer, "backlog"); !strings.Contains(backlog, "pushed-by-the-peer") {
		t.Fatalf("consumer backlog missing the peer's ticket after the receive:\n%s", backlog)
	}

	// (e) A superseded push proves nothing, even though its mirror holds the
	// remote's head: the rejection's re-check fetched, so the mirror can hold
	// a peer's head the store never took in. The consumer's mirror holds the
	// head now (it just fetched), so a landed push step proves and a
	// superseded one, identical in every other respect, must not.
	pushStep := func(superseded string) syncPushStep {
		return func(context.Context, string, string, bool, bool) (storage.SyncPushResult, error) {
			return storage.SyncPushResult{Head: "unused", Superseded: superseded}, nil
		}
	}
	for _, tc := range []struct {
		superseded string
		wantProven bool
	}{{"", true}, {"rejected, remote already carries HEAD", false}} {
		session, closeStore, err := openSyncSession(context.Background(), resolved)
		if err != nil {
			t.Fatalf("open consumer sync session: %v", err)
		}
		outcome, err := performSyncPush(context.Background(), context.Background(), session, resolved, "", false, false, pushStep(tc.superseded))
		if closeErr := closeStore(); closeErr != nil {
			t.Fatalf("close consumer sync session: %v", closeErr)
		}
		if err != nil {
			t.Fatalf("performSyncPush(superseded=%q) error = %v", tc.superseded, err)
		}
		if got := outcome.proven != (remoteAdvertisement{}); got != tc.wantProven {
			t.Fatalf("performSyncPush(superseded=%q) proven = %t, want %t", tc.superseded, got, tc.wantProven)
		}
	}
}

// lastPushTrace is the newest durable trace a push attempt recorded.
func lastPushTrace(t *testing.T, ws workspace.Info) syncTraceRecord {
	t.Helper()
	records := readSyncTraceRecords(t, ws)
	for i := len(records) - 1; i >= 0; i-- {
		if strings.HasPrefix(records[i].Command, "lit sync push") {
			return records[i]
		}
	}
	t.Fatalf("no push trace recorded")
	return syncTraceRecord{}
}
