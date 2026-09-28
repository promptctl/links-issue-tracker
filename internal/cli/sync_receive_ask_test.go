package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAutomaticReceiveAsksBeforeFetching is the end-to-end proof of the three
// DONE-WHEN arms of links-scale-om3r.3s7, for two clones over a real git
// remote driven through the real CLI, each receive being the worker's body
// run in this process (receiveNow). (a) With the remote unmoved and the debounce
// lapsed, the receive confirms the remote unmoved and fetches nothing. (b)
// After the other clone pushes a ticket, a command here still receives it.
// (c) With the remote unreachable, the question fails loud and the command
// proceeds on local data.
// [LAW:behavior-not-structure] every assertion reads a durable trace or a
// marker on disk, never how the receive reached its decision.
func TestAutomaticReceiveAsksBeforeFetching(t *testing.T) {
	remote, producer, consumer, ws := twoClonesOverOneDoltRemote(t)

	// Nothing recorded yet: the first receive cannot know the remote is
	// unmoved, so it fetches, and the record it leaves is the remote's
	// advertisement — which remote, at what URL, pointing where.
	receiveNow(t, consumer)
	first := lastReceiveTrace(t, ws)
	if first.Decision != string(storage.SyncReceiveUpToDate) {
		t.Fatalf("first receive decision = %q, want %q", first.Decision, storage.SyncReceiveUpToDate)
	}
	wantRecord := "origin " + remote + "\n" + remoteDoltHead(t, remote) + "\trefs/dolt/data\n"
	if got := string(readReceivedRefs(ws)); got != wantRecord {
		t.Fatalf("received-refs record = %q, want %q", got, wantRecord)
	}
	// The store as it stands now — first-ticket received, and the record saying
	// so — is what arm (d) restores later.
	snapshotName := strings.Fields(runCLIInDir(t, consumer, "snapshots", "new", "--label", "first-only"))[0]

	// (a) Unmoved: the question is answered with the recorded bytes, so the
	// receive confirms and fetches nothing — yet its knowledge counts as
	// refreshed, so the fetch-success marker moves and the staleness banner
	// stays quiet. The marker is aged first so "moved" is distinguishable from
	// "left alone".
	fetchedBefore := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fetchSuccessMarkerPath(ws), fetchedBefore, fetchedBefore); err != nil {
		t.Fatalf("age fetch-success marker error = %v", err)
	}
	receiveNow(t, consumer)
	unmoved := lastReceiveTrace(t, ws)
	if unmoved.Decision != receiveDecisionRemoteUnmoved || unmoved.Status != "ok" {
		t.Fatalf("unmoved receive decision/status = %q/%q, want %q/ok", unmoved.Decision, unmoved.Status, receiveDecisionRemoteUnmoved)
	}
	if unmoved.Metadata["remote"] != "origin" {
		t.Fatalf("unmoved receive metadata remote = %q, want origin", unmoved.Metadata["remote"])
	}
	if !markerModTime(t, fetchSuccessMarkerPath(ws)).After(fetchedBefore) {
		t.Fatalf("fetch-success marker did not move across an unmoved answer")
	}

	// (b) Moved: the other clone pushes, the advertisement changes, and the next
	// receive fetches and fast-forwards; its record is the new head.
	runCLIInDir(t, producer, "new", "--title", "second-ticket", "--topic", "demo", "--type", "task")
	runCLIInDir(t, producer, "sync", "push")
	receiveNow(t, consumer)
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

	// (d) Rotated: the store goes back to the first-only snapshot while the
	// remote stays where it is. The record described the directory that was
	// rotated away, so it must not survive the rotation — with it standing, the
	// unmoved remote would never be fetched again and second-ticket would stay
	// lost. The rotation forgets it, and the next receive fetches.
	runCLIInDir(t, consumer, "snapshots", "restore", snapshotName)
	if backlog := runCLIInDir(t, consumer, "backlog"); strings.Contains(backlog, "second-ticket") {
		t.Fatalf("restore did not take the consumer back to first-only:\n%s", backlog)
	}
	if got := readReceivedRefs(ws); got != nil {
		t.Fatalf("received-refs record survived a snapshot restore: %q", got)
	}
	receiveNow(t, consumer)
	restored := lastReceiveTrace(t, ws)
	if restored.Decision != string(storage.SyncReceiveFastForwarded) {
		t.Fatalf("receive after a restore decision = %q, want %q", restored.Decision, storage.SyncReceiveFastForwarded)
	}
	if backlog := runCLIInDir(t, consumer, "backlog"); !strings.Contains(backlog, "second-ticket") {
		t.Fatalf("consumer backlog missing second-ticket after the restore's receive:\n%s", backlog)
	}
	if got := string(readReceivedRefs(ws)); got != wantRecord {
		t.Fatalf("received-refs record after the restore's receive = %q, want %q", got, wantRecord)
	}

	// (c) Unreachable: the question cannot be answered, which is not "unmoved".
	// The failure is traced under its own decision, the fetch runs and fails,
	// and a command still serves local data.
	if err := os.Rename(remote, remote+".away"); err != nil {
		t.Fatalf("rename remote away error = %v", err)
	}
	t.Cleanup(func() { _ = os.Rename(remote+".away", remote) })
	tracesBefore := len(receiveTraces(t, ws))
	receiveNow(t, consumer)
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

// twoClonesOverOneDoltRemote builds a bare git remote and two lit clones of
// it: the producer, whose `first-ticket` is already pushed, and the consumer,
// initialised but not yet received. It returns the consumer's workspace.
func twoClonesOverOneDoltRemote(t *testing.T) (remote, producer, consumer string, ws workspace.Info) {
	t.Helper()
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote = filepath.Join(base, "remote.git")

	producer = filepath.Join(base, "alpha")
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

	consumer = filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	ws = workspace.Info{Location: workspace.LocationFromStorageDir(filepath.Join(consumer, ".git", "links"))}
	return remote, producer, consumer, ws
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

func markerModTime(t *testing.T, markerPath string) time.Time {
	t.Helper()
	info, err := os.Stat(markerPath)
	if err != nil {
		t.Fatalf("stat %s error = %v", filepath.Base(markerPath), err)
	}
	return info.ModTime()
}

// TestRecordReceivedWritesOnlyWhatASettledReceiveLearned pins the record's
// one writer to its contract: the advertisement observed before the fetch is
// recorded only when the receive settled cleanly and the question was
// answered, while the fetch-success marker follows the fetch alone. Each row
// is a receive outcome the inline path can produce; the assertion is what is
// on disk afterwards. [LAW:behavior-not-structure]
func TestRecordReceivedWritesOnlyWhatASettledReceiveLearned(t *testing.T) {
	observed := remoteAdvertisement{remote: "origin", url: "git@example.invalid:o/r.git", refs: "abc\trefs/dolt/data"}
	converged := &reconcileOutcome{state: storage.SyncReconcileLinearized}
	unconverged := &reconcileOutcome{state: storage.SyncReconcileProsePending}
	failed := &reconcileOutcome{err: errors.New("reconcile backend unavailable")}
	previous := []byte("origin git@example.invalid:o/r.git\nold\trefs/dolt/data\n")

	cases := []struct {
		name        string
		outcome     syncReceiveOutcome
		observed    remoteAdvertisement
		wantRecord  []byte // what received-refs.last holds afterwards
		wantFetchOK bool   // whether fetch-success.last was written
	}{
		{"up to date, answered", syncReceiveOutcome{skip: syncTargetReady, state: storage.SyncReceiveUpToDate}, observed, observed.record(), true},
		{"fast-forwarded, answered", syncReceiveOutcome{skip: syncTargetReady, state: storage.SyncReceiveFastForwarded}, observed, observed.record(), true},
		{"diverged, reconcile converged", syncReceiveOutcome{skip: syncTargetReady, state: storage.SyncReceiveDiverged, reconcile: converged}, observed, observed.record(), true},
		{"diverged, reconcile unconverged", syncReceiveOutcome{skip: syncTargetReady, state: storage.SyncReceiveDiverged, reconcile: unconverged}, observed, previous, true},
		{"diverged, reconcile failed", syncReceiveOutcome{skip: syncTargetReady, state: storage.SyncReceiveDiverged, reconcile: failed}, observed, previous, true},
		{"fetch failed", syncReceiveOutcome{skip: syncTargetReady, receiveErr: errors.New("remote unreachable")}, observed, previous, false},
		{"target skipped", syncReceiveOutcome{skip: syncTargetNoRemote}, observed, previous, false},
		{"fetched, question unanswered", syncReceiveOutcome{skip: syncTargetReady, state: storage.SyncReceiveUpToDate}, remoteAdvertisement{}, previous, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := workspace.Info{Location: workspace.LocationFromStorageDir(t.TempDir())}
			if err := store.WriteReceivedRefs(ws.DatabasePath, previous); err != nil {
				t.Fatalf("seed record error = %v", err)
			}
			recordReceived(ws, tc.outcome, tc.observed)
			if got := readReceivedRefs(ws); string(got) != string(tc.wantRecord) {
				t.Fatalf("record = %q, want %q", got, tc.wantRecord)
			}
			_, err := os.Stat(fetchSuccessMarkerPath(ws))
			if gotFetchOK := err == nil; gotFetchOK != tc.wantFetchOK {
				t.Fatalf("fetch-success marker written = %v, want %v (stat: %v)", gotFetchOK, tc.wantFetchOK, err)
			}
		})
	}
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

// TestNothingToReceiveIsOnlyAnEmptyAnswer pins the pre-clone stop: the zero
// advertisement and an empty listing end the receive, and a listing with a
// ref does not.
func TestNothingToReceiveIsOnlyAnEmptyAnswer(t *testing.T) {
	cases := []struct {
		name string
		ad   remoteAdvertisement
		want bool
	}{
		{"no remote picked", remoteAdvertisement{}, true},
		{"remote with no lit data", remoteAdvertisement{remote: "origin", url: "u", refs: ""}, true},
		{"remote with lit data", remoteAdvertisement{remote: "origin", url: "u", refs: "abc\trefs/dolt/data"}, false},
	}
	for _, c := range cases {
		if got := c.ad.nothingToReceive(); got != c.want {
			t.Errorf("%s: nothingToReceive() = %t, want %t", c.name, got, c.want)
		}
	}
}
