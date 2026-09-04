package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// renderedGrouping parses `lit --help` (the rendered usage) into the group each
// command appears under, plus where each group header falls, so assertions read
// the observable help output rather than the internal commandGroups slice.
// [LAW:behavior-not-structure] the contract is what `--help` shows an agent, not
// how the registry is stored.
func renderedGrouping(t *testing.T) (groupOf map[string]string, headerLine map[string]int) {
	t.Helper()
	root := newRootCommand(context.Background(), io.Discard, io.Discard)
	groupOf = map[string]string{}
	headerLine = map[string]int{}
	current := ""
	for i, line := range strings.Split(root.UsageString(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "  ") { // "  <name>   <summary>" — a command row
			if fields := strings.Fields(line); len(fields) > 0 && current != "" {
				groupOf[fields[0]] = current
			}
			continue
		}
		// A non-indented line is a section header. The non-group scaffolding
		// sections (Usage/Flags/Additional Commands/the "Use ..." footer) are not
		// command groups, so they clear the current group rather than becoming one.
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "Usage:", trimmed == "Flags:",
			strings.HasPrefix(trimmed, "Additional"), strings.HasPrefix(trimmed, "Use \""):
			current = ""
		default:
			current = trimmed
			headerLine[current] = i
		}
	}
	return groupOf, headerLine
}

// commandGroupPaths derives every advertised command-group path — a command
// whose surface is a set of subcommands, nested groups like `sync remote` and
// `bulk label` included — from the registry the completion projection also
// reads, so the help contract below covers exactly the groups lit advertises
// and can never lag behind a newly registered one. [LAW:one-source-of-truth]
func commandGroupPaths() [][]string {
	var paths [][]string
	var walk func(prefix []string, subs []SubcommandSpec)
	walk = func(prefix []string, subs []SubcommandSpec) {
		for _, sub := range subs {
			if len(sub.Subcommands) == 0 {
				continue
			}
			path := append(append([]string{}, prefix...), sub.Name)
			paths = append(paths, path)
			walk(path, sub.Subcommands)
		}
	}
	for _, spec := range commandSpecs(context.Background(), io.Discard, io.Discard) {
		if spec.Hidden || len(spec.Subcommands) == 0 {
			continue
		}
		paths = append(paths, []string{spec.Name})
		walk([]string{spec.Name}, spec.Subcommands)
	}
	return paths
}

// Asking a command group for help is answered as help: the group's usage on
// stdout, nothing on stderr, and success — Run returning nil is what main maps
// to exit 0 (links-cli-zc3r). The shape this pins out was an error-framed usage
// line with a retry-then-doctor remediation and exit 1, an answer-shaped wrong
// answer: the retry could never succeed, and doctor got run against a healthy
// store. [LAW:behavior-not-structure] only the observable answer is asserted;
// where the recognition lives is the implementation's business.
func TestCommandGroupHelpExitsZeroWithUsageOnStdout(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var initOut bytes.Buffer
	if err := Run(context.Background(), &initOut, &initOut, []string{"init"}); err != nil {
		t.Fatalf("Run(init) error = %v", err)
	}

	for _, path := range commandGroupPaths() {
		for _, helpFlag := range []string{"-h", "--help"} {
			args := append(append([]string{}, path...), helpFlag)
			var stdout, stderr bytes.Buffer
			if err := Run(context.Background(), &stdout, &stderr, args); err != nil {
				t.Fatalf("Run(%v) error = %v, want help answered as success", args, err)
			}
			if got := stdout.String(); !strings.Contains(got, path[len(path)-1]) {
				t.Fatalf("Run(%v) stdout = %q, want usage naming %q", args, got, path[len(path)-1])
			}
			if got := stderr.String(); got != "" {
				t.Fatalf("Run(%v) stderr = %q, want empty — no error framing on a help answer", args, got)
			}
		}
	}
}

// A nested group's help must be answered by the OUTER family resolve, before
// the dispatch pipeline acquires the workspace, store, or app the real
// subcommand needs — under store contention an acquisition-first help would
// block for the open-retry budget and then fail busy instead of answering.
// Running outside any git repository is the machine-checkable proxy: success
// here is only possible if help acquired nothing. [LAW:verifiable-goals]
// Paths are derived from the registry, so a future nested group added to the
// completion tree without pre-acquisition help recognition fails this test.
func TestNestedGroupHelpAnswersWithoutAcquiringResources(t *testing.T) {
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir(nonRepo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	tested := 0
	for _, path := range commandGroupPaths() {
		if len(path) < 2 {
			continue
		}
		tested++
		for _, helpFlag := range []string{"-h", "--help"} {
			args := append(append([]string{}, path...), helpFlag)
			var stdout, stderr bytes.Buffer
			if err := Run(context.Background(), &stdout, &stderr, args); err != nil {
				t.Fatalf("Run(%v) outside a repo error = %v, want help answered without any workspace/store/app", args, err)
			}
			if got := stdout.String(); !strings.Contains(got, path[len(path)-1]) {
				t.Fatalf("Run(%v) stdout = %q, want usage naming %q", args, got, path[len(path)-1])
			}
		}
	}
	if tested == 0 {
		t.Fatal("registry advertises no nested groups; the test asserted nothing")
	}
}

// The state-transition surface splits across two help groups so the high-traffic
// status lifecycle stands out: the core verbs stay in Agent Operations, the rare
// retention verbs move to their own Issue Retention group rendered below it. This
// pins that split — the acceptance criterion for regrouping the transition verbs —
// against the rendered `lit --help`.
func TestTransitionVerbGrouping(t *testing.T) {
	t.Parallel()
	groupOf, headerLine := renderedGrouping(t)

	for _, name := range []string{"start", "done", "close", "open"} {
		if got := groupOf[name]; got != "Agent Operations" {
			t.Fatalf("core lifecycle verb %q renders under %q, want Agent Operations", name, got)
		}
	}
	for _, name := range []string{"archive", "unarchive", "delete", "restore"} {
		if got := groupOf[name]; got != "Issue Retention" {
			t.Fatalf("retention verb %q renders under %q, want Issue Retention (demoted out of Agent Operations)", name, got)
		}
	}

	// Issue Retention must render below Agent Operations, or the demotion that
	// keeps the core verbs prominent would not hold in the output an agent reads.
	ops, okOps := headerLine["Agent Operations"]
	ret, okRet := headerLine["Issue Retention"]
	if !okOps || !okRet {
		t.Fatalf("help is missing a group header: Agent Operations present=%v, Issue Retention present=%v", okOps, okRet)
	}
	if ret <= ops {
		t.Fatalf("Issue Retention header at line %d, want below Agent Operations at line %d", ret, ops)
	}
}
