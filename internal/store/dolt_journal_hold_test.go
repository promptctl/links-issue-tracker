package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dolthub/dolt/go/store/chunks"
)

// chunkJournalPath is dolt's chunk journal for lit's database — the file a
// concurrent open's crash recovery truncates and fsyncs, and therefore the
// file a snapshot copy can capture torn. chunks.JournalFileID is dolt's own
// name for it, so this cannot drift from the engine's spelling.
func chunkJournalPath(doltRoot string) string {
	return filepath.Join(doltRoot, doltDatabaseName, ".dolt", "noms", chunks.JournalFileID)
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
