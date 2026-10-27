package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestLockDoltJournalExclusiveRefusesUninitializedWorkspace pins the refusal
// that keeps `lit snapshots new` honest on a workspace that was never
// initialized: the journal-lock helper contends on Dolt's lock, it never
// mints Dolt's tree. Without the refusal, the shared acquisition path's
// MkdirAll+O_CREATE would fabricate <db>/links/.dolt/noms/LOCK, the snapshot
// copy's database-dir stat would then pass, and a bogus empty "snapshot"
// would be minted.
func TestLockDoltJournalExclusiveRefusesUninitializedWorkspace(t *testing.T) {
	t.Parallel()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	release, err := LockDoltJournalExclusive(context.Background(), testStderr(), doltRoot)
	if err == nil {
		_ = release()
		t.Fatal("LockDoltJournalExclusive() succeeded on an uninitialized workspace; want a not-initialized refusal")
	}
	if !errors.Is(err, ErrWorkspaceNotInitialized) {
		t.Fatalf("LockDoltJournalExclusive() error = %v; want the not-initialized condition", err)
	}
	if _, statErr := os.Stat(doltRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the refusal fabricated %s (stat err = %v); it must create nothing", doltRoot, statErr)
	}
}

// chunkJournalPath is dolt's chunk journal for lit's database — the file a
// concurrent open's crash recovery truncates and fsyncs, and therefore the
// file a snapshot copy can capture torn. chunks.JournalFileID is dolt's own
// name for it, so this cannot drift from the engine's spelling.
func chunkJournalPath(doltRoot string) string {
	return filepath.Join(doltRoot, doltDatabaseName, ".dolt", "noms", chunks.JournalFileID)
}

// TestOpenForReadPendingMigrationUnderJournalHolder pins what a read open
// does when it must bring the schema forward while the copy's journal hold
// is live: it hands off to the write open, whose engine refuses Dolt's
// read-only fallback and waits out the holder for the bounded
// coResidentHolderWait — never applying DDL through a read engine, which is
// read-only. A holder that outlasts the budget
// surfaces as the write open's contention refusal, and the same open
// succeeds — applying the migration — once the holder releases. The budget
// is shrunk because exhaustion is this premise's certain outcome; the
// assertions are about the exhausted error's shape, not the budget's size.
func TestOpenForReadPendingMigrationUnderJournalHolder(t *testing.T) {
	// serial: no t.Parallel — rewrites the package-level
	// coResidentHolderWait budget.
	prevBudget := coResidentHolderWait
	coResidentHolderWait = 700 * time.Millisecond
	t.Cleanup(func() { coResidentHolderWait = prevBudget })

	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	openOneVersionBehind(t, ctx, doltRoot)

	release, err := LockDoltJournalExclusive(ctx, testStderr(), doltRoot)
	if err != nil {
		t.Fatalf("LockDoltJournalExclusive() error = %v", err)
	}
	reader, err := OpenForRead(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err == nil {
		_ = reader.Close()
		_ = release()
		t.Fatalf("OpenForRead() with a pending migration succeeded under the journal hold; a read-only engine cannot have applied DDL")
	}
	if !errors.Is(err, ErrWorkspaceBusy) {
		_ = release()
		t.Fatalf("OpenForRead() error = %v; want the write open's ErrWorkspaceBusy contention refusal", err)
	}
	if !strings.Contains(err.Error(), "handed off to the write open") {
		_ = release()
		t.Fatalf("OpenForRead() error = %v; want it to say why a read needed the write open", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release journal lock: %v", err)
	}

	healed, err := OpenForRead(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() after release error = %v; want the retried open to apply the migration", err)
	}
	if err := healed.Close(); err != nil {
		t.Fatalf("healed reader Close() error = %v", err)
	}
}

// TestJournalLockHoldExcludesJournalRecovery pins the contract: while
// LockDoltJournalExclusive is held (as the `lit snapshots new` copy holds it
// for its whole walk), NO concurrent open runs journal crash-recovery I/O. A
// read open never does — it opens Dolt read-only without trying the lock, so
// it reads a dirty journal and leaves it as it found it, held or not — and a
// write open, the one open that recovers a journal, cannot get past the hold.
//
// Three arms. Under the hold, a read is served and the dirty journal is
// byte-identical. With the lock free, a read still leaves it byte-identical:
// recovery is a write open's job alone. The third arm is the first two's
// detector: a write open with the lock free truncates the same dirty tail —
// otherwise the read arms pass vacuously against a journal nothing would
// have recovered anyway. [LAW:verifiable-goals]
func TestJournalLockHoldExcludesJournalRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	s, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	epic, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "journal epic", Topic: "sync", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "journal payload", Topic: "sync", ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// The shape an interrupted write leaves: a tail no record parser accepts
	// (0xFF length bytes decode as an over-max record length), which is
	// exactly what makes bootstrap's recovery truncate back to the last good
	// offset — when it is allowed to write.
	journal := chunkJournalPath(doltRoot)
	f, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open journal for dirtying: %v", err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0xFF}, 8)); err != nil {
		t.Fatalf("append dirty tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close dirtied journal: %v", err)
	}
	dirty, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("read dirtied journal: %v", err)
	}

	release, err := LockDoltJournalExclusive(ctx, testStderr(), doltRoot)
	if err != nil {
		t.Fatalf("LockDoltJournalExclusive() error = %v", err)
	}
	reader, err := OpenForRead(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		_ = release()
		t.Fatalf("OpenForRead() under the journal hold error = %v; the read-only engine must serve it", err)
	}
	if count, err := reader.LocalIssueCount(ctx); err != nil || count != 2 {
		_ = reader.Close()
		_ = release()
		t.Fatalf("LocalIssueCount() under the hold = %d, %v; want 2 (the epic and its payload), nil", count, err)
	}
	if err := reader.Close(); err != nil {
		_ = release()
		t.Fatalf("reader Close() error = %v", err)
	}
	underHold, err := os.ReadFile(journal)
	if err != nil {
		_ = release()
		t.Fatalf("read journal after held-lock open: %v", err)
	}
	if !bytes.Equal(dirty, underHold) {
		_ = release()
		t.Fatalf("journal changed under the held lock (%d -> %d bytes): the tear window links-sync-pgct.15 documents is open", len(dirty), len(underHold))
	}
	if err := release(); err != nil {
		t.Fatalf("release journal lock: %v", err)
	}

	// Lock free: a read still recovers nothing.
	reader2, err := OpenForRead(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() after release error = %v", err)
	}
	if err := reader2.Close(); err != nil {
		t.Fatalf("second reader Close() error = %v", err)
	}
	afterFreeRead, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("read journal after free-lock read: %v", err)
	}
	if !bytes.Equal(dirty, afterFreeRead) {
		t.Fatalf("journal changed under a read with the lock free (%d -> %d bytes): a read open must never recover the journal", len(dirty), len(afterFreeRead))
	}

	// Detector arm: lock free, a write open runs recovery and truncates the
	// dirty tail — proving the read arms' byte-equality was a read that writes
	// nothing, not a journal that needed no recovery.
	writer, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() after release error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer Close() error = %v", err)
	}
	recovered, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("read journal after free-lock write open: %v", err)
	}
	if bytes.Equal(dirty, recovered) {
		t.Fatalf("journal unchanged after a write open with the lock free; recovery did not run, so this test lost its detector")
	}
}
