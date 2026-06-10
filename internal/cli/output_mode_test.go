package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

func TestParseGlobalOutputMode(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantArgs []string
		wantMode outputMode
	}{
		{
			name:     "default mode is text",
			args:     []string{"quickstart"},
			wantArgs: []string{"quickstart"},
			wantMode: outputModeText,
		},
		{
			name:     "exact global json flag enables json mode",
			args:     []string{"--json", "quickstart"},
			wantArgs: []string{"quickstart"},
			wantMode: outputModeJSON,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotArgs, gotMode, err := parseGlobalOutputMode(tc.args, &bytes.Buffer{})
			if err != nil {
				t.Fatalf("parseGlobalOutputMode() error = %v", err)
			}
			if gotMode != tc.wantMode {
				t.Fatalf("mode = %q, want %q", gotMode, tc.wantMode)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("args = %#v, want %#v", gotArgs, tc.wantArgs)
			}
		})
	}
}

func TestRunQuickstartDefaultsToTextOnNonTTY(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"quickstart"}); err != nil {
		t.Fatalf("Run(quickstart) error = %v", err)
	}
	if strings.Contains(stdout.String(), "\"summary\"") {
		t.Fatalf("quickstart default output should be text: %q", stdout.String())
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatal("quickstart text output is empty")
	}
}

func TestRunQuickstartRejectsJSONOutput(t *testing.T) {
	var stdout bytes.Buffer
	err := Run(context.Background(), &stdout, &stdout, []string{"--json", "quickstart"})
	if err == nil {
		t.Fatal("Run(--json quickstart) unexpectedly succeeded")
	}
	if got := ExitCode(err); got != ExitUsage {
		t.Fatalf("ExitCode(--json quickstart) = %d, want %d", got, ExitUsage)
	}
}

func TestRunQuickstartRejectsCommandLocalJSONFlag(t *testing.T) {
	var stdout bytes.Buffer
	err := Run(context.Background(), &stdout, &stdout, []string{"quickstart", "--json"})
	if err == nil {
		t.Fatal("Run(quickstart --json) unexpectedly succeeded")
	}
	if got := ExitCode(err); got != ExitUsage {
		t.Fatalf("ExitCode(quickstart --json) = %d, want %d", got, ExitUsage)
	}
}

// TestSubcommandJSONFlagPromotesWriterMode pins the absorbed variance:
// the subcommand --json flag promotes output mode through the writer, not a
// parallel bool. [LAW:one-source-of-truth] [LAW:single-enforcer]
func TestSubcommandJSONFlagPromotesWriterMode(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "probe", Topic: "mode", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var jsonBuf bytes.Buffer
	if err := runShow(ctx, newOutputModeWriter(&jsonBuf, outputModeText), ap, []string{created.ID, "--json"}); err != nil {
		t.Fatalf("runShow --json error = %v", err)
	}
	if !json.Valid(jsonBuf.Bytes()) {
		t.Fatalf("--json output is not valid JSON: %s", jsonBuf.String())
	}

	var textBuf bytes.Buffer
	if err := runShow(ctx, &textBuf, ap, []string{created.ID}); err != nil {
		t.Fatalf("runShow (text) error = %v", err)
	}
	if json.Valid(textBuf.Bytes()) && strings.TrimSpace(textBuf.String()) != "" {
		t.Fatalf("text output decoded as JSON; mode leaked: %s", textBuf.String())
	}
}
