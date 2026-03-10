package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestQuickstartOutputsStructuredJSON(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"quickstart", "--json"}); err != nil {
		t.Fatalf("Run(quickstart --json) error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("quickstart json decode failed: %v", err)
	}
	if _, ok := payload["summary"]; !ok {
		t.Fatalf("quickstart payload missing summary: %#v", payload)
	}
	if _, ok := payload["workflow"]; !ok {
		t.Fatalf("quickstart payload missing workflow: %#v", payload)
	}
}
