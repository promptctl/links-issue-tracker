package store

import (
	"context"
	"errors"
	"slices"
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
// behind it for as long as it runs, so this deadline is the only end their wait
// has. A cut pass leaves the journal uncollected, which tells a collection that
// stopped from one that ran on past its caller; the store then takes a write.
func TestARunningCollectionStopsAtItsDeadline(t *testing.T) {
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

	cut, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	outcome, err := s.SyncCompact(cut, GCFull)
	if !errors.Is(err, context.DeadlineExceeded) || outcome.Ran {
		t.Fatalf("SyncCompact() past its deadline = %+v, %v; want a pass that did not run, cut by its deadline", outcome, err)
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
