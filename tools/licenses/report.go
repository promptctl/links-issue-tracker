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
	// The legend must be true of EVERY row it describes, which rules out the
	// two obvious phrasings. "`-` means the source is the module in the first
	// column" is false on the four native-library rows, whose first column is a
	// bare name (`icu`, `musl`) naming no module at all. And "names another
	// coordinate" is false for a directory replacement, whose cell holds a
	// directory path that is deliberately NOT a coordinate — the very
	// distinction Replacement was introduced to keep. "repository-relative" is
	// wrong too: modfile.IsDirectoryPath accepts `../sibling` and absolute
	// paths, neither of which is inside this repository.
	if _, err := fmt.Fprintf(w, "The Source column names where a component's source came from when that is not the component named in the first column. It reads `-` for every component built from its own named source, and otherwise holds either a module coordinate or a local directory path, depending on what lit's `go.mod` substitutes with its `replace` directive — see FORKS.md in lit's source repository for what those substitutions change and why.\n\n"); err != nil {
		return err
	}

	// The Source column carries the fact that a `replace` directive built this
	// row from somewhere other than the coordinate in the Module column. It is
	// rendered for every row — "-" where the two agree — rather than as a
	// footnote or a separate section, because a substitution is a property OF
	// THE ROW, and a reader scanning the table must not have to know that a
	// second section exists in order to learn it. [LAW:dataflow-not-control-flow]
	if _, err := fmt.Fprintf(w, "| Module | Version | License | Source |\n|---|---|---|---|\n"); err != nil {
		return err
	}
	counts := make(map[string]int, len(entries))
	var noted []Entry
	for _, e := range entries {
		if _, err := fmt.Fprintf(w, "| %s | %s | %s | %s |\n", e.Module.Path, cellOrDash(e.Module.Version), e.LicenseName, cellOrDash(e.Module.Replacement.String())); err != nil {
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
			if _, err := fmt.Fprintf(w, "- **%s %s** — %s\n", e.Module.Path, cellOrDash(e.Module.Version), e.Note); err != nil {
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

// cellOrDash renders a value that is legitimately absent for some rows into a
// markdown table cell. Two columns need it: Version, because parseModuleList
// (modules.go) deliberately accepts an empty one, and Source, which is empty
// for every component whose source is the module naming it — nearly all of
// them. An empty cell renders as invisible whitespace, silently breaking the
// table's column alignment and leaving a reader unable to tell "nothing here"
// from "this column stopped applying". [LAW:no-silent-failure] the gap is
// rendered, not hidden. [LAW:one-source-of-truth] both columns spell an absent
// value the same way, because a reader should not have to learn two
// conventions for one idea.
func cellOrDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
