package workflows

import (
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// RawDefault returns the unparsed bytes of a definition as authored at a
// given non-project layer, keyed the same way Load resolves it: relPath is
// the layer-relative path Definition.Path carries. `lit workflows edit`
// scaffolds a project override by copying this verbatim, so the override
// starts as an exact copy of what's currently active rather than a
// re-derived reconstruction that could drift from the authored file.
// [LAW:one-source-of-truth]
//
// The project layer has no raw default to copy from — it IS the override
// target — so passing templates.SourceProject is a caller error.
func RawDefault(env environ.Env, source templates.Source, relPath string) ([]byte, error) {
	switch source {
	case templates.SourceGlobal:
		path := globalWorkflowsDir(env).Join(relPath)
		raw, err := os.ReadFile(path.String())
		if err != nil {
			return nil, fmt.Errorf("read global workflow default %s: %w", path, err)
		}
		return raw, nil
	case templates.SourceEmbedded:
		raw, err := fs.ReadFile(embeddedDefaultsFS, relPath)
		if err != nil {
			return nil, fmt.Errorf("read embedded workflow default %s: %w", relPath, err)
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("workflows: no raw default for the %s layer (it is the override target, not a source to copy from)", source)
	}
}

// ScaffoldFresh builds a fresh definition file's content for `lit workflows
// edit` when the requested id-or-point matches no loaded definition: a
// commented reference to each activation key (skipping the one the point
// itself supplies, since liveLine already shows it live) plus that one key
// uncommented and ready to fire, and a placeholder body.
// [LAW:comments-carry-meaning] the comments exist to teach the format inline,
// at the moment a user is about to author their first line of it.
func ScaffoldFresh(key string, liveLine string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("# when: lists the moments the definition fires at, labels: narrows it to\n")
	b.WriteString("# tickets carrying a label. Entries within one key are alternatives (OR);\n")
	b.WriteString("# declaring both keys requires both (AND).\n")
	b.WriteString("# Run `lit workflows --help` for the full format and every moment.\n")
	if key != "when" {
		b.WriteString("# when: [enter review, exit plan, show_ticket]  # entering or exiting a state, or an event\n")
	}
	if key != "labels" {
		b.WriteString("# labels: [needs-design, blocked]\n")
	}
	b.WriteString(liveLine)
	b.WriteString("\n")
	b.WriteString("# id: my-custom-id     # optional; defaults to this file's relative path under .lit/workflows/\n")
	b.WriteString("# name: My Guidance    # optional pretty name shown by `lit workflows`\n")
	b.WriteString("---\n")
	b.WriteString("Write the guidance to inject here. `<id>` is replaced with the acted-on ticket's id when there is one.\n")
	return []byte(b.String())
}
