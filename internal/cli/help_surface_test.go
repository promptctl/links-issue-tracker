package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// litWritesIntoConsumerRepos names the tracked entries a reader of lit's help
// output actually has, because `lit init` puts them there. Everything else
// tracked in this repository — docs/, internal/, README.md — exists here and
// nowhere the tool runs, so naming one in help text hands the reader a path
// that resolves only for a contributor standing in this source tree.
var litWritesIntoConsumerRepos = map[string]bool{
	"AGENTS.md": true,
	"CLAUDE.md": true,
}

// repoOnlyNames derives, from the repository itself, every top-level tracked
// entry help text must not name. Deriving beats listing: a directory added to
// this repo tomorrow is forbidden in help text the same day, with nobody
// remembering to extend a constant. [LAW:one-source-of-truth]
//
// Tracked entries only (git ls-files, not a directory walk) so the set is the
// same on every machine — a build output or a local scratch directory must not
// decide whether this gate passes. [LAW:verifiable-goals]
func repoOnlyNames(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		top, _, nested := strings.Cut(line, "/")
		if litWritesIntoConsumerRepos[top] || seen[top] {
			continue
		}
		seen[top] = true
		// A directory is only ever cited as a path, so the slash is part of what
		// makes the mention a pointer: `lit` the command is not `lit/` the
		// directory, and prose about "the docs" is not `docs/`. A file has no
		// such suffix, so its whole name is the token.
		if nested {
			top += "/"
		}
		names = append(names, top)
	}
	return names
}

// Help text must be readable where lit runs. `lit` is installed into other
// people's repositories, so a help string naming `docs/cli-reference.md` is a
// map of a territory the reader is not standing in — the pointer dead-ends for
// every reader except a contributor with this checkout open.
// [FRAMING:representation]
//
// The gate covers the whole help surface at once — every advertised command's
// rendered help, the embedded long-form descriptions, and the guidance
// templates `lit quickstart` prints — because the bug it pins out appeared in
// all three at the same time and is one mistake, not three.
func TestHelpNeverNamesAPathOnlyLitsOwnRepoHas(t *testing.T) {
	root := mustRepoRoot(t)
	forbidden := repoOnlyNames(t, root)
	if len(forbidden) == 0 {
		t.Fatal("repoOnlyNames derived an empty set; the gate would pass vacuously")
	}

	for name, body := range helpSurface(t, root) {
		for _, path := range forbidden {
			if !strings.Contains(body, path) {
				continue
			}
			t.Errorf("%s names %q, a path only lit's own checkout has; "+
				"carry the text in the help command system instead of pointing at a file the reader does not have", name, path)
		}
	}
}

// helpSurface collects every body of help text lit ships, keyed by a name that
// says where to go fix a failure.
//
// Two kinds of source, because neither covers the other. The rendered pages are
// the real composed answer a caller sees, which is the only way to catch a
// pointer smuggled in through a flag's usage string. The Go string literals
// catch every OTHER message lit can print — above all the usage errors, which
// hand the reader a pointer at the exact moment they got the invocation wrong
// and most need one that works. Those are unreachable from `--help` and
// unbounded to enumerate by invocation, so they are read where they are
// written. [LAW:behavior-not-structure] the behavioral scan is the primary; the
// literal scan covers the messages no fixed set of invocations can reach.
func helpSurface(t *testing.T, root string) map[string]string {
	t.Helper()
	surface := map[string]string{}

	for name, literal := range userFacingStringLiterals(t, root) {
		surface[name] = literal
	}

	// Every directory of text the binary embeds and prints at a user: the
	// long-form command help, the quickstart guidance templates, and the
	// workflow definitions `lit done` and its siblings inject. Keys are
	// repo-relative — two of these directories are named `defaults`, and a
	// base-name key would have silently dropped one of them behind the other.
	for _, dir := range []string{
		filepath.Join(root, "internal", "cli", "helptext"),
		filepath.Join(root, "internal", "templates", "defaults"),
		filepath.Join(root, "internal", "workflows", "defaults"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s) error = %v", dir, err)
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("ReadFile(%s) error = %v", path, readErr)
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				t.Fatalf("Rel(%s) error = %v", path, relErr)
			}
			surface[filepath.ToSlash(rel)] = string(body)
		}
	}

	for path, page := range renderedHelpPages(t) {
		surface["`lit "+path+" --help`"] = page
	}
	return surface
}

// shippedPackageDirs asks the toolchain which of this module's packages are
// compiled into the lit binary, as repo-relative directories. That set is the
// exact boundary of "text a consumer can be shown": repo tooling that never
// leaves this checkout — the law-token index generator, the doc-claims registry
// — writes for a reader standing here, and naming a repo path is right for it.
// Asking the build beats keeping an exemption list, which would have to be
// pruned by hand every time a package moved in or out of the binary.
// [LAW:one-source-of-truth]
func shippedPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.Dir}}", "./cmd/lit")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/lit: %v", err)
	}
	var dirs []string
	for _, dir := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		rel, relErr := filepath.Rel(root, dir)
		if relErr != nil || strings.HasPrefix(rel, "..") {
			continue // a dependency outside this module
		}
		dirs = append(dirs, dir)
	}
	if len(dirs) == 0 {
		t.Fatal("go list -deps named no in-module package; the literal scan would be vacuous")
	}
	return dirs
}

// userFacingStringLiterals returns every string literal the shipped binary can
// print, keyed by file:line. Import paths are excluded — the module path
// contains `internal/`, so every import would otherwise report itself — and so
// are comments, whose reader is a contributor with this checkout open.
func userFacingStringLiterals(t *testing.T, root string) map[string]string {
	t.Helper()
	literals := map[string]string{}
	fset := token.NewFileSet()

	for _, dir := range shippedPackageDirs(t, root) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s) error = %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				t.Fatalf("parse %s: %v", path, parseErr)
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				t.Fatalf("Rel(%s) error = %v", path, relErr)
			}
			for _, decl := range file.Decls {
				if gen, isGen := decl.(*ast.GenDecl); isGen && gen.Tok == token.IMPORT {
					continue
				}
				ast.Inspect(decl, func(node ast.Node) bool {
					lit, ok := node.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					value, unquoteErr := strconv.Unquote(lit.Value)
					if unquoteErr != nil {
						return true
					}
					// Keyed by line AND column: a message and its separator commonly
					// share a line — `fmt.Sprintf("... %s", strings.Join(x, ", "))` —
					// and a line-only key kept whichever came last, which is the
					// `", "`. That silently dropped 18% of the shipped literals,
					// the usage errors this scan exists for among them.
					// [LAW:one-source-of-truth] one key per literal, not per line.
					pos := fset.Position(lit.Pos())
					literals[fmt.Sprintf("%s:%d:%d", filepath.ToSlash(rel), pos.Line, pos.Column)] = value
					return true
				})
			}
		}
	}
	return literals
}

// renderedHelpPages runs `--help` on every advertised command path and returns
// what a caller sees. Rendering beats reading the sources: the page is composed
// from a description, a usage line and a flag table, and a pointer smuggled in
// through any one of them reaches the reader the same way.
// [LAW:behavior-not-structure]
func renderedHelpPages(t *testing.T) map[string]string {
	t.Helper()
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

	pages := map[string]string{}
	for _, path := range append(commandGroupPaths(), commandLeafPaths()...) {
		var stdout, stderr bytes.Buffer
		args := append(append([]string{}, path...), "--help")
		if runErr := Run(context.Background(), &stdout, &stderr, args); runErr != nil {
			t.Fatalf("Run(%v) error = %v, want help answered as success", args, runErr)
		}
		pages[strings.Join(path, " ")] = stdout.String()
	}
	return pages
}

// `lit help <cmd>` and `lit <cmd> --help` are two spellings of one question, so
// they must not be two answers. They were: cobra rendered the first from a Long
// description and its own empty flag set — telling a reader that `lit import`
// accepts only `--help` — while the second rendered the leaf's real flags and no
// description. [LAW:one-source-of-truth]
func TestHelpCommandAndHelpFlagRenderTheSamePage(t *testing.T) {
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

	for _, path := range append(commandGroupPaths(), commandLeafPaths()...) {
		flagSpelling := render(t, append(append([]string{}, path...), "--help"))
		commandSpelling := render(t, append([]string{"help"}, path...))
		if flagSpelling != commandSpelling {
			t.Errorf("`lit %s --help` and `lit help %s` disagree:\n--help:\n%s\nhelp:\n%s",
				strings.Join(path, " "), strings.Join(path, " "), flagSpelling, commandSpelling)
		}
	}
}

func render(t *testing.T, args []string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), &stdout, &stderr, args); err != nil {
		t.Fatalf("Run(%v) error = %v, want help answered as success", args, err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("Run(%v) stderr = %q, want empty — a help answer is not an error", args, got)
	}
	return stdout.String()
}

// A command whose declaration names a help file that is not embedded must fail
// loudly at that declaration rather than shipping a silently blank description.
// [LAW:no-silent-failure]
func TestHelpTextPanicsOnAMissingFile(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("helpText on an unembedded name returned instead of panicking")
		}
	}()
	_ = helpText("no-such-command")
}

// `lit help <unknown>` must refuse, not answer. Rewriting the topic into
// `lit <unknown> --help` would hand it to cobra's root help, which prints the
// whole command list and exits 0 (links-cli-yn14) — the question "what is this
// command" answered with an answer-shaped non-answer.
func TestHelpRefusesATopicThatNamesNoCommand(t *testing.T) {
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

	var stdout, stderr bytes.Buffer
	runErr := Run(context.Background(), &stdout, &stderr, []string{"help", "nosuchcommand"})

	var unknown UnknownCommandError
	if !errors.As(runErr, &unknown) {
		t.Fatalf("Run([help nosuchcommand]) error = %v (stdout %q), want UnknownCommandError", runErr, stdout.String())
	}
	if unknown.Command != "nosuchcommand" {
		t.Errorf("UnknownCommandError.Command = %q, want the topic the caller typed", unknown.Command)
	}
	if got := stdout.String(); got != "" {
		t.Errorf("Run([help nosuchcommand]) stdout = %q, want nothing — a refusal is not a help page", got)
	}
}

// `lit help help` asks about cobra's own built-in command. Cobra registers it
// lazily inside ExecuteC, so a registered-command scan that runs earlier does
// not see it — and the first version of this rewrite refused `lit help help`
// as unknown while its own remediation told the caller to run
// `lit help <command>`. The advertised-path tests cannot catch that: `help` is
// not a registry row, so nothing else in this package ever types it.
func TestHelpAnswersForCobrasOwnHelpCommand(t *testing.T) {
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

	var stdout, stderr bytes.Buffer
	if runErr := Run(context.Background(), &stdout, &stderr, []string{"help", "help"}); runErr != nil {
		t.Fatalf("Run([help help]) error = %v, want help answered as success", runErr)
	}
	if stdout.Len() == 0 {
		t.Error("Run([help help]) printed nothing; a help request must be answered")
	}
	if got := stderr.String(); got != "" {
		t.Errorf("Run([help help]) stderr = %q, want empty — a help answer is not an error", got)
	}
}
