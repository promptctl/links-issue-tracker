package cli

import (
	"fmt"
	"os"
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
//   - Anything else: treated as a lifecycle point (a moment spelled as a
//     `when:` entry — `enter <state>`, `exit <state>`, an event — or, falling
//     through, a label) with no definition bound there yet. A fresh file is
//     scaffolded under `.lit/workflows/`, commented to show each activation
//     key, with the requested one live.
//
// Neither shape ever overwrites an existing file — scaffolding always writes
// only under the project's `.lit/workflows/`, never into the global or
// embedded layers, and never clobbers unrelated project content silently.
// [LAW:no-silent-failure]
func runWorkflowsEdit(inv Invocation, ws workspace.Info, target string) error {
	set := workflows.Load(inv.Env, ws.RootDir)
	if def, ok := set.Lookup(target); ok {
		return editExistingDefinition(inv, ws, def)
	}
	return editFreshDefinition(inv, ws, target)
}

func editExistingDefinition(inv Invocation, ws workspace.Info, def workflows.Definition) error {
	projectPath := projectWorkflowPath(ws, def.Path)
	if def.Source == templates.SourceProject {
		return openOrPrintWorkflowFile(inv, projectPath)
	}
	if err := refuseExistingFile(projectPath, def.ID); err != nil {
		return err
	}
	raw, err := workflows.RawDefault(inv.Env, def.Source, def.Path)
	if err != nil {
		return err
	}
	if err := writeWorkflowScaffold(projectPath, def.ID, raw); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(inv.Stdout, "scaffolded override for %q (was %s) -> %s\n", def.ID, def.Source, projectPath); err != nil {
		return err
	}
	return openOrPrintWorkflowFile(inv, projectPath)
}

func editFreshDefinition(inv Invocation, ws workspace.Info, point string) error {
	key, liveLine := classifyWorkflowPoint(point)
	projectPath := projectWorkflowPath(ws, scaffoldFilenameSlug(point)+".md")
	if err := refuseExistingFile(projectPath, point); err != nil {
		return err
	}
	if err := writeWorkflowScaffold(projectPath, point, workflows.ScaffoldFresh(key, liveLine)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(inv.Stdout, "scaffolded a new definition at %s\n", projectPath); err != nil {
		return err
	}
	return openOrPrintWorkflowFile(inv, projectPath)
}

func projectWorkflowPath(ws workspace.Info, relPath string) string {
	return filepath.Join(ws.RootDir, ".lit", "workflows", filepath.FromSlash(relPath))
}

// refuseExistingFile is the fast-path guard both scaffold shapes check
// before doing any work: never clobber a file that's already there, whatever
// put it there. It gives a quick, friendly rejection for the common
// (non-racing) case; writeWorkflowScaffold's O_EXCL open is the actual
// enforcer, since a stat-then-write here would leave a TOCTOU gap a
// concurrent `lit workflows edit` could win. [LAW:single-enforcer]
func refuseExistingFile(path string, subject string) error {
	if _, err := os.Stat(path); err == nil {
		return conflictError(subject, path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return nil
}

func conflictError(subject string, path string) error {
	return MergeConflictError{Message: fmt.Sprintf("cannot scaffold %q: %s already exists (edit it directly, or run `lit workflows` to see what's already loaded there)", subject, path)}
}

// writeWorkflowScaffold is the actual no-clobber enforcer: O_EXCL makes file
// creation atomic, so a file that appears between refuseExistingFile's check
// and this call — the race that check alone cannot close — fails the same
// way refuseExistingFile's fast path would, instead of silently overwriting
// whatever landed there. [LAW:single-enforcer]
func writeWorkflowScaffold(path string, subject string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return conflictError(subject, path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return file.Close()
}

// openOrPrintWorkflowFile always names the scaffolded/target path — cheap,
// and useful whether or not an editor also opens — then, only when stdin and
// stdout are both real terminals and $EDITOR is set, hands the file to it
// interactively. A non-interactive caller (an agent, a script, a pipe, or a
// test harness capturing output into a buffer) or a terminal with no $EDITOR
// configured gets just the path: enough to open the file themselves.
func openOrPrintWorkflowFile(inv Invocation, path string) error {
	if _, err := fmt.Fprintln(inv.Stdout, path); err != nil {
		return err
	}
	if !interactive(inv.Stdin, inv.Stdout) {
		return nil
	}
	editorCmd := strings.Fields(inv.Env.Get("EDITOR"))
	if len(editorCmd) == 0 {
		return nil
	}
	// The editor runs where lit ran, on the invocation's own streams: the
	// terminals interactive just found are the ones it reads and draws on.
	dir, err := inv.Dir.Path()
	if err != nil {
		return fmt.Errorf("open %s in $EDITOR: %w", path, err)
	}
	cmd := inv.Env.Command(dir, editorCmd[0], append(editorCmd[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inv.Stdin, inv.Stdout, inv.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("open %s in $EDITOR (%s): %w", path, inv.Env.Get("EDITOR"), err)
	}
	return nil
}

// unsafeScaffoldFilenameChars is everything a fresh scaffold's filename
// strips: anything but lowercase letters, digits, underscore, and hyphen —
// deliberately keeping underscore so an event name like "show_ticket"
// scaffolds to the readable "show_ticket.md" rather than "show-ticket.md",
// and "enter in_progress" to "enter-in_progress.md".
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
// loaded definition into the activation key it names, the one way `lit
// workflows --help` documents: a point workflows.ParseMoment reads as a moment
// this lit fires — `enter <state>` or `exit <state>` for any state, since
// states are open strings, or an event in the catalog — is a `when:` entry;
// anything else is a label, since labels are open vocabulary too.
// [LAW:single-enforcer] the point is read by the same parser as a `when:`
// entry, so the two spellings cannot drift.
func classifyWorkflowPoint(point string) (key string, liveLine string) {
	if moment, ok := workflows.ParseMoment(point); ok && moment.Known() {
		return "when", fmt.Sprintf("when: [%s]", yamlDoubleQuoted(moment.String()))
	}
	return "labels", fmt.Sprintf("labels: [%s]", yamlDoubleQuoted(point))
}

// yamlDoubleQuoted wraps a value as a YAML double-quoted scalar — the one
// style whose only special characters are `\` and `"` (a flow-context
// character like `,` or `]` is literal inside the quotes) — so a point
// containing a comma, bracket, or colon embeds safely into the flow sequence
// ScaffoldFresh's live key line renders. [LAW:single-enforcer] every
// value embedded into scaffolded frontmatter routes through this one
// escaper, applied uniformly rather than only when a special character
// happens to be present.
func yamlDoubleQuoted(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`
}
