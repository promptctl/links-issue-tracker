package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// Invoking a retired command yields a documented pointer, not silent success and
// not cobra's bare unknown-command error: a typed RetiredCommandError whose
// message names the replacement commands, classified as a validation-class exit
// with a self-contained (no second-line) remediation. The handler returns before
// opening any workspace, so this holds outside a git repo too.
func TestRetiredCommandsPointToReplacements(t *testing.T) {
	for _, name := range []string{"ready", "queue"} {
		var out bytes.Buffer
		err := Run(context.Background(), &out, &out, []string{name})

		var retired RetiredCommandError
		if !errors.As(err, &retired) {
			t.Fatalf("lit %s error = %v (%T), want RetiredCommandError", name, err, err)
		}
		if retired.Command != name {
			t.Errorf("RetiredCommandError.Command = %q, want %q", retired.Command, name)
		}
		msg := err.Error()
		for _, want := range []string{"retired", "lit backlog", "lit next"} {
			if !strings.Contains(msg, want) {
				t.Errorf("lit %s error = %q, want it to mention %q", name, msg, want)
			}
		}
		if got := ExitCode(err); got != ExitValidation {
			t.Errorf("lit %s exit code = %d, want %d", name, got, ExitValidation)
		}
		if got := commandErrorReason(err); got != "retired_command" {
			t.Errorf("lit %s reason = %q, want retired_command", name, got)
		}
		// The message already names the replacements, so there is no second
		// remediation line to drift from it. [LAW:one-source-of-truth]
		if rem := commandErrorRemediation(commandErrorReason(err)); rem != "" {
			t.Errorf("lit %s remediation = %q, want empty (message is self-contained)", name, rem)
		}
	}
}

// The single-purpose commands folded into flags this pass are retired the same
// way ready/queue were: a RetiredCommandError whose message names the surviving
// flag, so a stale invocation is redirected rather than silently broken. Each
// carries its own pointer, so the expected substrings differ per command.
func TestFoldedCommandsPointToTheirFlags(t *testing.T) {
	cases := []struct {
		name  string
		wants []string
	}{
		{"assign", []string{"retired", "lit update", "--assignee"}},
		{"ls-at", []string{"retired", "lit ls --at"}},
		{"overview", []string{"retired", "lit stores --counts"}},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		err := Run(context.Background(), &out, &out, []string{tc.name})

		var retired RetiredCommandError
		if !errors.As(err, &retired) {
			t.Fatalf("lit %s error = %v (%T), want RetiredCommandError", tc.name, err, err)
		}
		if retired.Command != tc.name {
			t.Errorf("RetiredCommandError.Command = %q, want %q", retired.Command, tc.name)
		}
		msg := err.Error()
		for _, want := range tc.wants {
			if !strings.Contains(msg, want) {
				t.Errorf("lit %s error = %q, want it to mention %q", tc.name, msg, want)
			}
		}
		if got := ExitCode(err); got != ExitValidation {
			t.Errorf("lit %s exit code = %d, want %d", tc.name, got, ExitValidation)
		}
		if got := commandErrorReason(err); got != "retired_command" {
			t.Errorf("lit %s reason = %q, want retired_command", tc.name, got)
		}
	}
}

// `bulk import` is a retired subcommand: it answers with the pointer to
// `backup restore`, which owns the same export-restore mechanism. It dispatches
// through the production family table (runAppFamily), and its handler ignores the
// app, so a nil app is enough to prove the retirement message.
func TestBulkImportRetiredPointsToBackupRestore(t *testing.T) {
	var out bytes.Buffer
	err := runAppFamily(bulkFamily, context.Background(), &out, nil, []string{"import"})

	var retired RetiredCommandError
	if !errors.As(err, &retired) {
		t.Fatalf("bulk import error = %v (%T), want RetiredCommandError", err, err)
	}
	if retired.Command != "bulk import" {
		t.Errorf("RetiredCommandError.Command = %q, want %q", retired.Command, "bulk import")
	}
	for _, want := range []string{"retired", "lit backup restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("bulk import error = %q, want it to mention %q", err.Error(), want)
		}
	}
	// It is dropped from the advertised bulk subcommand surface (completion/usage)
	// while staying dispatchable. [LAW:one-source-of-truth]
	for _, sub := range bulkFamily.visibleSubcommands() {
		if sub.Name == "import" {
			t.Errorf("bulk import must not appear in the advertised subcommand surface")
		}
	}
}

// The retired commands stay registered (so they can answer with the pointer) but
// are marked Hidden, which is the single bit that keeps them off both `--help`
// and the completion projection.
func TestRetiredCommandsAreHiddenButRegistered(t *testing.T) {
	specs := commandSpecs(context.Background(), nil, nil)
	byName := map[string]CommandSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	for _, name := range []string{"ready", "queue", "assign", "ls-at", "overview"} {
		spec, ok := byName[name]
		if !ok {
			t.Fatalf("retired command %q must stay registered so it can return its pointer", name)
		}
		if !spec.Hidden {
			t.Errorf("retired command %q must be Hidden to leave the advertised surface", name)
		}
	}
}

// `lit --help` presents the curated workable surface and no retired command: the
// surviving views are listed and no "(retired)" summary leaks through, because a
// Hidden command is absent from the rendered help. This is the ticket's surface
// acceptance criterion.
func TestRootHelpShowsCuratedWorkableSurface(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), &out, &out, []string{"--help"}); err != nil {
		t.Fatalf("lit --help error = %v", err)
	}
	help := out.String()
	for _, want := range []string{"backlog", "next", "ls", "orphaned"} {
		if !strings.Contains(help, want) {
			t.Errorf("lit --help missing curated command %q; output:\n%s", want, help)
		}
	}
	if strings.Contains(help, "(retired)") {
		t.Errorf("lit --help leaks a retired command summary; output:\n%s", help)
	}
}
