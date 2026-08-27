package cli

import (
	"os"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The backstop runs after mutating commands, so what keeps it off every
// command's latency path is the probe interval — without it, every mutation
// would pay an engine open to ask a question whose answer is almost always
// "nothing is due". A fresh marker must block; an aged one must allow.
func TestCompactProbeIntervalGatesTheBackstop(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	now := time.Now()

	if !shouldRunNow(compactMarkerPath(ws), now, compactProbeInterval) {
		t.Fatal("a workspace that has never been probed must be allowed to probe")
	}

	if err := markRunAttempt(ws, compactMarkerPath(ws)); err != nil {
		t.Fatalf("markRunAttempt error = %v", err)
	}
	if _, err := os.Stat(compactMarkerPath(ws)); err != nil {
		t.Fatalf("marker not created: %v", err)
	}

	if shouldRunNow(compactMarkerPath(ws), now.Add(time.Second), compactProbeInterval) {
		t.Fatal("a probe inside the interval must be blocked; otherwise every mutation opens an engine")
	}
	if !shouldRunNow(compactMarkerPath(ws), now.Add(compactProbeInterval+time.Second), compactProbeInterval) {
		t.Fatal("a probe past the interval must be allowed; otherwise the store stops being maintained")
	}
}

// The marker is written BEFORE the pass, not after, which is what bounds a
// store that fails every attempt to one try per interval rather than one per
// command. A marker written only on success would leave a broken store stalling
// every mutation forever.
func TestCompactMarkerIsDistinctFromTheReceiveMarker(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}

	if compactMarkerPath(ws) == receiveMarkerPath(ws) {
		t.Fatal("compaction and receive share a debounce marker; one would silently suppress the other")
	}
}

// The mirror waits a bounded time for its spawning parent to release the
// engine, and that bound must sit above everything the parent legitimately
// schedules after the spawn — the compaction backstop included. A bound inside
// the parent's own tail abandons a mirror that owed a push, for work the parent
// was designed to do.
//
// This asserts the relationship rather than either number, so retuning any term
// stays free and only breaking the ordering fails. [LAW:behavior-not-structure]
func TestMirrorParentWaitExceedsThePostSpawnTail(t *testing.T) {
	t.Parallel()

	if parentPostSpawnTail < compactTimeout {
		t.Fatalf("parentPostSpawnTail (%s) omits compactTimeout (%s); a step that runs after the spawn must be summed into the tail",
			parentPostSpawnTail, compactTimeout)
	}
	if mirrorParentWaitTimeout <= parentPostSpawnTail {
		t.Fatalf("mirrorParentWaitTimeout (%s) is inside the parent's designed tail (%s); a healthy parent would be abandoned mid-tail",
			mirrorParentWaitTimeout, parentPostSpawnTail)
	}
}
