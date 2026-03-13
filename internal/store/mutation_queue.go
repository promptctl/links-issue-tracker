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
	"syscall"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

const (
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

type mutationQueueEntry struct {
	ID            string          `json:"id"`
	Operation     string          `json:"operation"`
	Payload       json.RawMessage `json:"payload"`
	EnqueuedAt    time.Time       `json:"enqueued_at"`
	EnqueuedByPID int             `json:"enqueued_by_pid"`
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

// DrainMutationQueue applies any pending queued mutations left by prior processes.
// [LAW:single-enforcer] This is the single entry point for queue drain, called once at the CLI boundary.
func (s *Store) DrainMutationQueue(ctx context.Context) error {
	release, acquired, err := tryAcquireNonBlockingMutationQueueLock(s.queueLockPath)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer release()

	applyCtx, cancel := context.WithTimeout(ctx, mutationQueueApplyTimeout)
	defer cancel()
	if err := s.applyMutationQueueLocked(applyCtx); err != nil {
		_ = s.writeMutationQueueTelemetry(map[string]any{
			"event": "queue_drain_error",
			"error": err.Error(),
		})
		return err
	}
	return nil
}

func (s *Store) appendMutationQueueEntry(entry mutationQueueEntry) error {
	if err := os.MkdirAll(filepath.Dir(s.queuePath), 0o700); err != nil {
		return fmt.Errorf("create queue dir: %w", err)
	}
	encodedEntry, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode queue entry: %w", err)
	}
	file, err := os.OpenFile(s.queuePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open mutation queue: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(encodedEntry, '\n')); err != nil {
		return fmt.Errorf("append mutation queue entry: %w", err)
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
	running, knownOwner, runErr := mutationQueueLockOwnerRunning(lockPath)
	if runErr != nil {
		return runErr
	}
	// [LAW:single-enforcer] Liveness ownership is the single authority for lock-file deletion decisions.
	if knownOwner && running {
		return nil
	}
	if !knownOwner && time.Since(info.ModTime()) <= staleAfter {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale mutation queue lock: %w", err)
	}
	return nil
}

func mutationQueueLockOwnerRunning(path string) (running bool, knownOwner bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	pidValue := strings.TrimSpace(string(raw))
	if pidValue == "" {
		return false, false, nil
	}
	pid, err := strconv.Atoi(pidValue)
	if err != nil || pid <= 0 {
		return false, false, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, true, nil
	}
	signalErr := proc.Signal(syscall.Signal(0))
	if signalErr == nil || errors.Is(signalErr, syscall.EPERM) {
		return true, true, nil
	}
	if errors.Is(signalErr, os.ErrProcessDone) || errors.Is(signalErr, syscall.ESRCH) {
		return false, true, nil
	}
	return false, true, signalErr
}

func (s *Store) applyMutationQueueLocked(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.queuePath), 0o700); err != nil {
		return fmt.Errorf("create queue dir: %w", err)
	}
	offset, err := s.readMutationQueueOffset()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(s.queuePath, os.O_CREATE|os.O_RDONLY, 0o600)
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
		_, applyErr := s.applyMutationQueueEntry(ctx, entry)
		if applyErr != nil {
			retryable := shouldRetryQueuedMutationError(applyErr)
			_ = s.writeMutationQueueTelemetry(map[string]any{
				"event":      "queue_entry_apply_error",
				"entry_id":   entry.ID,
				"operation":  entry.Operation,
				"error":      applyErr.Error(),
				"retryable":  retryable,
				"queue_path": s.queuePath,
			})
			if retryable {
				return fmt.Errorf("apply queued mutation entry %s (%s): %w", entry.ID, entry.Operation, applyErr)
			}
			currentOffset = nextOffset
			if err := s.writeMutationQueueOffset(currentOffset); err != nil {
				return err
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

func shouldRetryQueuedMutationError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, ErrTransientManifestReadOnly) {
		return true
	}
	var transientErr transientManifestReadOnlyError
	if errors.As(err, &transientErr) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "acquire commit lock: lock not acquired")
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
	if err := os.WriteFile(tmpPath, payload, 0o600); err != nil {
		return fmt.Errorf("write mutation queue offset temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.queueOffsetPath); err != nil {
		return fmt.Errorf("rename mutation queue offset: %w", err)
	}
	return nil
}

func (s *Store) writeMutationQueueTelemetry(fields map[string]any) error {
	if err := os.MkdirAll(s.telemetryDir, 0o700); err != nil {
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
	return os.WriteFile(filepath.Join(s.telemetryDir, fileName), encoded, 0o600)
}
