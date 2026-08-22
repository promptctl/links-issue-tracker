package main

import (
	"fmt"
	"io"
	"sort"
)

// WriteReport renders the human-readable license report: a module-to-license
// table for every linked module, followed by a per-license summary count.
// Table order follows entries exactly as given (see WriteBundle); only the
// summary re-groups, since a count-by-license has no natural module order of
// its own.
func WriteReport(w io.Writer, entries []Entry) error {
	if _, err := fmt.Fprintf(w, "# Third-Party License Report\n\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%d third-party components (Go modules and statically-linked native libraries) are compiled into this binary. Full license texts accompany this report in THIRD_PARTY_LICENSES.\n\n", len(entries)); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "| Module | Version | License |\n|---|---|---|\n"); err != nil {
		return err
	}
	counts := make(map[string]int, len(entries))
	var noted []Entry
	for _, e := range entries {
		if _, err := fmt.Fprintf(w, "| %s | %s | %s |\n", e.Module.Path, versionCell(e.Module.Version), e.LicenseName); err != nil {
			return fmt.Errorf("write report row for %s: %w", e.Module.Path, err)
		}
		counts[e.LicenseName]++
		if e.Note != "" {
			noted = append(noted, e)
		}
	}

	// Notes sit directly under the table they annotate: the dual-license
	// elections and compound-expression provenance a License cell alone can't
	// carry (Entry.Note). Rendering them here is what makes an election visible
	// to a reader of the shipped report, not only to a reader of our source.
	if len(noted) > 0 {
		if _, err := fmt.Fprintf(w, "\n## Notes\n\n"); err != nil {
			return err
		}
		for _, e := range noted {
			if _, err := fmt.Fprintf(w, "- **%s %s** — %s\n", e.Module.Path, versionCell(e.Module.Version), e.Note); err != nil {
				return fmt.Errorf("write note for %s: %w", e.Module.Path, err)
			}
		}
	}

	if _, err := fmt.Fprintf(w, "\n## Summary\n\n| License | Count |\n|---|---|\n"); err != nil {
		return err
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(w, "| %s | %d |\n", name, counts[name]); err != nil {
			return fmt.Errorf("write summary row for %s: %w", name, err)
		}
	}
	return nil
}

// versionCell renders a module version for a markdown table cell. An empty
// version — parseModuleList (modules.go) deliberately accepts one, for a
// local `replace` directive with no version — would otherwise leave the cell
// invisibly empty in rendered markdown, silently breaking the table's visual
// column alignment. [LAW:no-silent-failure] the gap is rendered, not hidden.
func versionCell(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
