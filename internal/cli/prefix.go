package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/issueid"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

// prefixSetResult is the JSON-serialisable shape returned by `lit prefix set`.
// Both the preview path (--apply omitted) and the applied path emit the same
// struct so consumers can branch on Applied without parsing free text.
type prefixSetResult struct {
	Previous string `json:"previous"`
	Current  string `json:"current"`
	Applied  bool   `json:"applied"`
	Note     string `json:"note,omitempty"`
}

func runPrefix(stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return errors.New("usage: lit prefix set <new-prefix> [--apply] [--json]")
	}
	return runPrefixSet(stdout, ws, args[1:])
}

func runPrefixSet(stdout io.Writer, ws workspace.Info, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("prefix set")
	apply := fs.Bool("apply", false, "Apply the rename (without this flag, prints a preview)")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 || fs.NArg() != 0 {
		return errors.New("usage: lit prefix set <new-prefix> [--apply] [--json]")
	}
	requested := strings.TrimSpace(positional[0])
	normalized, err := issueid.NormalizeConfiguredPrefix(requested)
	if err != nil {
		return fmt.Errorf("invalid prefix %q: %w", requested, err)
	}

	previous := ws.IssuePrefix
	if normalized == previous {
		result := prefixSetResult{
			Previous: previous,
			Current:  previous,
			Applied:  false,
			Note:     "prefix unchanged",
		}
		return printValue(stdout, result, *jsonOut, prefixSetTextOutput)
	}

	if !*apply {
		result := prefixSetResult{
			Previous: previous,
			Current:  normalized,
			Applied:  false,
			Note:     "preview only — pass --apply to write config.json. Existing issue IDs keep their old prefix; only new issues use the new one.",
		}
		return printValue(stdout, result, *jsonOut, prefixSetTextOutput)
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
	return printValue(stdout, result, *jsonOut, prefixSetTextOutput)
}

func prefixSetTextOutput(w io.Writer, v any) error {
	r := v.(prefixSetResult)
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
