package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/store"
)

func TestBuildCommandErrorPayloadUnknownCommand(t *testing.T) {
	err := errors.New(`unknown command "wat"`)
	payload := buildCommandErrorPayload(err)

	if payload.Code != "validation" {
		t.Fatalf("code = %q, want validation", payload.Code)
	}
	if payload.Reason != "unknown_command" {
		t.Fatalf("reason = %q, want unknown_command", payload.Reason)
	}
	if payload.ExitCode != ExitValidation {
		t.Fatalf("exit_code = %d, want %d", payload.ExitCode, ExitValidation)
	}
	if payload.Remediation == "" {
		t.Fatal("remediation should not be empty")
	}
	if payload.TraceRef == "" {
		t.Fatal("trace_ref should not be empty")
	}
}


func TestBuildCommandErrorPayloadNotFound(t *testing.T) {
	err := store.NotFoundError{Entity: "issue", ID: "lit-abc"}
	payload := buildCommandErrorPayload(err)

	if payload.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", payload.Code)
	}
	if payload.Reason != "entity_not_found" {
		t.Fatalf("reason = %q, want entity_not_found", payload.Reason)
	}
}

func TestBuildCommandErrorPayloadTraceRefDeterministic(t *testing.T) {
	err := errors.New("boom")
	a := buildCommandErrorPayload(err)
	b := buildCommandErrorPayload(err)
	if a.TraceRef != b.TraceRef {
		t.Fatalf("trace_ref mismatch: %q != %q", a.TraceRef, b.TraceRef)
	}
}

func TestShouldEmitJSONError(t *testing.T) {
	nonTTY := &bytes.Buffer{}

	t.Run("default errors use text", func(t *testing.T) {
		if shouldEmitJSONError([]string{"quickstart"}, nonTTY) {
			t.Fatal("expected text mode when no explicit json was requested")
		}
	})

	t.Run("exact global json flag enables json", func(t *testing.T) {
		if !shouldEmitJSONError([]string{"--json", "quickstart"}, nonTTY) {
			t.Fatal("expected json mode from --json")
		}
	})

	t.Run("command-local json flag wins for startup errors", func(t *testing.T) {
		if !shouldEmitJSONError([]string{"ready", "--json"}, nonTTY) {
			t.Fatal("expected json mode from command-local --json")
		}
	})
}

func TestWriteCommandErrorJSON(t *testing.T) {
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	exitCode := WriteCommandError(&stderr, &stdout, []string{"--json", "unknown"}, errors.New(`unknown command "unknown"`))
	if exitCode != ExitValidation {
		t.Fatalf("exitCode = %d, want %d", exitCode, ExitValidation)
	}

	var payload map[string]map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatalf("stderr should be json: %v", err)
	}
	errorPayload := payload["error"]
	if errorPayload["code"] != "validation" {
		t.Fatalf("code = %v, want validation", errorPayload["code"])
	}
	if errorPayload["reason"] != "unknown_command" {
		t.Fatalf("reason = %v, want unknown_command", errorPayload["reason"])
	}
	if errorPayload["exit_code"] != float64(ExitValidation) {
		t.Fatalf("exit_code = %v, want %d", errorPayload["exit_code"], ExitValidation)
	}
}

func TestWriteCommandErrorText(t *testing.T) {
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	WriteCommandError(&stderr, &stdout, []string{"unknown"}, errors.New(`unknown command "unknown"`))

	if !strings.Contains(stderr.String(), "error (code=3): unknown command \"unknown\"") {
		t.Fatalf("unexpected text stderr: %q", stderr.String())
	}
}

