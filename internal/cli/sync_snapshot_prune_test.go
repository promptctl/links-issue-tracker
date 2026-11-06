package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/fsperm"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

const snapshotPruneWarning = "lit: could not prune old recovery snapshots (what the reconcile landed is kept): "

// plantStuckReconcileSnapshots fills ws's store with more reconcile recovery
// snapshots than it keeps, the oldest of which cannot be removed: the next
// reconcile that lands snapshots, and its prune fails on that one.
func plantStuckReconcileSnapshots(t *testing.T, ws workspace.Info) {
	t.Helper()
	dir := dbsnapshot.DirFor(ws.DatabasePath)
	const base = int64(1700000000000000000)
	for i := int64(0); i < 12; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%d-pre-reconcile-%d", base+i, base+i))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("plant snapshot: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "held"), nil, 0o644); err != nil {
			t.Fatalf("plant snapshot file: %v", err)
		}
	}
	fsperm.Deny(t, filepath.Join(dir, fmt.Sprintf("%d-pre-reconcile-%d", base, base)), fsperm.RemoveAll)
	fsperm.RestoreTree(t, dir)
}

// requirePruneFailureReported reads the prune failure off the command's stderr
// and its event, and requires the command to have succeeded.
func requirePruneFailureReported(t *testing.T, command, stderr string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s error = %v, want the landed outcome reported without one\nstderr:\n%s", command, err, stderr)
	}
	if !strings.Contains(stderr, snapshotPruneWarning) {
		t.Errorf("%s stderr = %q, want the prune failure %q", command, stderr, snapshotPruneWarning)
	}
	prunes := lastEvent(t).Occurrences["reconcile.snapshot_prunes"]
	if len(prunes) != 1 {
		t.Fatalf("reconcile.snapshot_prunes = %v, want the one prune the landing ran", prunes)
	}
	prune, _ := prunes[0].(map[string]any)
	got, _ := prune["reconcile.snapshot_prune_error"].(string)
	if got == "" || !strings.Contains(stderr, snapshotPruneWarning+got) {
		t.Errorf("reconcile.snapshot_prune_error = %q, want the failure stderr printed", got)
	}
}

// A three-way reconcile that lands and then cannot prune reports the failure
// on its stderr and its event, and does not fail.
func TestAReconcileWhosePruneFailsReportsItOnItsEvent(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	setRunEnv(t, DisableAutoSyncEnvVar, "1")
	_, producer, consumer, _, epicID := twoClonesOverOneDoltRemote(t)
	runCLIInDir(t, producer, "new", "--title", "producer-only", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push")
	runCLIInDir(t, consumer, "new", "--title", "consumer-only", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, consumer, "sync", "fetch")
	ws, err := workspace.Resolve(runEnv(t), consumer)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	plantStuckReconcileSnapshots(t, ws)

	stdout, stderr, err := runCLIInDirSplit(t, consumer, "sync", "reconcile")
	if !strings.Contains(stdout, "linear") {
		t.Fatalf("lit sync reconcile stdout = %q, want a landed reconcile", stdout)
	}
	requirePruneFailureReported(t, "lit sync reconcile", stderr, err)
}

// A take-remote that resets and then cannot prune reports the failure on its
// stderr and its event, and does not fail.
func TestATakeRemoteWhosePruneFailsReportsItOnItsEvent(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	setRunEnv(t, DisableAutoSyncEnvVar, "1")
	consumer, ws := disjointConsumer(t)
	plantStuckReconcileSnapshots(t, ws)

	_, refusal := runCLIInDirErr(t, consumer, "sync", "reconcile", "take", "remote")
	token := regexp.MustCompile(`--owner-approved ([0-9a-f]{12})`).FindStringSubmatch(fmt.Sprint(refusal))
	if token == nil {
		t.Fatalf("take remote without approval = %v, want a refusal carrying the approval token", refusal)
	}
	stdout, stderr, err := runCLIInDirSplit(t, consumer, "sync", "reconcile", "take", "remote", "--owner-approved", token[1])
	if !strings.HasPrefix(stdout, "took remote:") {
		t.Fatalf("lit sync reconcile take remote stdout = %q, want the take reported", stdout)
	}
	requirePruneFailureReported(t, "lit sync reconcile take remote", stderr, err)
}
