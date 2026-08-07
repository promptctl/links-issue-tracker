package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/trace"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

const (
	automationTriggerEnvVar      = "LNKS_AUTOMATION_TRIGGER"
	automationReasonEnvVar       = "LNKS_AUTOMATION_REASON"
	automationTraceRefFileEnvVar = "LNKS_AUTOMATION_TRACE_REF_FILE"

	// automationTraceKind is this trace kind's directory name under
	// <storageDir>/traces/. [LAW:one-source-of-truth]
	automationTraceKind = "automation"
)

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
	return trace.Dir(ws.StorageDir, automationTraceKind)
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
	record.Trigger = strings.TrimSpace(record.Trigger)
	record.Command = strings.TrimSpace(record.Command)
	record.SideEffect = strings.TrimSpace(record.SideEffect)
	record.Status = strings.TrimSpace(record.Status)
	record.Reason = strings.TrimSpace(record.Reason)
	record.Metadata = compactTraceMetadata(record.Metadata)
	// [LAW:one-source-of-truth] All automatic-action traces use one shared record shape and, via trace.Write, one collision-retry writer.
	id, path, err := trace.Write(ws.StorageDir, automationTraceKind, trace.Slug(record.Trigger), func(id string, recordedAt time.Time) ([]byte, error) {
		record.ID = id
		record.RecordedAt = recordedAt.Format(time.RFC3339Nano)
		record.WorkspaceID = ws.WorkspaceID
		payload, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(payload, '\n'), nil
	})
	if err != nil {
		return automationTraceRef{}, err
	}
	return automationTraceRef{ID: id, Path: path}, nil
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
