package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

const (
	defaultOperationTimeout = 45 * time.Second
	operationTimeoutEnvVar  = "LIT_OPERATION_TIMEOUT"
)

type timeoutFileSnapshot struct {
	Path        string `json:"path"`
	Exists      bool   `json:"exists"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	Mode        string `json:"mode,omitempty"`
	ModifiedAt  string `json:"modified_at,omitempty"`
	ContentHead string `json:"content_head,omitempty"`
	Error       string `json:"error,omitempty"`
}

func resolveOperationTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(operationTimeoutEnvVar))
	if raw == "" {
		return defaultOperationTimeout, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", operationTimeoutEnvVar, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be > 0", operationTimeoutEnvVar)
	}
	return duration, nil
}

func writeOperationTimeoutTelemetry(args []string, timeout time.Duration) (string, error) {
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		cwd = "."
	}
	ws, wsErr := workspace.Resolve(cwd)
	telemetryDir := filepath.Join(cwd, ".lit-timeouts")
	if wsErr == nil {
		telemetryDir = filepath.Join(ws.StorageDir, "telemetry", "timeouts")
	}
	if err := os.MkdirAll(telemetryDir, 0o755); err != nil {
		return "", err
	}

	now := time.Now().UTC()
	baseName := fmt.Sprintf("timeout-%s-%d", now.Format("20060102T150405.000000000Z"), os.Getpid())
	stackPath := filepath.Join(telemetryDir, baseName+".stack.txt")
	processPath := filepath.Join(telemetryDir, baseName+".ps.txt")
	telemetryPath := filepath.Join(telemetryDir, baseName+".json")

	stackDump := captureAllGoroutineStacks()
	if err := os.WriteFile(stackPath, stackDump, 0o644); err != nil {
		return "", err
	}
	processDump := captureProcessTableSnapshot()
	if writeErr := os.WriteFile(processPath, processDump, 0o644); writeErr != nil {
		processPath = ""
	}

	payload := map[string]any{
		"recorded_at":      now.Format(time.RFC3339Nano),
		"event":            "operation_timeout",
		"timeout":          timeout.String(),
		"pid":              os.Getpid(),
		"cwd":              cwd,
		"args":             args,
		"stack_trace_file": stackPath,
		"lock_files":       []timeoutFileSnapshot{},
	}
	if processPath != "" {
		payload["process_table_file"] = processPath
	}
	if wsErr == nil {
		payload["workspace"] = map[string]string{
			"workspace_id":   ws.WorkspaceID,
			"root_dir":       ws.RootDir,
			"storage_dir":    ws.StorageDir,
			"database_path":  ws.DatabasePath,
			"dolt_repo_path": ws.DoltRepoPath,
		}
		payload["lock_files"] = captureTimeoutFileSnapshots([]string{
			filepath.Join(ws.DatabasePath, ".links-commit.lock"),
			filepath.Join(ws.DatabasePath, ".links-mutation-queue.lock"),
			filepath.Join(ws.DatabasePath, ".links-mutation-queue.offset"),
			filepath.Join(ws.DatabasePath, ".links-mutation-queue.jsonl"),
			filepath.Join(ws.DoltRepoPath, ".dolt/noms/LOCK"),
			filepath.Join(ws.DoltRepoPath, ".dolt/stats/.dolt/noms/LOCK"),
			filepath.Join(ws.DoltRepoPath, ".dolt/noms/manifest"),
		})
	} else {
		payload["workspace_resolve_error"] = wsErr.Error()
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(telemetryPath, encoded, 0o644); err != nil {
		return "", err
	}
	return telemetryPath, nil
}

func captureAllGoroutineStacks() []byte {
	// [LAW:verifiable-goals] Timeout diagnostics always include a full goroutine dump so lock contention sources are inspectable post-mortem.
	size := 1 << 20
	for {
		buffer := make([]byte, size)
		written := runtime.Stack(buffer, true)
		if written < len(buffer) {
			return buffer[:written]
		}
		if size >= 1<<26 {
			return buffer
		}
		size *= 2
	}
}

func captureProcessTableSnapshot() []byte {
	cmd := exec.Command("ps", "-Ao", "pid,ppid,etime,stat,command")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return []byte(fmt.Sprintf("ps capture failed: %v\n%s", err, strings.TrimSpace(string(output))))
	}
	return output
}

func captureTimeoutFileSnapshots(paths []string) []timeoutFileSnapshot {
	snapshots := make([]timeoutFileSnapshot, 0, len(paths))
	for _, path := range paths {
		snapshots = append(snapshots, captureTimeoutFileSnapshot(path))
	}
	return snapshots
}

func captureTimeoutFileSnapshot(path string) timeoutFileSnapshot {
	snapshot := timeoutFileSnapshot{Path: path}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot
	}
	if err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}
	snapshot.Exists = true
	snapshot.SizeBytes = info.Size()
	snapshot.Mode = info.Mode().String()
	snapshot.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339Nano)
	if info.IsDir() {
		return snapshot
	}
	payload, readErr := os.ReadFile(path)
	if readErr != nil {
		snapshot.Error = readErr.Error()
		return snapshot
	}
	const maxContentHead = 4096
	if len(payload) > maxContentHead {
		payload = payload[:maxContentHead]
	}
	snapshot.ContentHead = string(payload)
	return snapshot
}
