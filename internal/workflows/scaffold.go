package workflows

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
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
// new --when <moment> --label <label>`: each key the command names is written
// live with its entries, a key it leaves out is written commented as a
// reference, and a placeholder body follows.
// [LAW:dataflow-not-control-flow] every scaffold writes the same lines in the
// same order, each key once; whether a key is live with its entries or
// commented with an example is the value that varies.
// [LAW:comments-carry-meaning] the comments exist to teach the format inline,
// at the moment a user is about to author their first line of it.
func ScaffoldFresh(when []Moment, labels []string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("# when: lists the moments the definition fires at, labels: narrows it to\n")
	b.WriteString("# tickets carrying a label. Entries within one key are alternatives (OR);\n")
	b.WriteString("# declaring both keys requires both (AND).\n")
	b.WriteString("# Run `lit workflows --help` for the full format and every moment.\n")
	b.WriteString(keyLine("when", Spellings(when), "[enter review, exit plan, show_ticket]  # entering or exiting a state, or an event") + "\n")
	b.WriteString(keyLine("labels", labels, "[needs-design, blocked]") + "\n")
	b.WriteString("# id: my-custom-id     # optional; defaults to this file's relative path under .lit/workflows/\n")
	b.WriteString("# name: My Guidance    # optional pretty name shown by `lit workflows`\n")
	b.WriteString("---\n")
	b.WriteString("Write the guidance to inject here. `<id>` is replaced with the acted-on ticket's id when there is one.\n")
	return []byte(b.String())
}

// keyLine is one key's frontmatter line: live with its entries quoted, or,
// for a key with none, commented with example entries.
func keyLine(key string, entries []string, example string) string {
	if len(entries) == 0 {
		return "# " + key + ": " + example
	}
	quoted := make([]string, len(entries))
	for i, entry := range entries {
		quoted[i] = strconv.Quote(entry)
	}
	return key + ": [" + strings.Join(quoted, ", ") + "]"
}
