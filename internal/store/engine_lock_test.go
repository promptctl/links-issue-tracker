package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestEngineWriteLockSerializesConcurrentOpen pins the fix for
// links-sync-pgct.11. Embedded Dolt permits only one read-write engine per
// path: before this fix, a second concurrent Open() on the same workspace
// while the first was still live failed outright with Dolt's raw "cannot
// update manifest: database is read only" (or, on a lucky timing, an
// intermittent pass) instead of simply waiting for the first to release.
// This is the exact shape of the field race: a foreground `lit new` opening
// its engine while an earlier command's on-change mirror still has its own
// engine open. The fix makes the second Open() block on the first, not fail.
func TestEngineWriteLockSerializesConcurrentOpen(t *testing.T) {
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
		t.Fatalf("second Open() returned (err=%v) while the first was still open; expected it to block on the engine-write lock instead of racing or failing", err)
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
		t.Fatal("second Open() did not complete within 5s of the first releasing — the engine lock is not being released on Close")
	}
}

// TestEngineWriteLockSerializesOpenAgainstOpenSync reproduces the exact
// cross-type race links-sync-pgct.11 describes: a foreground mutating
// command's Store (Open) is still live when the on-change mirror's Store
// (OpenSync) tries to open its own engine on the same path. Before the fix
// these are two independent engine opens with nothing between them; the fix
// routes both through the same engine-write lock, so OpenSync waits instead
// of colliding.
func TestEngineWriteLockSerializesOpenAgainstOpenSync(t *testing.T) {
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
		t.Fatalf("mirror OpenSync() returned (err=%v) while the foreground Store was still open; expected it to block on the engine-write lock", err)
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

// TestOpenForReadDoesNotWaitForEngineLock pins the other half of the
// contract: a read-only open does not conflict with a live read-write
// engine, so it must not be routed through the engine-write lock. Without
// this, every `lit backlog`/`lit next`/`lit show` would start serializing
// against the on-change mirror's push window for no reason — the exact
// unnecessary-latency regression the engine lock must not introduce.
func TestOpenForReadDoesNotWaitForEngineLock(t *testing.T) {
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
		t.Fatalf("OpenForRead() error = %v while a read-write engine was open; read-only opens must not wait on the engine-write lock", err)
	}
	_ = reader.Close()
}
