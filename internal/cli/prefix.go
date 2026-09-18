package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// prefixSetResult is the outcome of `lit prefix set`. Both the preview path
// (--apply omitted) and the applied path produce the same struct so the text
// renderer reads typed fields rather than re-deriving them.
type prefixSetResult struct {
	Previous string
	Current  string
	Applied  bool
	Note     string
}

// prefixFamily is the `lit prefix` surface: one legal first argument, `set`.
// The hand-rolled args[0] test it replaces was a second copy of what every
// other family gets from resolve — legal-name lookup, the shared usage string,
// and a help request answered as help rather than as a usage error.
// [LAW:one-type-per-behavior] one subcommand is still a subcommand family.
// prefixSetUsage is the one spelling of prefix set's shape. It was written
// twice — once as the family's usage and once inside the leaf — which is the
// duplication this ticket exists to remove, and the copy the arity refusal
// needed was a third that nobody wrote. [LAW:one-source-of-truth]
const prefixSetUsage = "usage: lit prefix set <new-prefix> [--apply]"

var prefixFamily = commandFamily[wsSubcommand]{
	usage: prefixSetUsage,
	subcommands: []subcommandRow[wsSubcommand]{
		{name: "set", payload: wsSubcommand{declare: prefixSetLeaf}},
	},
}

func prefixSetLeaf() wsLeaf {
	fs := newCobraFlagSet("prefix set")
	apply := fs.Bool("apply", false, "Apply the rename (without this flag, prints a preview)")
	return wsLeaf{fs: fs, positionals: 1, usage: prefixSetUsage, work: func(ctx context.Context, stdout io.Writer, ws workspace.Info, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: prefixSetUsage}
		}
		requested := strings.TrimSpace(positional[0])
		// [LAW:single-enforcer] workspace.ConfiguredPrefix is the one boundary that
		// mints a valid prefix; the CLI never normalizes on its own.
		spec, err := workspace.ConfiguredPrefix(requested)
		if err != nil {
			// Typed for the same reason the init path is: a prefix the rules refuse
			// is refused identically on every rerun, so it must not reach the
			// default's retry-then-doctor advice. Untyped, the two sibling commands
			// classified this one condition two different ways. [LAW:no-silent-failure]
			return ValidationError{Message: fmt.Sprintf("invalid prefix %q: %v", requested, err)}
		}
		normalized := spec.Value()

		previous := ws.IssuePrefix.Value()
		if normalized == previous {
			result := prefixSetResult{
				Previous: previous,
				Current:  previous,
				Applied:  false,
				Note:     "prefix unchanged",
			}
			return prefixSetTextOutput(stdout, result)
		}

		if !*apply {
			result := prefixSetResult{
				Previous: previous,
				Current:  normalized,
				Applied:  false,
				Note:     "preview only — pass --apply to write config.json. Existing issue IDs keep their old prefix; only new issues use the new one.",
			}
			return prefixSetTextOutput(stdout, result)
		}

		if _, err := workspace.UpdateConfig(ws.ConfigPath, func(cfg workspace.Config) (workspace.Config, error) {
			cfg.IssuePrefix = normalized
			return cfg, nil
		}); err != nil {
			return fmt.Errorf("update workspace config: %w", err)
		}

		result := prefixSetResult{
			Previous: previous,
			Current:  normalized,
			Applied:  true,
		}
		return prefixSetTextOutput(stdout, result)
	}}
}

func prefixSetTextOutput(w io.Writer, r prefixSetResult) error {
	if r.Applied {
		_, err := fmt.Fprintf(w, "issue_prefix: %s -> %s (applied)\n", r.Previous, r.Current)
		return err
	}
	if r.Previous == r.Current {
		_, err := fmt.Fprintf(w, "issue_prefix: %s (%s)\n", r.Current, r.Note)
		return err
	}
	if _, err := fmt.Fprintf(w, "issue_prefix: %s -> %s (preview)\n", r.Previous, r.Current); err != nil {
		return err
	}
	if r.Note != "" {
		if _, err := fmt.Fprintf(w, "  %s\n", r.Note); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, "  Run with --apply to write config.json.")
	return err
}
