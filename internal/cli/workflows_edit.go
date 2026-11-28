package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/templates"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// runWorkflowsEdit is `lit workflows edit <id>`: hand the user the
// project-layer file of the definition with that id. A project-layer
// definition is already its own file — open it directly. A global or embedded
// one is copied verbatim into a new project-layer file at its own relative
// path, the override, ready to edit. An id nothing loaded holds is refused,
// naming the flags that scaffold a new definition (editFreshDefinition).
//
// Scaffolding never overwrites an existing file — it writes only under the
// project's `.lit/workflows/`, never into the global or embedded layers, and
// never clobbers unrelated project content silently. [LAW:no-silent-failure]
func runWorkflowsEdit(inv Invocation, ws workspace.Info, id string) error {
	def, ok := workflows.Load(inv.Env, ws.RootDir).Lookup(id)
	if !ok {
		return model.ValidationError{Message: fmt.Sprintf("no workflow definition with id %q (run `lit workflows` to see loaded ids); to scaffold a new one, name when it fires: `lit workflows edit --when \"enter closed\"`, `--label needs-design`, or both", id)}
	}
	return editExistingDefinition(inv, ws, def)
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

// editFreshDefinition is `lit workflows edit --when <moment> --label <name>`:
// scaffold a new project-layer definition firing on the keys the flags name,
// read as a definition's own keys are (readKeyFlags), in a file named after
// them.
func editFreshDefinition(inv Invocation, ws workspace.Info, whenTexts, labelTexts []string) error {
	when, labels, err := readKeyFlags(whenTexts, labelTexts)
	if err != nil {
		return err
	}
	name := scaffoldName(when, labels)
	projectPath := projectWorkflowPath(ws, name+".md")
	if err := refuseExistingFile(projectPath, name); err != nil {
		return err
	}
	if err := writeWorkflowScaffold(projectPath, name, workflows.ScaffoldFresh(when, labels)); err != nil {
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

// scaffoldName is a fresh scaffold's file name, without its .md: each entry
// it binds, as the file spells it, made filename-safe, joined by "+".
// [LAW:one-source-of-truth] the name comes from what the file binds, so two
// spellings of one binding (`enter in-progress`, `Enter In_Progress`) name one
// file, and the second scaffold is refused as the conflict it is.
func scaffoldName(when []workflows.Moment, labels []string) string {
	parts := append(workflows.Spellings(when), labels...)
	for i, part := range parts {
		parts[i] = scaffoldFilenameSlug(part)
	}
	return strings.Join(parts, "+")
}

func scaffoldFilenameSlug(entry string) string {
	slug := strings.Trim(unsafeScaffoldFilenameChars.ReplaceAllString(entry, "-"), "-")
	if slug == "" {
		return "definition"
	}
	return slug
}
