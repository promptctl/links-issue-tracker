package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAutomaticReceiveAsksBeforeFetching is the end-to-end proof of the three
// DONE-WHEN arms of links-scale-om3r.3s7, driven through the real CLI for two
// clones over a real git remote. (a) With the remote unmoved and the debounce
// lapsed, the receive confirms the remote unmoved and fetches nothing. (b)
// After the other clone pushes a ticket, a command here still receives it.
// (c) With the remote unreachable, the question fails loud and the command
// proceeds on local data exactly as it did before the question existed.
// [LAW:behavior-not-structure] every assertion reads a durable trace or a
// marker on disk, never how the receive reached its decision.
func TestAutomaticReceiveAsksBeforeFetching(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	producer := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, producer, "config", "user.email", "a@a.co")
	runGit(t, producer, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	runCLIInDir(t, producer, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, producer, "new", "--title", "first-ticket", "--topic", "demo", "--type", "task")
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	ws := workspace.Info{Location: workspace.LocationFromStorageDir(filepath.Join(consumer, ".git", "links"))}
	t.Setenv(DisableAutoSyncEnvVar, "0")

	// Nothing recorded yet: the first lapsed receive cannot know the remote is
	// unmoved, so it fetches, and the record it leaves is the remote's
	// advertisement — which remote, at what URL, pointing where.
	runCLIInDir(t, consumer, "backlog")
	first := lastReceiveTrace(t, ws)
	if first.Decision != string(storage.SyncReceiveUpToDate) {
		t.Fatalf("first lapsed receive decision = %q, want %q", first.Decision, storage.SyncReceiveUpToDate)
	}
	wantRecord := "origin " + remote + "\n" + remoteDoltHead(t, remote) + "\trefs/dolt/data\n"
	if got := string(readReceivedRefs(ws)); got != wantRecord {
		t.Fatalf("received-refs record = %q, want %q", got, wantRecord)
	}

	// (a) Unmoved: the question is answered with the recorded bytes, so the
	// receive confirms and fetches nothing — yet its knowledge counts as
	// refreshed, so the fetch-success marker moves and the staleness banner
	// stays quiet.
	fetchedBefore := markerModTime(t, fetchSuccessMarkerPath(ws))
	lapseReceiveDebounce(t, ws)
	runCLIInDir(t, consumer, "backlog")
	unmoved := lastReceiveTrace(t, ws)
	if unmoved.Decision != receiveDecisionRemoteUnmoved || unmoved.Status != "ok" {
		t.Fatalf("unmoved receive decision/status = %q/%q, want %q/ok", unmoved.Decision, unmoved.Status, receiveDecisionRemoteUnmoved)
	}
	if unmoved.Metadata["remote"] != "origin" {
		t.Fatalf("unmoved receive metadata remote = %q, want origin", unmoved.Metadata["remote"])
	}
	if markerModTime(t, fetchSuccessMarkerPath(ws)).Before(fetchedBefore) {
		t.Fatalf("fetch-success marker moved backwards across an unmoved answer")
	}

	// (b) Moved: the other clone pushes, the advertisement changes, and the next
	// lapsed receive fetches and fast-forwards; its record is the new head.
	runCLIInDir(t, producer, "new", "--title", "second-ticket", "--topic", "demo", "--type", "task")
	runCLIInDir(t, producer, "sync", "push")
	lapseReceiveDebounce(t, ws)
	runCLIInDir(t, consumer, "backlog")
	moved := lastReceiveTrace(t, ws)
	if moved.Decision != string(storage.SyncReceiveFastForwarded) {
		t.Fatalf("receive after the remote moved decision = %q, want %q", moved.Decision, storage.SyncReceiveFastForwarded)
	}
	if backlog := runCLIInDir(t, consumer, "backlog"); !strings.Contains(backlog, "second-ticket") {
		t.Fatalf("consumer backlog missing second-ticket after the remote moved:\n%s", backlog)
	}
	wantRecord = "origin " + remote + "\n" + remoteDoltHead(t, remote) + "\trefs/dolt/data\n"
	if got := string(readReceivedRefs(ws)); got != wantRecord {
		t.Fatalf("received-refs record after fast-forward = %q, want %q", got, wantRecord)
	}

	// (c) Unreachable: the question cannot be answered, which is not "unmoved".
	// The failure is traced under its own decision, the fetch runs and fails
	// as it always did, and the command itself still serves local data.
	if err := os.Rename(remote, remote+".away"); err != nil {
		t.Fatalf("rename remote away error = %v", err)
	}
	t.Cleanup(func() { _ = os.Rename(remote+".away", remote) })
	tracesBefore := len(receiveTraces(t, ws))
	lapseReceiveDebounce(t, ws)
	if backlog := runCLIInDir(t, consumer, "backlog"); !strings.Contains(backlog, "second-ticket") {
		t.Fatalf("consumer backlog lost local data while the remote was unreachable:\n%s", backlog)
	}
	traces := receiveTraces(t, ws)[tracesBefore:]
	if len(traces) != 2 {
		t.Fatalf("unreachable remote left %d receive traces, want 2 (check failed, then fetch error): %+v", len(traces), traces)
	}
	if traces[0].Decision != receiveDecisionRemoteCheckFailed || traces[0].Status != "error" {
		t.Fatalf("first trace decision/status = %q/%q, want %q/error", traces[0].Decision, traces[0].Status, receiveDecisionRemoteCheckFailed)
	}
	if traces[1].Decision != "error" {
		t.Fatalf("second trace decision = %q, want error (the fetch ran and failed)", traces[1].Decision)
	}
	if got := string(readReceivedRefs(ws)); got != wantRecord {
		t.Fatalf("a failed receive rewrote the received-refs record: %q", got)
	}
}

// receiveTraces returns the durable sync traces the automatic receive wrote,
// in chronological order.
func receiveTraces(t *testing.T, ws workspace.Info) []syncTraceRecord {
	t.Helper()
	var out []syncTraceRecord
	for _, record := range readSyncTraceRecords(t, ws) {
		if record.Command == receiveTraceCommand {
			out = append(out, record)
		}
	}
	return out
}

func lastReceiveTrace(t *testing.T, ws workspace.Info) syncTraceRecord {
	t.Helper()
	traces := receiveTraces(t, ws)
	if len(traces) == 0 {
		t.Fatalf("no automatic receive trace recorded")
	}
	return traces[len(traces)-1]
}

// lapseReceiveDebounce ages the receive debounce marker past its interval so
// the next command's receive is due, without waiting for the interval.
func lapseReceiveDebounce(t *testing.T, ws workspace.Info) {
	t.Helper()
	lapsed := time.Now().Add(-2 * receiveDebounceInterval)
	if err := os.Chtimes(receiveMarkerPath(ws), lapsed, lapsed); err != nil {
		t.Fatalf("age receive marker error = %v", err)
	}
}

func markerModTime(t *testing.T, markerPath string) time.Time {
	t.Helper()
	info, err := os.Stat(markerPath)
	if err != nil {
		t.Fatalf("stat %s error = %v", filepath.Base(markerPath), err)
	}
	return info.ModTime()
}

// remoteDoltHead is the object the bare remote's refs/dolt/data points at —
// read from the remote itself, so the expected record is the territory and not
// a second copy of the code's derivation.
func remoteDoltHead(t *testing.T, bareRemote string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", bareRemote, "rev-parse", "refs/dolt/data")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse refs/dolt/data on %s error = %v", bareRemote, err)
	}
	return strings.TrimSpace(string(out))
}
