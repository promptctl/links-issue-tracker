package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func newMutationQueueTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	return &Store{
		queuePath:       filepath.Join(root, ".links-mutation-queue.jsonl"),
		queueOffsetPath: filepath.Join(root, ".links-mutation-queue.offset"),
		queueLockPath:   filepath.Join(root, ".links-mutation-queue.lock"),
		telemetryDir:    filepath.Join(root, "telemetry"),
	}
}

func TestDrainMutationQueueAppliesPendingEntries(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	payload, err := json.Marshal(CreateIssueInput{
		Title:     "queued issue",
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("json.Marshal(CreateIssueInput) error = %v", err)
	}
	entry := mutationQueueEntry{
		ID:            "qop-drain-test",
		Operation:     mutationOperationCreateIssue,
		Payload:       payload,
		EnqueuedAt:    time.Now().UTC(),
		EnqueuedByPID: os.Getpid(),
	}
	if err := st.appendMutationQueueEntry(entry); err != nil {
		t.Fatalf("appendMutationQueueEntry() error = %v", err)
	}

	if err := st.DrainMutationQueue(ctx); err != nil {
		t.Fatalf("DrainMutationQueue() error = %v", err)
	}

	issues, err := st.ListIssues(ctx, ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("ListIssues() len = %d, want 1", len(issues))
	}
	if issues[0].Title != "queued issue" {
		t.Fatalf("ListIssues()[0].Title = %q, want %q", issues[0].Title, "queued issue")
	}
}

func TestDrainMutationQueueRetryableFailureDoesNotAdvanceOffset(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	// Hold the commit lock so the queued mutation will fail with a retryable error.
	if err := os.WriteFile(st.commitLockPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(commit lock) error = %v", err)
	}
	payload, err := json.Marshal(CreateIssueInput{
		Title:     "queued retryable",
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("json.Marshal(CreateIssueInput) error = %v", err)
	}
	entry := mutationQueueEntry{
		ID:            "qop-retryable",
		Operation:     mutationOperationCreateIssue,
		Payload:       payload,
		EnqueuedAt:    time.Now().UTC(),
		EnqueuedByPID: os.Getpid(),
	}
	if err := st.appendMutationQueueEntry(entry); err != nil {
		t.Fatalf("appendMutationQueueEntry() error = %v", err)
	}

	err = st.DrainMutationQueue(ctx)
	if err == nil {
		t.Fatal("DrainMutationQueue() error = nil, want failure")
	}

	offset, offsetErr := st.readMutationQueueOffset()
	if offsetErr != nil {
		t.Fatalf("readMutationQueueOffset() error = %v", offsetErr)
	}
	if offset != 0 {
		t.Fatalf("queue offset = %d, want 0 (retryable failure should not advance)", offset)
	}
}

func TestDrainMutationQueueNonRetryableFailureAdvancesOffset(t *testing.T) {
	st := newMutationQueueTestStore(t)
	entry := mutationQueueEntry{
		ID:            "qop-non-retryable",
		Operation:     "unsupported_operation",
		Payload:       json.RawMessage(`{}`),
		EnqueuedAt:    time.Now().UTC(),
		EnqueuedByPID: os.Getpid(),
	}
	if err := st.appendMutationQueueEntry(entry); err != nil {
		t.Fatalf("appendMutationQueueEntry() error = %v", err)
	}

	err := st.applyMutationQueueLocked(context.Background())
	if err != nil {
		t.Fatalf("applyMutationQueueLocked() error = %v, want nil for non-retryable", err)
	}
	offset, offsetErr := st.readMutationQueueOffset()
	if offsetErr != nil {
		t.Fatalf("readMutationQueueOffset() error = %v", offsetErr)
	}
	info, statErr := os.Stat(st.queuePath)
	if statErr != nil {
		t.Fatalf("Stat(queue) error = %v", statErr)
	}
	if offset != info.Size() {
		t.Fatalf("queue offset = %d, want %d", offset, info.Size())
	}
}

func TestRemoveStaleMutationQueueLockKeepsLiveOwner(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, ".links-mutation-queue.lock")
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	staleModTime := time.Now().Add(-(2 * mutationQueueLockStaleAge))
	if err := os.Chtimes(lockPath, staleModTime, staleModTime); err != nil {
		t.Fatalf("Chtimes(lock) error = %v", err)
	}
	if err := removeStaleMutationQueueLock(lockPath, mutationQueueLockStaleAge); err != nil {
		t.Fatalf("removeStaleMutationQueueLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("Stat(lock) error = %v, want lock preserved for live owner", err)
	}
}

func TestRemoveStaleMutationQueueLockRemovesDeadOwner(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, ".links-mutation-queue.lock")
	if err := os.WriteFile(lockPath, []byte("99999999\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	if err := removeStaleMutationQueueLock(lockPath, mutationQueueLockStaleAge); err != nil {
		t.Fatalf("removeStaleMutationQueueLock() error = %v", err)
	}
	_, err := os.Stat(lockPath)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(lock) error = %v, want os.ErrNotExist", err)
	}
}

func TestDrainMutationQueueDoesNotTruncateQueueFile(t *testing.T) {
	st := newMutationQueueTestStore(t)
	if err := os.MkdirAll(filepath.Dir(st.queuePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(queue dir) error = %v", err)
	}
	consumedPayload := "{}\n"
	if err := os.WriteFile(st.queuePath, []byte(consumedPayload), 0o644); err != nil {
		t.Fatalf("WriteFile(queue) error = %v", err)
	}

	if err := st.applyMutationQueueLocked(context.Background()); err != nil {
		t.Fatalf("applyMutationQueueLocked() error = %v", err)
	}
	info, err := os.Stat(st.queuePath)
	if err != nil {
		t.Fatalf("Stat(queue) error = %v", err)
	}
	if info.Size() != int64(len(consumedPayload)) {
		t.Fatalf("queue size = %d, want %d (no truncation)", info.Size(), len(consumedPayload))
	}
	offset, err := st.readMutationQueueOffset()
	if err != nil {
		t.Fatalf("readMutationQueueOffset() error = %v", err)
	}
	if offset != int64(len(consumedPayload)) {
		t.Fatalf("queue offset = %d, want %d", offset, len(consumedPayload))
	}
}
