package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A pass on an engine that has reached no remote is one hold of Dolt's LOCK
// from open to close. A writer that could take LOCK between the pass and its
// re-open would wait on the commit lock the pass still held, and every writer
// behind it would stand still with it and fail.
func TestAPassOnAnEngineThatReachedNoRemoteNeverLetsGoOfLOCK(t *testing.T) {
	t.Parallel()
	for _, mode := range []GCMode{GCNewGen, GCFull} {
		reports := &heldReports{}
		ctx := WithLockObserver(context.Background(), reports.observer())
		s, err := OpenMaintenance(ctx, testEnv(), testStderr(), migratedDoltDir(t), "test-workspace-id")
		if err != nil {
			t.Fatalf("OpenMaintenance() error = %v", err)
		}
		commitPayload(t, ctx, s, "before")
		if err := s.checkoutNewAt(ctx, "elsewhere", headCommit(t, ctx, s)); err != nil {
			t.Fatal(err)
		}

		outcome, err := s.SyncCompact(ctx, mode)
		if err != nil || !outcome.Ran {
			t.Fatalf("SyncCompact(%v) = %+v, %v; want a pass that ran", mode, outcome, err)
		}

		if branch, err := activeBranch(ctx, s.db); err != nil || branch != "elsewhere" {
			t.Fatalf("active branch after the pass = %q (%v); want elsewhere, the session's branch", branch, err)
		}
		commitPayload(t, ctx, s, "after")
		passes, looked := s.CompactionPasses()
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		want := []storage.CompactionPass{{Depth: mode, Ran: true, Rotation: storage.CompactionRotationConnection}}
		if !looked || !slices.Equal(passes, want) {
			t.Fatalf("CompactionPasses() = %+v, %t; want %+v, true", passes, looked, want)
		}
		holds, _ := reports.snapshot()
		if len(holds) != 1 || !holds[0].Routine {
			t.Fatalf("holds = %+v, want the one routine hold of the maintenance open", holds)
		}
	}
}

// A push leaves its remote open until the engine closes, so a pass after one
// closes the engine: replacing the connection would leave the sweep to remove
// the push's live refs as orphans, and the next push would fail reading them.
// The rotation clears the fact, so the next pass on the new engine keeps it.
func TestAPassAfterAPushRotatesTheEngineAndTheNextPushLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot, _, _ := pushedMirror(t)
	session, err := OpenSync(ctx, testEnv(), testStderr(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer session.Close()
	origin := remoteName(t, "origin")

	commitPayload(t, ctx, session, "c2")
	if _, err := session.SyncPush(ctx, origin, "master", false, false); err != nil {
		t.Fatalf("SyncPush() error = %v", err)
	}
	for _, mode := range []GCMode{GCNewGen, GCFull} {
		if _, err := session.SyncCompact(ctx, mode); err != nil {
			t.Fatalf("SyncCompact(%v) error = %v", mode, err)
		}
	}
	commitPayload(t, ctx, session, "c3")
	if _, err := session.SyncPush(ctx, origin, "master", false, false); err != nil {
		t.Fatalf("SyncPush() after the passes error = %v", err)
	}

	passes, _ := session.CompactionPasses()
	want := []storage.CompactionPass{
		{Depth: GCNewGen, Ran: true, Rotation: storage.CompactionRotationEngine},
		{Depth: GCFull, Ran: true, Rotation: storage.CompactionRotationConnection},
	}
	if !slices.Equal(passes, want) {
		t.Fatalf("CompactionPasses() = %+v, want %+v", passes, want)
	}
}

// The backstop's hold is routine because its pass ends on its own: writers wait
// behind it for as long as it runs, so this deadline is what ends their wait
// while a collection marks. Doubling it walks the cut from before DOLT_GC starts,
// which proves nothing, into marking, which Dolt's error names by its SaveHashes
// step. A Dolt that lost the cut runs the pass to its end here instead. A cut
// pass leaves the journal uncollected, and the store then takes a write.
func TestADeadlineCutsACollectionWhileItMarks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	s, err := OpenMaintenance(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenMaintenance() error = %v", err)
	}
	commitPayload(t, ctx, s, "before")
	before, err := s.measureCompactionSignals()
	if err != nil {
		t.Fatal(err)
	}

	err = nil
	for deadline := time.Millisecond; err == nil || !strings.Contains(err.Error(), "SaveHashes"); deadline *= 2 {
		cut, cancel := context.WithTimeout(ctx, deadline)
		var outcome storage.CompactionOutcome
		outcome, err = s.SyncCompact(cut, GCFull)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || outcome.Ran {
			t.Fatalf("SyncCompact() under %s = %+v, %v; want a pass that did not run, cut by its deadline", deadline, outcome, err)
		}
	}
	after, err := s.measureCompactionSignals()
	if err != nil {
		t.Fatal(err)
	}
	if after.JournalBytes < before.JournalBytes {
		t.Fatalf("journal went from %d to %d bytes: the collection ran on past its deadline", before.JournalBytes, after.JournalBytes)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := OpenMaintenance(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("reopen after the cut error = %v", err)
	}
	defer reopened.Close()
	commitPayload(t, ctx, reopened, "after")
}

// A deadline or a cancel can land after DOLT_GC returned, when the store is
// rewritten and its connection poisoned. Nothing is left to cut: whichever way
// the pass rotates, the connection still has to come back, or the pass reads as
// failed and every statement after it fails.
func TestACancelAfterTheCollectionStillRotates(t *testing.T) {
	t.Parallel()
	for _, want := range []storage.CompactionRotation{storage.CompactionRotationConnection, storage.CompactionRotationEngine} {
		ctx := context.Background()
		s, err := OpenMaintenance(ctx, testEnv(), testStderr(), migratedDoltDir(t), "test-workspace-id")
		if err != nil {
			t.Fatalf("OpenMaintenance() error = %v", err)
		}
		defer s.Close()
		commitPayload(t, ctx, s, "before")
		s.reachedRemote = want == storage.CompactionRotationEngine
		if err := s.ExecRawForTest(ctx, "CALL DOLT_GC()"); err != nil {
			t.Fatalf("DOLT_GC error = %v", err)
		}

		past, cancel := context.WithCancel(ctx)
		cancel()
		if got, err := s.rotateAfterGC(past); got != want || err != nil {
			t.Fatalf("rotateAfterGC() past the cancel = %q, %v; want %q", got, err, want)
		}
		commitPayload(t, ctx, s, "after")
	}
}

// A rotation that failed left no usable connection, so the pass says none: the
// event is what an operator reads to learn whether the store recovered.
func TestAFailedRotationIsNotReportedAsOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := OpenMaintenance(ctx, testEnv(), testStderr(), migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenMaintenance() error = %v", err)
	}
	defer s.Close()
	if err := s.db.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got, err := s.rotateAfterGC(ctx); got != "" || err == nil {
		t.Fatalf("rotateAfterGC() on a closed pool = %q, %v; want no rotation and the failure", got, err)
	}
}

// A store that looked and owed nothing says so, a store that could not measure
// did not look, and a store nothing asked has not looked: those are three
// different facts, and the event's [] and absent key depend on keeping them.
func TestCompactionPassesTellALookFromNoLook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := OpenMaintenance(ctx, testEnv(), testStderr(), migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenMaintenance() error = %v", err)
	}
	defer s.Close()
	if passes, looked := s.CompactionPasses(); looked || len(passes) != 0 {
		t.Fatalf("CompactionPasses() on a fresh store = %+v, %t; want none, no look", passes, looked)
	}
	outcome, err := s.MaintainIfDue(ctx)
	if err != nil || outcome.Compaction.Ran {
		t.Fatalf("MaintainIfDue() on a small store = %+v, %v; want nothing owed", outcome, err)
	}
	if passes, looked := s.CompactionPasses(); !looked || len(passes) != 0 {
		t.Fatalf("CompactionPasses() after a look that owed nothing = %+v, %t; want none, looked", passes, looked)
	}
}
