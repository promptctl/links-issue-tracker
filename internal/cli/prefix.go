package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
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
	// Census is what the ids already in the store use. It rides on every
	// outcome because the decision it informs — is this a repair of config.json
	// or a rewrite of it — is made at the preview and checked after the apply.
	Census idPrefixCensus
}

// prefixFamily is the `lit prefix` surface: one legal first argument, `set`.
// It gets from resolve what every other family gets — legal-name lookup, the
// shared usage string, and a help request answered as help rather than as a
// usage error. [LAW:one-type-per-behavior] one subcommand is still a
// subcommand family.
// prefixSetUsage is the one spelling of prefix set's shape: the family's usage,
// the leaf's usage and the arity refusal all read it. [LAW:one-source-of-truth]
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
			// default's retry-then-doctor advice. Untyped, the two sibling
			// commands would classify this one condition two different ways.
			// [LAW:no-silent-failure]
			return model.ValidationError{Message: fmt.Sprintf("invalid prefix %q: %v", requested, err)}
		}
		census, err := readWorkspaceIDPrefixCensus(ctx, ws)
		if err != nil {
			return err
		}
		normalized := spec.Value()
		// Stored, not Mintable: this command is the repair for a stored prefix
		// the rules refuse, so it has to run in that state and name what it is
		// replacing.
		previous := string(ws.IssuePrefix.Stored())
		if normalized == previous {
			result := prefixSetResult{
				Previous: previous,
				Current:  previous,
				Applied:  false,
				Note:     "prefix unchanged",
				Census:   census,
			}
			return prefixSetTextOutput(stdout, result)
		}

		if !*apply {
			result := prefixSetResult{
				Previous: previous,
				Current:  normalized,
				Applied:  false,
				Note:     "preview only — pass --apply to write config.json. Existing issue IDs keep their old prefix; only new issues use the new one.",
				Census:   census,
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
			Census:   census,
		}
		return prefixSetTextOutput(stdout, result)
	}}
}

func prefixSetTextOutput(w io.Writer, r prefixSetResult) error {
	head := fmt.Sprintf("issue_prefix: %s -> %s (preview)", r.Previous, r.Current)
	switch {
	case r.Applied:
		head = fmt.Sprintf("issue_prefix: %s -> %s (applied)", r.Previous, r.Current)
	case r.Previous == r.Current:
		head = fmt.Sprintf("issue_prefix: %s (%s)", r.Current, r.Note)
	}
	if _, err := fmt.Fprintf(w, "%s\n  issue ids in this store use: %s\n", head, r.Census); err != nil {
		return err
	}
	if r.Applied || r.Previous == r.Current {
		return nil
	}
	if _, err := fmt.Fprintf(w, "  %s\n", r.Note); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "  Run with --apply to write config.json.")
	return err
}
