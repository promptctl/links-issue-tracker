package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/templates"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// runWorkflowsEdit is `lit workflows edit <id-or-point>`'s whole job:
// scaffold a project-layer file to customize guidance, then hand it to the
// user. Two shapes, chosen by whether target resolves against an already-
// loaded definition: [LAW:dataflow-not-control-flow] both run the same
// scaffold-then-open pipeline; only which content gets scaffolded varies.
//
//   - An existing id: override it. A project-layer definition is already the
//     override — open it directly. A global/embedded one gets copied
//     verbatim into a new project-layer file at its own relative path, ready
//     to edit.
//   - Anything else: treated as a lifecycle point (an event name, a state
//     name optionally suffixed `:enter`/`:exit`, or — falling through — a
//     label) with no definition bound there yet. A fresh file is scaffolded
//     under `.lit/workflows/`, commented to show every activation dimension,
//     with the requested one live.
//
// Neither shape ever overwrites an existing file — scaffolding always writes
// only under the project's `.lit/workflows/`, never into the global or
// embedded layers, and never clobbers unrelated project content silently.
// [LAW:no-silent-failure]
func runWorkflowsEdit(stdout io.Writer, ws workspace.Info, target string) error {
	set := workflows.Load(ws.RootDir)
	if def, ok := set.Lookup(target); ok {
		return editExistingDefinition(stdout, ws, def)
	}
	return editFreshDefinition(stdout, ws, target)
}

func editExistingDefinition(stdout io.Writer, ws workspace.Info, def workflows.Definition) error {
	projectPath := projectWorkflowPath(ws, def.Path)
	if def.Source == templates.SourceProject {
		return openOrPrintWorkflowFile(stdout, projectPath)
	}
	if err := refuseExistingFile(projectPath, def.ID); err != nil {
		return err
	}
	raw, err := workflows.RawDefault(def.Source, def.Path)
	if err != nil {
		return err
	}
	if err := writeWorkflowScaffold(projectPath, raw); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "scaffolded override for %q (was %s) -> %s\n", def.ID, def.Source, projectPath); err != nil {
		return err
	}
	return openOrPrintWorkflowFile(stdout, projectPath)
}

func editFreshDefinition(stdout io.Writer, ws workspace.Info, point string) error {
	dimension, liveLine, slug := classifyWorkflowPoint(point)
	projectPath := projectWorkflowPath(ws, slug+".md")
	if err := refuseExistingFile(projectPath, point); err != nil {
		return err
	}
	if err := writeWorkflowScaffold(projectPath, workflows.ScaffoldFresh(dimension, liveLine)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "scaffolded a new definition at %s\n", projectPath); err != nil {
		return err
	}
	return openOrPrintWorkflowFile(stdout, projectPath)
}

func projectWorkflowPath(ws workspace.Info, relPath string) string {
	return filepath.Join(ws.RootDir, ".lit", "workflows", filepath.FromSlash(relPath))
}

// refuseExistingFile is the one guard both scaffold shapes route through
// before writing: never clobber a file that's already there, whatever put it
// there. [LAW:single-enforcer]
func refuseExistingFile(path string, subject string) error {
	if _, err := os.Stat(path); err == nil {
		return MergeConflictError{Message: fmt.Sprintf("cannot scaffold %q: %s already exists (edit it directly, or run `lit workflows` to see what's already loaded there)", subject, path)}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return nil
}

func writeWorkflowScaffold(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// openOrPrintWorkflowFile always names the scaffolded/target path — cheap,
// and useful whether or not an editor also opens — then, only when stdout is
// a real terminal and $EDITOR is set, hands the file to it interactively. A
// non-interactive caller (an agent, a script, a pipe, or a test harness
// capturing output into a buffer) or a terminal with no $EDITOR configured
// gets just the path: enough to open the file themselves.
func openOrPrintWorkflowFile(stdout io.Writer, path string) error {
	if _, err := fmt.Fprintln(stdout, path); err != nil {
		return err
	}
	if !isTerminal(stdout) {
		return nil
	}
	editorCmd := strings.Fields(os.Getenv("EDITOR"))
	if len(editorCmd) == 0 {
		return nil
	}
	cmd := exec.Command(editorCmd[0], append(editorCmd[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("open %s in $EDITOR (%s): %w", path, os.Getenv("EDITOR"), err)
	}
	return nil
}

// isTerminal reports whether w is a real terminal rather than a pipe, file
// redirect, or an in-memory buffer (what tests and any non-*os.File caller
// pass) — exactly the distinction `lit workflows edit` needs to decide
// whether opening $EDITOR makes sense. Checking against the actual stream
// the command is writing to (rather than the process-global os.Stdout) means
// a test capturing output into a bytes.Buffer never accidentally launches an
// editor. Needs no new dependency (golang.org/x/term is not already a direct
// one) — the character-device check is the standard no-dependency approach.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// unsafeScaffoldFilenameChars is everything a fresh scaffold's filename
// strips: anything but lowercase letters, digits, underscore, and hyphen —
// deliberately keeping underscore so an event name like "work_started"
// scaffolds to the readable "work_started.md" rather than "work-started.md".
var unsafeScaffoldFilenameChars = regexp.MustCompile(`[^a-z0-9_-]+`)

func scaffoldFilenameSlug(input string) string {
	slug := strings.ToLower(strings.TrimSpace(input))
	slug = unsafeScaffoldFilenameChars.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "point"
	}
	return slug
}

// classifyWorkflowPoint turns a `lit workflows edit` target that matched no
// loaded definition into the activation dimension it names, in one
// deterministic order documented in docs/workflows.md:
//
//  1. Explicit `<state>:enter` / `<state>:exit` — states are open strings by
//     design, so this needs no validation against a known set.
//  2. A name in the event catalog — an event.
//  3. One of the three built-in lifecycle states, bare (defaults to enter).
//  4. Anything else — a label; labels are open vocabulary too.
//
// [LAW:dataflow-not-control-flow] one function, one ordered set of checks;
// which branch fires is decided entirely by the input, never by a caller
// flag.
func classifyWorkflowPoint(point string) (dimension string, liveLine string, filenameSlug string) {
	if state, when, ok := parseStatePointSuffix(point); ok {
		return "states", stateActivationLine(state, when), stateFilenameSlug(state, when)
	}
	if workflows.Event(point).Known() {
		return "events", fmt.Sprintf("events: [%s]", point), scaffoldFilenameSlug(point)
	}
	if isBuiltinState(point) {
		return "states", stateActivationLine(point, workflows.WhenEnter), stateFilenameSlug(point, workflows.WhenEnter)
	}
	return "labels", fmt.Sprintf("labels: [%s]", point), scaffoldFilenameSlug(point)
}

func parseStatePointSuffix(point string) (state string, when workflows.When, ok bool) {
	name, suffix, found := strings.Cut(point, ":")
	if !found {
		return "", "", false
	}
	switch strings.ToLower(strings.TrimSpace(suffix)) {
	case "enter":
		return strings.TrimSpace(name), workflows.WhenEnter, true
	case "exit":
		return strings.TrimSpace(name), workflows.WhenExit, true
	default:
		return "", "", false
	}
}

func isBuiltinState(point string) bool {
	normalized := strings.ToLower(strings.TrimSpace(point))
	for _, state := range builtinStates {
		if state == normalized {
			return true
		}
	}
	return false
}

func stateActivationLine(state string, when workflows.When) string {
	if when == workflows.WhenExit {
		return fmt.Sprintf("states: [{name: %s, when: exit}]", state)
	}
	return fmt.Sprintf("states: [%s]", state)
}

func stateFilenameSlug(state string, when workflows.When) string {
	slug := scaffoldFilenameSlug(state)
	if when == workflows.WhenExit {
		return slug + "_exit"
	}
	return slug
}
