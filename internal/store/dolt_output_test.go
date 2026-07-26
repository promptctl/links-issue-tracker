package store

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// captureStdout runs fn with the process stdout file descriptor redirected to a
// pipe and returns everything written to it. Dolt's EphemeralPrinter reads the
// os.Stdout package var at call time, so swapping the descriptor is the only way
// to observe what it emits — an in-process io.Writer swap would not catch it.
func captureStdout(t *testing.T, fn func() error) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	fnErr := fn()

	os.Stdout = orig
	_ = w.Close()
	captured := <-done
	_ = r.Close()

	if fnErr != nil {
		t.Fatalf("captured operation failed: %v", fnErr)
	}
	return captured
}

// TestDoltTransferKeepsStdoutClean is the regression guard for
// promptctl-sync-output-okh1: the embedded Dolt engine's chunk-download progress
// (the "N of M chunks complete" redraw, with cursor-control escapes) must never
// reach stdout, lit's parseable result channel. It exercises the two transfer
// primitives that emit it — DOLT_CLONE (init adopt) and DOLT_FETCH (sync pull) —
// and asserts stdout stays byte-empty across both.
//
// [LAW:behavior-not-structure] asserts the contract (nothing on stdout), not the
// mechanism (how dolt's output channel is routed), so a different suppression that
// still keeps stdout clean would keep this test green.
func TestDoltTransferKeepsStdoutClean(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	producer := filepath.Join(base, "producer")
	consumer := filepath.Join(base, "consumer")
	remoteURL := "file://" + filepath.Join(base, "remote")

	seedReconcileRemote(t, ctx, producer, remoteURL)

	// DOLT_CLONE — the confirmed heavy emitter (init adopt bootstraps by clone).
	cloneOut := captureStdout(t, func() error {
		return AdoptRemoteByClone(ctx, consumer, "ws", "origin", remoteURL, "master")
	})
	if len(cloneOut) != 0 {
		t.Fatalf("adopt-by-clone wrote %d bytes to stdout, want 0: %q", len(cloneOut), cloneOut)
	}

	// DOLT_FETCH — sync pull's transfer step. Push a second change from the
	// producer so the fetch has real chunks to transfer, then fetch it under
	// capture. Same CliOut seam, exercised through the pull code path.
	seedSecondChange(t, ctx, producer, remoteURL)
	sync := openSyncOrFatal(t, ctx, consumer)
	defer sync.Close()
	fetchOut := captureStdout(t, func() error {
		return sync.SyncFetch(ctx, "origin", false)
	})
	if len(fetchOut) != 0 {
		t.Fatalf("sync fetch wrote %d bytes to stdout, want 0: %q", len(fetchOut), fetchOut)
	}
}

// seedSecondChange creates and pushes another issue from an existing producer
// store so a subsequent fetch has chunks to move.
func seedSecondChange(t *testing.T, ctx context.Context, root, remoteURL string) {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(second change): %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "second", Topic: "topic", IssueType: "task"}); err != nil {
		t.Fatalf("CreateIssue(second): %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(second change): %v", err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.SyncPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncPush(second): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(second sync): %v", err)
	}
}
