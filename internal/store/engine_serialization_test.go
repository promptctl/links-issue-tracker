package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestConcurrentOpenWaitsForLiveWriteEngine pins the fix for
// links-sync-pgct.11. Embedded Dolt permits only one write-capable engine per
// path: before this fix, a second concurrent Open() on the same workspace
// while the first was still live failed outright with Dolt's raw "cannot
// update manifest: database is read only" (or, on a lucky timing, an
// intermittent pass) instead of simply waiting for the first to release.
// This is the exact shape of the field race: a foreground `lit new` opening
// its engine while an earlier command's on-change mirror still has its own
// engine open. The contract is behavioral — the second Open() waits on the
// first, then succeeds — and is provided by the second open's bounded retry
// against Dolt's own journal lock, which the first Store's engine holds for
// its whole lifetime. (Originally provided by a lit-minted engine flock,
// retired in links-locking-il18.3 as a partial shadow of that same lock.)
func TestConcurrentOpenWaitsForLiveWriteEngine(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	openCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		s, openErr := Open(openCtx, doltRoot, "test-workspace-id")
		if openErr != nil {
			done <- openErr
			return
		}
		done <- s.Close()
	}()

	// The second Open must actually be waiting on the first's still-live
	// engine, not racing past it or failing outright: it has to still be
	// blocked after a settle window. If it already returned (success or
	// error), this run proves nothing about the serialization contract under
	// test, so fail loudly rather than pass vacuously.
	select {
	case err := <-done:
		t.Fatalf("second Open() returned (err=%v) while the first was still open; expected it to wait on the first's live engine instead of racing or failing", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Open()/Close() error after the first released = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Open() did not complete within 5s of the first releasing — Close is not releasing the journal lock")
	}
}

// TestOpenSyncWaitsForLiveForegroundEngine reproduces the exact
// cross-type race links-sync-pgct.11 describes: a foreground mutating
// command's Store (Open) is still live when the on-change mirror's Store
// (OpenSync) tries to open its own engine on the same path. Before the fix
// these are two independent engine opens with nothing between them; both now
// contend on Dolt's own journal lock with a bounded retry, so OpenSync waits
// instead of colliding.
func TestOpenSyncWaitsForLiveForegroundEngine(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	foreground, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("foreground Open() error = %v", err)
	}

	mirrorCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		s, openErr := OpenSync(mirrorCtx, doltRoot, "test-workspace-id")
		if openErr != nil {
			done <- openErr
			return
		}
		done <- s.Close()
	}()

	select {
	case err := <-done:
		t.Fatalf("mirror OpenSync() returned (err=%v) while the foreground Store was still open; expected it to wait on the foreground's live engine", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := foreground.Close(); err != nil {
		t.Fatalf("foreground Close() error = %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("mirror OpenSync()/Close() error after the foreground released = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mirror OpenSync() did not complete within 5s of the foreground releasing")
	}
}

// TestOpenForReadDoesNotWaitForLiveWriteEngine pins the other half of the
// contract: a read open beside a live write engine keeps Dolt's read-only
// fallback and must not wait on the journal lock. Without this, every `lit
// backlog`/`lit next`/`lit show` would start serializing against the
// on-change mirror's push window for no reason — the exact
// unnecessary-latency regression write-open serialization must not
// introduce.
func TestOpenForReadDoesNotWaitForLiveWriteEngine(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	writer, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer writer.Close()

	readCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	reader, err := OpenForRead(readCtx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v while a write engine was open; read opens must not wait on the journal lock", err)
	}
	_ = reader.Close()
}
