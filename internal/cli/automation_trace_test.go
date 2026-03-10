package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestRecordAutomationTraceWritesCanonicalJSON(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	traceRef, err := recordAutomationTrace(ws, automationTraceRecord{
		Trigger:    "startup-preflight",
		Command:    "lit ls",
		SideEffect: "block command execution until beads migration completes",
		Status:     "blocked",
		Reason:     "beads residue detected during startup preflight",
		Metadata: map[string]string{
			"blocked_command":     "lit ls",
			"remediation_command": "lit migrate beads --apply --json",
		},
	})
	if err != nil {
		t.Fatalf("recordAutomationTrace() error = %v", err)
	}
	if filepath.Dir(traceRef.Path) != automationTraceDir(ws) {
		t.Fatalf("trace directory = %q, want %q", filepath.Dir(traceRef.Path), automationTraceDir(ws))
	}

	payload, err := os.ReadFile(traceRef.Path)
	if err != nil {
		t.Fatalf("ReadFile(trace) error = %v", err)
	}
	var record automationTraceRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatalf("json.Unmarshal(trace) error = %v", err)
	}
	if record.WorkspaceID != ws.WorkspaceID {
		t.Fatalf("workspace_id = %q, want %q", record.WorkspaceID, ws.WorkspaceID)
	}
	if record.Trigger != "startup-preflight" {
		t.Fatalf("trigger = %q, want startup-preflight", record.Trigger)
	}
	if record.Command != "lit ls" {
		t.Fatalf("command = %q, want lit ls", record.Command)
	}
}

func TestMaybeRecordAutomatedCommandTraceWritesTraceRefFile(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	traceRefFile := filepath.Join(t.TempDir(), "trace-ref.txt")
	t.Setenv(automationTriggerEnvVar, "git-pre-push")
	t.Setenv(automationReasonEnvVar, "git push triggered the managed pre-push sync")
	t.Setenv(automationTraceRefFileEnvVar, traceRefFile)

	traceRef, err := maybeRecordAutomatedCommandTrace(
		ws,
		"lit sync push --remote origin --branch codex/test",
		"mirror Dolt data to the configured git remote",
		"ok",
		"",
		map[string]string{"branch": "codex/test", "remote": "origin"},
	)
	if err != nil {
		t.Fatalf("maybeRecordAutomatedCommandTrace() error = %v", err)
	}
	if traceRef == nil {
		t.Fatal("traceRef = nil, want recorded trace")
	}
	traceRefPayload, err := os.ReadFile(traceRefFile)
	if err != nil {
		t.Fatalf("ReadFile(traceRefFile) error = %v", err)
	}
	if got := string(traceRefPayload); got != traceRef.Path+"\n" {
		t.Fatalf("trace ref file = %q, want %q", got, traceRef.Path+"\n")
	}
}

func TestBuildCommandErrorPayloadIncludesDetailedErrorFields(t *testing.T) {
	payload := buildCommandErrorPayload(BeadsMigrationRequiredError{
		Summary:            "hooks=1",
		Trigger:            "startup-preflight",
		BlockedCommand:     "lit ls",
		RemediationCommand: "lit migrate beads --apply --json",
		TraceRef:           "/tmp/trace.json",
	})
	if payload.Trigger != "startup-preflight" {
		t.Fatalf("trigger = %v, want startup-preflight", payload.Trigger)
	}
	if payload.TraceRef != "/tmp/trace.json" {
		t.Fatalf("trace_ref = %v, want /tmp/trace.json", payload.TraceRef)
	}
	if payload.BlockedCommand != "lit ls" {
		t.Fatalf("blocked_command = %v, want lit ls", payload.BlockedCommand)
	}
}
