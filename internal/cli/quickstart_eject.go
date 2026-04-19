package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/templates"
)

type ejectResult struct {
	Name    string // canonical filename (e.g. "quickstart.md")
	Path    string // absolute path written
	Changed bool   // true if file was created or overwritten
	Skipped string // non-empty reason if the file was intentionally not written
}

// ejectTemplates writes the embedded default(s) for the selected templates to
// their global override paths. When force is false the operation is atomic:
// if any target already exists, nothing is written and every conflict is
// reported. With force=true, every target is (over)written.
func ejectTemplates(selection string, force bool) ([]ejectResult, error) {
	names, err := resolveEjectSelection(selection)
	if err != nil {
		return nil, err
	}
	// [LAW:dataflow-not-control-flow] Plan every target first (same operations, same order, every invocation); the write phase runs only when the plan is entirely clean, so the observable state is all-or-nothing.
	plans := make([]ejectResult, 0, len(names))
	hasConflict := false
	for _, name := range names {
		plan, planErr := planEject(name, force)
		if planErr != nil {
			return nil, planErr
		}
		plans = append(plans, plan)
		if plan.Skipped != "" {
			hasConflict = true
		}
	}
	if hasConflict {
		// Atomic abort: Changed must stay a truthful "was written" signal.
		for i := range plans {
			if plans[i].Skipped == "" {
				plans[i].Changed = false
			}
		}
		return plans, nil
	}
	for i := range plans {
		if err := writeEject(&plans[i]); err != nil {
			return plans, err
		}
	}
	return plans, nil
}

func planEject(name string, force bool) (ejectResult, error) {
	path := templates.GlobalPath(name)
	if strings.TrimSpace(path) == "" {
		return ejectResult{}, fmt.Errorf("eject %s: no global config directory configured", name)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		if !force {
			return ejectResult{Name: name, Path: path, Changed: false, Skipped: "exists"}, nil
		}
		return ejectResult{Name: name, Path: path, Changed: true}, nil
	} else if !os.IsNotExist(statErr) {
		return ejectResult{}, fmt.Errorf("eject %s: stat %s: %w", name, path, statErr)
	}
	return ejectResult{Name: name, Path: path, Changed: true}, nil
}

func writeEject(plan *ejectResult) error {
	content, err := templates.EmbeddedDefault(plan.Name)
	if err != nil {
		return fmt.Errorf("eject %s: read embedded default: %w", plan.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o755); err != nil {
		return fmt.Errorf("eject %s: create dir: %w", plan.Name, err)
	}
	if err := os.WriteFile(plan.Path, content, 0o644); err != nil {
		return fmt.Errorf("eject %s: write %s: %w", plan.Name, plan.Path, err)
	}
	return nil
}

// resolveEjectSelection parses the --eject flag value into canonical filenames.
// Empty string means "not invoked" and is rejected by the caller; "all" returns
// all managed templates; anything else is treated as a comma-separated list of
// short aliases.
func resolveEjectSelection(selection string) ([]string, error) {
	trimmed := strings.TrimSpace(selection)
	if trimmed == "" {
		return nil, fmt.Errorf("eject: empty selection")
	}
	if trimmed == "all" {
		return templates.Names(), nil
	}
	parts := strings.Split(trimmed, ",")
	resolved := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		name, err := templates.ResolveShortName(part)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		resolved = append(resolved, name)
	}
	return resolved, nil
}

func writeEjectReport(stdout io.Writer, results []ejectResult, force bool) error {
	conflicts := 0
	for _, r := range results {
		if r.Skipped == "exists" {
			conflicts++
		}
	}
	aborted := conflicts > 0
	for _, r := range results {
		switch {
		case r.Skipped == "exists":
			if _, err := fmt.Fprintf(stdout, "exists  %s (%s; pass --force to overwrite)\n", r.Name, r.Path); err != nil {
				return err
			}
		case aborted:
			if _, err := fmt.Fprintf(stdout, "skipped %s (not written; %d conflict(s) aborted the operation)\n", r.Name, conflicts); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintf(stdout, "ejected %s -> %s\n", r.Name, r.Path); err != nil {
				return err
			}
		}
	}
	if aborted {
		if force {
			return fmt.Errorf("eject aborted: unexpected conflicts reported with --force")
		}
		return fmt.Errorf("conflict: %d template(s) already exist; re-run with --force to overwrite", conflicts)
	}
	return nil
}
