package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bmf/links-issue-tracker/internal/model"
)

const (
	mutationQueueTriggerRead  = "read"
	mutationQueueTriggerWrite = "write"
	// [LAW:no-mode-explosion] Queue apply timeout is a single bounded policy for all queue-driven mutation attempts.
	mutationQueueApplyTimeout = 300 * time.Millisecond
	mutationQueueLockStaleAge = 30 * time.Second
)

const (
	mutationOperationRecordSyncState = "record_sync_state"
	mutationOperationCreateIssue     = "create_issue"
	mutationOperationUpdateIssue     = "update_issue"
	mutationOperationAddComment      = "add_comment"
	mutationOperationAddRelation     = "add_relation"
	mutationOperationRemoveRelation  = "remove_relation"
	mutationOperationImportIssue     = "import_issue"
	mutationOperationImportComment   = "import_comment"
	mutationOperationImportRelation  = "import_relation"
	mutationOperationImportLabel     = "import_label"
	mutationOperationAddLabel        = "add_label"
	mutationOperationRemoveLabel     = "remove_label"
	mutationOperationReplaceLabels   = "replace_labels"
	mutationOperationTransitionIssue = "transition_issue"
	mutationOperationSetParent       = "set_parent"
	mutationOperationClearParent     = "clear_parent"
	mutationOperationReplaceExport   = "replace_from_export"
	mutationOperationFsck            = "fsck"
)

type mutationQueueContextKey struct{}

type mutationQueueResult struct {
	value any
	err   error
}

type mutationQueueEntry struct {
	ID            string          `json:"id"`
	Operation     string          `json:"operation"`
	Payload       json.RawMessage `json:"payload"`
	EnqueuedAt    time.Time       `json:"enqueued_at"`
	EnqueuedByPID int             `json:"enqueued_by_pid"`
}

type MutationQueuedError struct {
	OperationID string
	Operation   string
}

func (e MutationQueuedError) Error() string {
	return fmt.Sprintf("mutation queued for async apply: operation=%s operation_id=%s", e.Operation, e.OperationID)
}

type updateIssueQueuePayload struct {
	ID    string           `json:"id"`
	Input UpdateIssueInput `json:"input"`
}

type removeRelationQueuePayload struct {
	SrcID   string `json:"src_id"`
	DstID   string `json:"dst_id"`
	RelType string `json:"type"`
}

type removeLabelQueuePayload struct {
	IssueID   string `json:"issue_id"`
	LabelName string `json:"label_name"`
}

type replaceLabelsQueuePayload struct {
	IssueID   string   `json:"issue_id"`
	Labels    []string `json:"labels"`
	CreatedBy string   `json:"created_by"`
}

type clearParentQueuePayload struct {
	ChildID string `json:"child_id"`
}

type fsckQueuePayload struct {
	Repair bool `json:"repair"`
}

func mutationQueueBypass(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	bypass, _ := ctx.Value(mutationQueueContextKey{}).(bool)
	return bypass
}

func (s *Store) syncQueueBeforeRead(ctx context.Context) error {
	if mutationQueueBypass(ctx) {
		return nil
	}
	if err := s.applyMutationQueue(ctx, mutationQueueTriggerRead); err != nil {
		_ = s.writeMutationQueueTelemetry(map[string]any{
			"event": "queue_read_apply_error",
			"error": err.Error(),
		})
	}
	return nil
}

func enqueueMutationAndApply[T any](ctx context.Context, s *Store, operation string, payload any) (T, error) {
	var zero T
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return zero, fmt.Errorf("encode queued mutation payload (%s): %w", operation, err)
	}
	entry := mutationQueueEntry{
		ID:            "qop-" + uuid.NewString(),
		Operation:     operation,
		Payload:       encodedPayload,
		EnqueuedAt:    time.Now().UTC(),
		EnqueuedByPID: os.Getpid(),
	}
	if err := s.appendMutationQueueEntry(entry); err != nil {
		return zero, err
	}
	applyErr := s.applyMutationQueue(ctx, mutationQueueTriggerWrite)
	result, ok := s.takeMutationQueueResult(entry.ID)
	if ok {
		if result.err != nil {
			return zero, result.err
		}
		if result.value == nil {
			return zero, nil
		}
		if typed, typedOK := result.value.(T); typedOK {
			return typed, nil
		}
		encodedResult, err := json.Marshal(result.value)
		if err != nil {
			return zero, fmt.Errorf("encode queued mutation result (%s): %w", operation, err)
		}
		var decoded T
		if err := json.Unmarshal(encodedResult, &decoded); err != nil {
			return zero, fmt.Errorf("decode queued mutation result (%s): %w", operation, err)
		}
		return decoded, nil
	}
	if applyErr != nil {
		return zero, MutationQueuedError{OperationID: entry.ID, Operation: operation}
	}
	return zero, MutationQueuedError{OperationID: entry.ID, Operation: operation}
}

func (s *Store) appendMutationQueueEntry(entry mutationQueueEntry) error {
	if err := os.MkdirAll(filepath.Dir(s.queuePath), 0o755); err != nil {
		return fmt.Errorf("create queue dir: %w", err)
	}
	encodedEntry, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode queue entry: %w", err)
	}
	file, err := os.OpenFile(s.queuePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open mutation queue: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(encodedEntry, '\n')); err != nil {
		return fmt.Errorf("append mutation queue entry: %w", err)
	}
	return nil
}

func (s *Store) applyMutationQueue(ctx context.Context, trigger string) error {
	if mutationQueueBypass(ctx) {
		return nil
	}
	release, acquired, err := tryAcquireNonBlockingMutationQueueLock(s.queueLockPath)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer release()

	applyCtx, cancel := context.WithTimeout(context.WithValue(ctx, mutationQueueContextKey{}, true), mutationQueueApplyTimeout)
	defer cancel()
	if err := s.applyMutationQueueLocked(applyCtx); err != nil {
		_ = s.writeMutationQueueTelemetry(map[string]any{
			"event":   "queue_apply_error",
			"trigger": trigger,
			"error":   err.Error(),
		})
		return err
	}
	return nil
}

func tryAcquireNonBlockingMutationQueueLock(lockPath string) (func(), bool, error) {
	acquire := func() (func(), bool, error) {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return func() {}, false, nil
			}
			return nil, false, fmt.Errorf("acquire mutation queue lock: %w", err)
		}
		_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
		if closeErr := file.Close(); closeErr != nil {
			_ = os.Remove(lockPath)
			return nil, false, fmt.Errorf("close mutation queue lock: %w", closeErr)
		}
		return func() {
			_ = os.Remove(lockPath)
		}, true, nil
	}

	release, acquired, err := acquire()
	if err != nil || acquired {
		return release, acquired, err
	}
	if staleErr := removeStaleMutationQueueLock(lockPath, mutationQueueLockStaleAge); staleErr != nil {
		return nil, false, staleErr
	}
	return acquire()
}

func removeStaleMutationQueueLock(lockPath string, staleAfter time.Duration) error {
	info, err := os.Stat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat mutation queue lock: %w", err)
	}
	if time.Since(info.ModTime()) <= staleAfter {
		running, knownOwner, runErr := commitLockOwnerRunning(lockPath)
		if runErr != nil {
			return runErr
		}
		if !knownOwner || running {
			return nil
		}
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale mutation queue lock: %w", err)
	}
	return nil
}

func (s *Store) applyMutationQueueLocked(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.queuePath), 0o755); err != nil {
		return fmt.Errorf("create queue dir: %w", err)
	}
	offset, err := s.readMutationQueueOffset()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(s.queuePath, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open mutation queue for read: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat mutation queue: %w", err)
	}
	if offset < 0 || offset > info.Size() {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek mutation queue: %w", err)
	}
	reader := bufio.NewReader(file)
	currentOffset := offset

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if len(line) == 0 {
					break
				}
				// Keep trailing partial line for the next pass.
				break
			}
			return fmt.Errorf("read mutation queue: %w", readErr)
		}
		nextOffset := currentOffset + int64(len(line))
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			currentOffset = nextOffset
			if err := s.writeMutationQueueOffset(currentOffset); err != nil {
				return err
			}
			continue
		}

		var entry mutationQueueEntry
		if err := json.Unmarshal([]byte(trimmed), &entry); err != nil {
			_ = s.writeMutationQueueTelemetry(map[string]any{
				"event":      "queue_entry_parse_error",
				"offset":     currentOffset,
				"raw_line":   trimmed,
				"error":      err.Error(),
				"queue_path": s.queuePath,
			})
			currentOffset = nextOffset
			if offsetErr := s.writeMutationQueueOffset(currentOffset); offsetErr != nil {
				return offsetErr
			}
			continue
		}
		value, applyErr := s.applyMutationQueueEntry(ctx, entry)
		s.storeMutationQueueResult(entry.ID, mutationQueueResult{value: value, err: applyErr})
		if applyErr != nil {
			_ = s.writeMutationQueueTelemetry(map[string]any{
				"event":      "queue_entry_apply_error",
				"entry_id":   entry.ID,
				"operation":  entry.Operation,
				"error":      applyErr.Error(),
				"queue_path": s.queuePath,
			})
			currentOffset = nextOffset
			if offsetErr := s.writeMutationQueueOffset(currentOffset); offsetErr != nil {
				return offsetErr
			}
			continue
		}

		currentOffset = nextOffset
		if err := s.writeMutationQueueOffset(currentOffset); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) applyMutationQueueEntry(ctx context.Context, entry mutationQueueEntry) (any, error) {
	switch entry.Operation {
	case mutationOperationRecordSyncState:
		var payload SyncState
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.RecordSyncState(ctx, payload)
	case mutationOperationCreateIssue:
		var payload CreateIssueInput
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.CreateIssue(ctx, payload)
	case mutationOperationUpdateIssue:
		var payload updateIssueQueuePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.UpdateIssue(ctx, payload.ID, payload.Input)
	case mutationOperationAddComment:
		var payload AddCommentInput
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.AddComment(ctx, payload)
	case mutationOperationAddRelation:
		var payload AddRelationInput
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.AddRelation(ctx, payload)
	case mutationOperationRemoveRelation:
		var payload removeRelationQueuePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.RemoveRelation(ctx, payload.SrcID, payload.DstID, payload.RelType)
	case mutationOperationImportIssue:
		var payload ImportIssue
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.ImportIssue(ctx, payload)
	case mutationOperationImportComment:
		var payload ImportComment
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.ImportComment(ctx, payload)
	case mutationOperationImportRelation:
		var payload ImportRelation
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.ImportRelation(ctx, payload)
	case mutationOperationImportLabel:
		var payload ImportLabel
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.ImportLabel(ctx, payload)
	case mutationOperationAddLabel:
		var payload AddLabelInput
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.AddLabel(ctx, payload)
	case mutationOperationRemoveLabel:
		var payload removeLabelQueuePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.RemoveLabel(ctx, payload.IssueID, payload.LabelName)
	case mutationOperationReplaceLabels:
		var payload replaceLabelsQueuePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.ReplaceLabels(ctx, payload.IssueID, payload.Labels, payload.CreatedBy)
	case mutationOperationTransitionIssue:
		var payload TransitionIssueInput
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.TransitionIssue(ctx, payload)
	case mutationOperationSetParent:
		var payload SetParentInput
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.SetParent(ctx, payload)
	case mutationOperationClearParent:
		var payload clearParentQueuePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.ClearParent(ctx, payload.ChildID)
	case mutationOperationReplaceExport:
		var payload model.Export
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return struct{}{}, s.ReplaceFromExport(ctx, payload)
	case mutationOperationFsck:
		var payload fsckQueuePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil, err
		}
		return s.Fsck(ctx, payload.Repair)
	default:
		return nil, fmt.Errorf("unsupported queued mutation operation %q", entry.Operation)
	}
}

func (s *Store) storeMutationQueueResult(entryID string, result mutationQueueResult) {
	s.queueResultsMu.Lock()
	defer s.queueResultsMu.Unlock()
	s.queueResults[entryID] = result
}

func (s *Store) takeMutationQueueResult(entryID string) (mutationQueueResult, bool) {
	s.queueResultsMu.Lock()
	defer s.queueResultsMu.Unlock()
	result, ok := s.queueResults[entryID]
	if ok {
		delete(s.queueResults, entryID)
	}
	return result, ok
}

func (s *Store) readMutationQueueOffset() (int64, error) {
	payload, err := os.ReadFile(s.queueOffsetPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read mutation queue offset: %w", err)
	}
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return 0, nil
	}
	offset, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse mutation queue offset: %w", err)
	}
	return offset, nil
}

func (s *Store) writeMutationQueueOffset(offset int64) error {
	tmpPath := fmt.Sprintf("%s.tmp.%d", s.queueOffsetPath, os.Getpid())
	payload := []byte(strconv.FormatInt(offset, 10) + "\n")
	if err := os.WriteFile(tmpPath, payload, 0o644); err != nil {
		return fmt.Errorf("write mutation queue offset temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.queueOffsetPath); err != nil {
		return fmt.Errorf("rename mutation queue offset: %w", err)
	}
	return nil
}

func (s *Store) writeMutationQueueTelemetry(fields map[string]any) error {
	if err := os.MkdirAll(s.telemetryDir, 0o755); err != nil {
		return err
	}
	payload := map[string]any{
		"recorded_at": time.Now().UTC().Format(time.RFC3339Nano),
		"pid":         os.Getpid(),
		"queue_path":  s.queuePath,
		"offset_path": s.queueOffsetPath,
		"lock_path":   s.queueLockPath,
	}
	for key, value := range fields {
		payload[key] = value
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	fileName := fmt.Sprintf("mutation-queue-%s-%d.json", time.Now().UTC().Format("20060102T150405.000000000Z"), os.Getpid())
	return os.WriteFile(filepath.Join(s.telemetryDir, fileName), encoded, 0o644)
}
