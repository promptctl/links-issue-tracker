package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/fslock"
)

// journalLockPath is where dolt's journaling store takes its exclusive
// manifest lock for lit's database: <doltRoot>/links/.dolt/noms/LOCK. Holding
// it with an independent fd is exactly what a foreign process's still-open
// engine looks like to a new open — flock conflicts are per open file
// description, so an in-test holder is indistinguishable from another process.
func journalLockPath(doltRoot string) string {
	return filepath.Join(doltRoot, doltDatabaseName, ".dolt", "noms", "LOCK")
}

// TestOpenFailsLoudWhenForeignEngineHoldsJournalLock pins the write-open
// contract at the heart of links-sync-pgct.11: when another process's engine
// holds the journal manifest lock past lit's own engine-flock handoff (the
// teardown straggler window), a write-capable open must fail loudly and
// boundedly — never silently hand back a read-only store whose every later
// write dies as "cannot update manifest: database is read only" with no
// in-process cure. The deterministic foreign holder makes the collision
// certain instead of timing-dependent. [LAW:no-silent-failure]
// [LAW:no-ambient-temporal-coupling]
func TestOpenFailsLoudWhenForeignEngineHoldsJournalLock(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lock := fslock.New(journalLockPath(doltRoot))
	if err := lock.TryLock(); err != nil {
		t.Fatalf("take journal lock as foreign holder: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	prevBudget := engineOpenRetryMaxElapsed
	engineOpenRetryMaxElapsed = 700 * time.Millisecond
	t.Cleanup(func() { engineOpenRetryMaxElapsed = prevBudget })

	start := time.Now()
	opened, err := Open(ctx, doltRoot, "test-workspace-id")
	elapsed := time.Since(start)
	if err == nil {
		_ = opened.Close()
		t.Fatalf("Open() succeeded against a foreign journal-lock holder; want a loud bounded failure")
	}
	if !errors.Is(err, nbs.ErrDatabaseLocked) {
		t.Fatalf("Open() error = %v; want the nbs.ErrDatabaseLocked contention classification to survive the chain", err)
	}
	// Bounded: the shrunken budget (plus dolt's own per-attempt lock waits)
	// must not balloon toward the production 30s.
	if elapsed > 10*time.Second {
		t.Fatalf("Open() took %s to fail against a held journal lock; the retry budget is not bounding the wait", elapsed)
	}
}

// TestOpenRecoversOnceForeignJournalHolderReleases pins the recovery half of
// the same contract: the bounded open-retry absorbs a holder that releases
// mid-wait, so the caller sees a working writable store, not an error and not
// a read-only one. The release happens while Open is already retrying; the
// retry loop — not test timing — owns the reconciliation.
func TestOpenRecoversOnceForeignJournalHolderReleases(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lock := fslock.New(journalLockPath(doltRoot))
	if err := lock.TryLock(); err != nil {
		t.Fatalf("take journal lock as foreign holder: %v", err)
	}
	release := time.AfterFunc(300*time.Millisecond, func() { _ = lock.Unlock() })
	defer release.Stop()

	reopened, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() after holder release error = %v; want the open retry to absorb the handoff", err)
	}
	defer reopened.Close()
	// The store must be genuinely writable — the whole point of failing fast
	// instead of accepting dolt's silent read-only fallback.
	created, err := reopened.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "post-handoff write works", Topic: "sync"})
	if err != nil {
		t.Fatalf("CreateIssue() on the recovered store error = %v", err)
	}
	if created.ID == "" {
		t.Fatalf("CreateIssue() returned an empty id")
	}
}

// TestOpenSyncContentionCarriesWorkspaceBusy pins the mirror-facing half of
// the write-open contention contract: when OpenSync times out against a
// foreign journal-lock holder, the error must carry ErrWorkspaceBusy — the
// uniform contention sentinel pushOutcomeOf maps to the non-failed
// workspace_busy outcome — alongside the ErrDatabaseLocked classification.
// The retired engine lock's wrapper carried the sentinel; without it a
// mirror blocked behind a healthy long-running writer records a push
// FAILURE and pages the owner over ordinary serialization.
func TestOpenSyncContentionCarriesWorkspaceBusy(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lock := fslock.New(journalLockPath(doltRoot))
	if err := lock.TryLock(); err != nil {
		t.Fatalf("take journal lock as foreign holder: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	prevBudget := engineOpenRetryMaxElapsed
	engineOpenRetryMaxElapsed = 700 * time.Millisecond
	t.Cleanup(func() { engineOpenRetryMaxElapsed = prevBudget })

	opened, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		_ = opened.Close()
		t.Fatalf("OpenSync() succeeded against a foreign journal-lock holder; want bounded contention")
	}
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("OpenSync() error = %v; want the ErrWorkspaceBusy contention sentinel in the chain", err)
	}
	if !errors.Is(err, nbs.ErrDatabaseLocked) {
		t.Fatalf("OpenSync() error = %v; want the nbs.ErrDatabaseLocked classification preserved alongside the sentinel", err)
	}
}

// TestOpenForReadToleratesForeignJournalHolder pins the read-open contract the
// write fix must NOT disturb: a read open beside a live foreign writer keeps
// dolt's read-only fallback and serves reads — reading a store someone else is
// writing is exactly what a read open is for.
func TestOpenForReadToleratesForeignJournalHolder(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if _, err := s.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "visible to readers", Topic: "sync"}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lock := fslock.New(journalLockPath(doltRoot))
	if err := lock.TryLock(); err != nil {
		t.Fatalf("take journal lock as foreign holder: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	reader, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() beside a journal-lock holder error = %v; want the read-only fallback to serve it", err)
	}
	defer reader.Close()
	count, err := reader.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount() on fallback reader error = %v", err)
	}
	if count != 1 {
		t.Fatalf("LocalIssueCount() = %d, want 1", count)
	}
}
