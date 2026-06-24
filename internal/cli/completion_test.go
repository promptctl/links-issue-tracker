package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func normalizeWhitespace(input string) string {
	return strings.Join(strings.Fields(input), " ")
}

func TestCompletionScriptsRender(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var stdout bytes.Buffer
		if err := runCompletion(&stdout, []string{shell}); err != nil {
			t.Fatalf("runCompletion(%q) error = %v", shell, err)
		}
		if !strings.Contains(stdout.String(), "lit") {
			t.Fatalf("completion output for %q missing lit command name: %q", shell, stdout.String())
		}
	}
}

func renderCompletion(t *testing.T, shell string) string {
	t.Helper()
	var stdout bytes.Buffer
	if err := runCompletion(&stdout, []string{shell}); err != nil {
		t.Fatalf("runCompletion(%q) error = %v", shell, err)
	}
	return stdout.String()
}

// TestCompletionTopLevelDerivedFromRegistry pins the law contract: the top-level
// command list every shell offers is exactly the registry projection, in order.
// A command added to commandSpecs flows into completion with no second edit;
// none can silently drop. [LAW:one-source-of-truth]
func TestCompletionTopLevelDerivedFromRegistry(t *testing.T) {
	joined := strings.Join(topLevelNames(commandCompletionModel()), " ")

	bash := renderCompletion(t, "bash")
	if !strings.Contains(bash, `local commands="`+joined+`"`) {
		t.Errorf("bash command list not the registry projection; got:\n%s", bash)
	}
	fish := renderCompletion(t, "fish")
	if !strings.Contains(fish, `__fish_use_subcommand' -a '`+joined+`'`) {
		t.Errorf("fish command list not the registry projection; got:\n%s", fish)
	}
	zsh := renderCompletion(t, "zsh")
	for _, name := range topLevelNames(commandCompletionModel()) {
		if !strings.Contains(zsh, "'"+name+":") {
			t.Errorf("zsh completion missing describe entry for %q", name)
		}
	}
}

// TestCompletionIncludesPreviouslyDriftedCommands pins the specific regression
// this ticket fixes: these eleven were absent from the hand-written bash literal
// before completion became a registry projection.
func TestCompletionIncludesPreviouslyDriftedCommands(t *testing.T) {
	drifted := []string{"assign", "backlog", "downgrade", "followup", "import", "lifeboat", "next", "orphaned", "prefix", "queue", "snapshots"}
	have := map[string]bool{}
	for _, name := range topLevelNames(commandCompletionModel()) {
		have[name] = true
	}
	for _, name := range drifted {
		if !have[name] {
			t.Errorf("completion model missing command %q that the registry registers", name)
		}
	}
}

// TestCompletionSubcommandsDerivedFromFamilies checks subcommand enumeration is
// projected from the family tables — including nested families — and excludes
// the deliberately hidden mirror entrypoint. [LAW:one-source-of-truth]
func TestCompletionSubcommandsDerivedFromFamilies(t *testing.T) {
	bash := renderCompletion(t, "bash")
	wants := []string{
		`sync)
      COMPREPLY=( $(compgen -W "status remote fetch pull push reconcile"`,
		`remote)
      COMPREPLY=( $(compgen -W "ls"`,
		`reconcile)
      COMPREPLY=( $(compgen -W "resolve abort"`,
	}
	for _, want := range wants {
		if !strings.Contains(bash, want) {
			t.Errorf("bash completion missing derived subcommand arm:\n%s\n--- full ---\n%s", want, bash)
		}
	}
}

// TestCompletionExcludesHiddenMirror guards the one row flagged hidden: a real,
// dispatchable subcommand that must never reach the advertised surface.
func TestCompletionExcludesHiddenMirror(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		out := renderCompletion(t, shell)
		if strings.Contains(out, backgroundMirrorSubcommand) {
			t.Errorf("%s completion leaks hidden subcommand %q", shell, backgroundMirrorSubcommand)
		}
	}
}

func TestRunHelpIncludesCompletion(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"help"}); err != nil {
		t.Fatalf("Run(help) error = %v", err)
	}
	help := normalizeWhitespace(stdout.String())
	if !strings.Contains(help, "completion Generate shell completion script") {
		t.Fatalf("help output missing completion command: %q", help)
	}
	if !strings.Contains(help, "ready List open work") {
		t.Fatalf("help output missing ready command: %q", help)
	}
}

func TestRunHelpDocumentsRankOrderingDefaults(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"help"}); err != nil {
		t.Fatalf("Run(help) error = %v", err)
	}
	help := normalizeWhitespace(stdout.String())
	if !strings.Contains(help, "ready List open work by readiness and rank") {
		t.Fatalf("help output missing rank-based ready description: %q", help)
	}
	if !strings.Contains(help, "ls List issues (rank by default)") {
		t.Fatalf("help output missing default rank ls description: %q", help)
	}
	if !strings.Contains(help, "children List child issues by rank") {
		t.Fatalf("help output missing rank-based children description: %q", help)
	}
}
