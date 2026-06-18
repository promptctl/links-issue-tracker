package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestParseGlobalArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantArgs []string
	}{
		{
			name:     "subcommand args pass through",
			args:     []string{"quickstart"},
			wantArgs: []string{"quickstart"},
		},
		{
			name:     "leading -- ends global parsing",
			args:     []string{"--", "ready"},
			wantArgs: []string{"ready"},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotArgs, err := parseGlobalArgs(tc.args)
			if err != nil {
				t.Fatalf("parseGlobalArgs() error = %v", err)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("args = %#v, want %#v", gotArgs, tc.wantArgs)
			}
		})
	}
}

// TestParseGlobalArgsRejectsOutputFlag pins the one legacy flag the global
// parser still refuses outright. [LAW:no-silent-failure]
func TestParseGlobalArgsRejectsOutputFlag(t *testing.T) {
	for _, arg := range []string{"--output", "--output=json"} {
		if _, err := parseGlobalArgs([]string{arg}); err == nil {
			t.Fatalf("parseGlobalArgs(%q) succeeded; want UnsupportedError", arg)
		}
	}
}

func TestRunQuickstartDefaultsToText(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"quickstart"}); err != nil {
		t.Fatalf("Run(quickstart) error = %v", err)
	}
	if strings.Contains(stdout.String(), "\"summary\"") {
		t.Fatalf("quickstart output should be text: %q", stdout.String())
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatal("quickstart text output is empty")
	}
}

// TestRejectsJSONFlag pins the removal: --json is no longer a flag, so it is an
// unknown flag at any position and the command fails with ExitUsage rather than
// emitting JSON. [LAW:no-silent-failure]
//
// The cases use workspace-free commands on purpose. The two rejection paths are
// the single enforcers every command shares — the root FlagErrorFunc (global
// position) and parseFlagSet (command-local) — so quickstart/version exercise
// the same mechanism a store-backed command would, without coupling the test to
// ambient workspace state: a command that opens the app first can fail at
// app.Open (exit 1) before its flags are ever parsed. [LAW:no-ambient-temporal-coupling]
func TestRejectsJSONFlag(t *testing.T) {
	cases := [][]string{
		{"--json", "quickstart"}, // global position → root FlagErrorFunc
		{"quickstart", "--json"}, // command-local → parseFlagSet
		{"version", "--json"},    // command-local → parseFlagSet
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			err := Run(context.Background(), &stdout, &stdout, args)
			if err == nil {
				t.Fatalf("Run(%v) unexpectedly succeeded", args)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Fatalf("ExitCode(%v) = %d, want %d", args, got, ExitUsage)
			}
		})
	}
}
