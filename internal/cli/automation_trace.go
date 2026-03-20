package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

const (
	automationTriggerEnvVar      = "LNKS_AUTOMATION_TRIGGER"
	automationReasonEnvVar       = "LNKS_AUTOMATION_REASON"
	automationTraceRefFileEnvVar = "LNKS_AUTOMATION_TRACE_REF_FILE"
)

var nonTraceSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

type automationTraceRecord struct {
	ID          string            `json:"id"`
	RecordedAt  string            `json:"recorded_at"`
	WorkspaceID string            `json:"workspace_id"`
	Trigger     string            `json:"trigger"`
	Command     string            `json:"command"`
	SideEffect  string            `json:"side_effect"`
	Status      string            `json:"status"`
	Reason      string            `json:"reason,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type automationTraceRef struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type automationContext struct {
	Trigger      string
	Reason       string
	TraceRefFile string
}

func automationTraceDir(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "traces", "automation")
}

func readAutomationContextFromEnv() automationContext {
	return automationContext{
		Trigger:      strings.TrimSpace(os.Getenv(automationTriggerEnvVar)),
		Reason:       strings.TrimSpace(os.Getenv(automationReasonEnvVar)),
		TraceRefFile: strings.TrimSpace(os.Getenv(automationTraceRefFileEnvVar)),
	}
}

func maybeRecordAutomatedCommandTrace(ws workspace.Info, command string, sideEffect string, status string, reason string, metadata map[string]string) (*automationTraceRef, error) {
	ctx := readAutomationContextFromEnv()
	if ctx.Trigger == "" {
		return nil, nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = ctx.Reason
	}
	traceRef, err := recordAutomationTrace(ws, automationTraceRecord{
		Trigger:    ctx.Trigger,
		Command:    strings.TrimSpace(command),
		SideEffect: strings.TrimSpace(sideEffect),
		Status:     strings.TrimSpace(status),
		Reason:     strings.TrimSpace(reason),
		Metadata:   metadata,
	})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ctx.TraceRefFile) != "" {
		if writeErr := os.WriteFile(ctx.TraceRefFile, []byte(traceRef.Path+"\n"), 0o644); writeErr != nil {
			return nil, fmt.Errorf("write automation trace ref: %w", writeErr)
		}
	}
	return &traceRef, nil
}

func recordAutomationTrace(ws workspace.Info, record automationTraceRecord) (automationTraceRef, error) {
	traceDir := automationTraceDir(ws)
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		return automationTraceRef{}, fmt.Errorf("create automation trace dir: %w", err)
	}
	// [LAW:one-source-of-truth] All automatic-action traces use one shared record shape and one storage directory.
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return automationTraceRef{}, fmt.Errorf("marshal automation trace: %w", err)
	}
	payload = append(payload, '\n')
	for attempt := 0; attempt < 5; attempt++ {
		timestamp := time.Now().UTC()
		traceID := fmt.Sprintf("%s-%s", timestamp.Format("20060102T150405.000000000Z"), traceSlug(record.Trigger))
		if attempt > 0 {
			traceID = fmt.Sprintf("%s-%d", traceID, attempt)
		}
		record.ID = traceID
		record.RecordedAt = timestamp.Format(time.RFC3339Nano)
		record.WorkspaceID = ws.WorkspaceID
		record.Trigger = strings.TrimSpace(record.Trigger)
		record.Command = strings.TrimSpace(record.Command)
		record.SideEffect = strings.TrimSpace(record.SideEffect)
		record.Status = strings.TrimSpace(record.Status)
		record.Reason = strings.TrimSpace(record.Reason)
		record.Metadata = compactTraceMetadata(record.Metadata)
		tracePath := filepath.Join(traceDir, traceID+".json")
		payload, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return automationTraceRef{}, fmt.Errorf("marshal automation trace: %w", err)
		}
		payload = append(payload, '\n')
		file, openErr := os.OpenFile(tracePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if openErr != nil {
			if os.IsExist(openErr) {
				continue
			}
			return automationTraceRef{}, fmt.Errorf("create automation trace: %w", openErr)
		}
		if _, writeErr := file.Write(payload); writeErr != nil {
			_ = file.Close()
			return automationTraceRef{}, fmt.Errorf("write automation trace: %w", writeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return automationTraceRef{}, fmt.Errorf("close automation trace: %w", closeErr)
		}
		return automationTraceRef{ID: traceID, Path: tracePath}, nil
	}
	return automationTraceRef{}, fmt.Errorf("create automation trace: too many id collisions")
}

func formatCommand(args []string) string {
	parts := []string{"lit"}
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "" {
			continue
		}
		parts = append(parts, trimmed)
	}
	return strings.Join(parts, " ")
}

func compactTraceMetadata(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		trimmedKey := strings.TrimSpace(key)
		trimmedValue := strings.TrimSpace(value)
		if trimmedKey == "" || trimmedValue == "" {
			continue
		}
		output[trimmedKey] = trimmedValue
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

func traceSlug(input string) string {
	normalized := strings.ToLower(strings.TrimSpace(input))
	normalized = nonTraceSlugPattern.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")
	if normalized == "" {
		return "trace"
	}
	return normalized
}
