package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/store/chunks"
)

// TestLockDoltJournalExclusiveRefusesUninitializedWorkspace pins the refusal
// that keeps `lit snapshots new` honest on a workspace that was never
// initialized: the journal-lock helper contends on Dolt's lock, it never
// mints Dolt's tree. Without the refusal, the shared acquisition path's
// MkdirAll+O_CREATE would fabricate <db>/links/.dolt/noms/LOCK, the snapshot
// copy's database-dir stat would then pass, and a bogus empty "snapshot"
// would be minted where the command previously failed clean.
func TestLockDoltJournalExclusiveRefusesUninitializedWorkspace(t *testing.T) {
	t.Parallel()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	release, err := LockDoltJournalExclusive(context.Background(), doltRoot)
	if err == nil {
		_ = release()
		t.Fatal("LockDoltJournalExclusive() succeeded on an uninitialized workspace; want a not-initialized refusal")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("LockDoltJournalExclusive() error = %v; want the not-initialized guidance", err)
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

// TestOpenForReadPendingMigrationUnderJournalHolder pins the classified
// failure of the one interleaving the copy's journal hold cannot make safe
// for readers: a read open that must MIGRATE while the hold is live resolves
// its lazy engine into Dolt's permanent read-only fallback, and the pending
// migration's DDL then fails. The failure must surface as the
// retry-after-holder guidance (not the raw read-only line), and the same
// open must succeed — applying the migration — once the holder releases.
func TestOpenForReadPendingMigrationUnderJournalHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	versions, err := registryVersionsDescending()
	if err != nil {
		t.Fatalf("enumerate migration versions: %v", err)
	}
	if len(versions) < 2 {
		t.Fatalf("registry has %d migrations; this test needs a next-lower version to land on", len(versions))
	}
	provider, err := newGooseProvider(st.db)
	if err != nil {
		t.Fatalf("newGooseProvider() error = %v", err)
	}
	// One migration behind: the exact state OpenForRead's auto-migrate exists
	// to bring forward.
	if _, err := provider.DownTo(ctx, versions[1]); err != nil {
		t.Fatalf("DownTo(%d) error = %v", versions[1], err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	release, err := LockDoltJournalExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockDoltJournalExclusive() error = %v", err)
	}
	reader, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		_ = reader.Close()
		_ = release()
		t.Fatalf("OpenForRead() with a pending migration succeeded under the journal hold; the read-only engine cannot have applied DDL")
	}
	if !strings.Contains(err.Error(), "pending schema migrations") {
		_ = release()
		t.Fatalf("OpenForRead() error = %v; want the retry-after-holder migration guidance", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release journal lock: %v", err)
	}

	healed, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() after release error = %v; want the retried open to apply the migration", err)
	}
	if err := healed.Close(); err != nil {
		t.Fatalf("healed reader Close() error = %v", err)
	}
}

// TestJournalLockHoldExcludesJournalRecovery pins the contract that closes
// links-sync-pgct.15: while LockDoltJournalExclusive is held (as the `lit
// snapshots new` copy holds it for its whole walk), NO concurrent open runs
// journal crash-recovery I/O — not even a "read" command, because lit never
// requests a read-only dolt open; read-only is purely the journal lock's
// contention fallback, and a plain `lit backlog` on an idle store otherwise
// opens write-capable and truncates a dirty journal (measured 2026-08-20:
// 98473 -> 98457 bytes, rewritten by a read command).
//
// Two arms, and the second is the first's detector: the same dirty journal
// that stays byte-identical under the hold must actually be truncated by the
// same open once the hold is released — otherwise the first arm passes
// vacuously against a journal nothing would have recovered anyway.
// [LAW:verifiable-goals]
func TestJournalLockHoldExcludesJournalRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	s, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := s.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "journal payload", Topic: "sync"}); err != nil {
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

	release, err := LockDoltJournalExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockDoltJournalExclusive() error = %v", err)
	}
	reader, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		_ = release()
		t.Fatalf("OpenForRead() under the journal hold error = %v; the read-only fallback must serve it", err)
	}
	if count, err := reader.LocalIssueCount(ctx); err != nil || count != 1 {
		_ = reader.Close()
		_ = release()
		t.Fatalf("LocalIssueCount() under the hold = %d, %v; want 1, nil", count, err)
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

	// Detector arm: lock free, the same open runs recovery and truncates the
	// dirty tail — proving the first arm's byte-equality was exclusion, not a
	// journal that needed no recovery.
	reader2, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() after release error = %v", err)
	}
	if err := reader2.Close(); err != nil {
		t.Fatalf("second reader Close() error = %v", err)
	}
	recovered, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("read journal after free-lock open: %v", err)
	}
	if bytes.Equal(dirty, recovered) {
		t.Fatalf("journal unchanged after an open with the lock free; recovery did not run, so this test lost its detector")
	}
}
