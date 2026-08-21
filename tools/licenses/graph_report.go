package main

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// graphRow is one reportable line of the module-graph audit.
type graphRow struct {
	Module  string
	Version string
	Path    string
	License string
}

// graphSection is a named group of rows. The report is built as a list of
// sections and then rendered by one loop, rather than as a sequence of
// hand-written blocks, so adding or reordering a category is a change to data
// and never to the rendering code. [LAW:dataflow-not-control-flow]
type graphSection struct {
	Title string
	Note  string
	Rows  []graphRow
}

// Section titles and the notes that tell a reader what each one means. They
// are constants rather than inline literals because the tests assert against
// the same names the report prints — a renamed section cannot pass a test that
// still expects the old title. [LAW:one-source-of-truth]
const (
	sectionReplaced     = "MODULES WHOSE SOURCE COMES FROM A DIFFERENT COORDINATE"
	sectionModuleGrants = "MODULE GRANTS OUTSIDE THE POLICY"
	sectionNestedTexts  = "NESTED LICENSE TEXTS OUTSIDE THE POLICY"
	sectionUnclassified = "LICENSE FILES THE CLASSIFIER COULD NOT IDENTIFY"
	sectionNoLicense    = "MODULES SHIPPING NO LICENSE FILE AT ALL"

	noteReplaced     = "A `replace` directive is in effect, so the go tool reports the ORIGINAL path and\nversion against the REPLACEMENT's directory. Every license below was read out of\nthe replacement's files and is listed under the original's name — which is also\nwhat a scanner resolving that coordinate will disagree with. Listed whether or\nnot the license is permissive, because the divergence is the finding."
	noteModuleGrants = "A module's own license grant, at its root. This is what a scanner that resolves\nmodule@version against a license database reports, and it is where a real\nobligation on this repository would appear."
	noteNestedTexts  = "License texts found deeper in a module's tree. This is what a file-walking\nscanner (FOSSA, Black Duck, most corporate SBOM pipelines) flags. Vendored test\ncorpora and dual-license option files land here and bind nobody — read the path\nbefore reading the license."
	noteUnclassified = "No SPDX license matched above the classifier's confidence threshold, so nobody\ncan say what these permit without reading them. Under an audit that asks for no\nquestions, an unclassifiable grant is a worse row than a known copyleft one."
	noteNoLicense    = "No license file was found anywhere in the module. Attribution for these cannot\nbe generated from the module source."
)

// rowsPerModule caps how many rows one module contributes to one section
// before the rest are counted instead of listed.
//
// It exists because of a specific, measured shape in this repository's graph:
// github.com/google/licenseclassifier — a license DETECTOR, whose payload is a
// reference corpus of every SPDX text — contributes 137 of the 144
// non-permissive nested rows, including AGPL-3.0 and every GPL variant. Printed
// in full it buries the seven rows that are actually about lit under a wall of
// a dependency's test data, and a report nobody can read is a report nobody
// reads. Three rows is enough to show what a module's contribution looks like;
// every genuine finding in this graph contributes one or two and so is never
// elided. The remainder is COUNTED, never dropped. [LAW:no-silent-failure]
const rowsPerModule = 3

// elidePerModule caps each module's run of rows at rowsPerModule, replacing
// what it drops with one row stating how many were held back. Rows arrive
// grouped by module (the inventory is resolved in sorted module order), so a
// single pass suffices and the output order is unchanged.
func elidePerModule(rows []graphRow, limit int) []graphRow {
	out := make([]graphRow, 0, len(rows))
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].Module == rows[i].Module {
			j++
		}
		run := rows[i:j]
		if len(run) <= limit {
			out = append(out, run...)
			i = j
			continue
		}
		out = append(out, run[:limit]...)
		out = append(out, graphRow{
			Module:  rows[i].Module,
			Version: rows[i].Version,
			Path:    fmt.Sprintf("... and %d more in this module", len(run)-limit),
			License: "(not listed)",
		})
		i = j
	}
	return out
}

// permitsHit reports whether a hit needs no reader, and it is deliberately
// stricter than LicenseFilter.Permits about where a module exception reaches.
//
// An allowlisted license is fine at any depth — a permissive license is
// permissive wherever it appears. An EXCEPTION is not: it records one human's
// reading of one file, and policy.json says which file in as many words
// ("Human-verified against the module's LICENSE"). Letting it excuse an
// arbitrary text deeper in the tree would suppress a license nobody ever
// looked at on the strength of having looked at a different one — and the rows
// it would suppress first are the unclassifiable ones, which this report calls
// the worst kind. Every current exception covers a module that ships only a
// root LICENSE, so this changes no output today; it is the rule that is wrong
// without it. [LAW:no-silent-failure]
func permitsHit(filter LicenseFilter, module string, h LicenseHit) bool {
	if filter.Allows(h.License) {
		return true
	}
	return h.IsRootGrant() && filter.Permits(module, h.License)
}

// partitionGraph sorts every hit into the section that describes what a reader
// must do about it, dropping everything the policy already permits.
//
// The split between a module's root grant and a text nested in its tree is the
// audit's central judgment, and it is derived from the hit's path rather than
// decided here, so the report cannot come to disagree with the inventory about
// where a license was found. [LAW:one-source-of-truth]
func partitionGraph(entries []GraphEntry, filter LicenseFilter) []graphSection {
	sections := []graphSection{
		{Title: sectionReplaced, Note: noteReplaced},
		{Title: sectionModuleGrants, Note: noteModuleGrants},
		{Title: sectionNestedTexts, Note: noteNestedTexts},
		{Title: sectionUnclassified, Note: noteUnclassified},
		{Title: sectionNoLicense, Note: noteNoLicense},
	}
	const (
		replaced = iota
		grants
		nested
		unclassified
		noLicense
	)

	for _, e := range entries {
		if e.Module.IsReplaced() {
			sections[replaced].Rows = append(sections[replaced].Rows, graphRow{
				Module: e.Module.Path, Version: e.Module.Version,
				Path: "source read from " + e.Module.ReplacedBy, License: rootGrantLicense(e.Hits),
			})
		}
		if len(e.Hits) == 0 {
			sections[noLicense].Rows = append(sections[noLicense].Rows, graphRow{
				Module: e.Module.Path, Version: e.Module.Version, Path: "-", License: "-",
			})
			continue
		}
		for _, h := range e.Hits {
			if permitsHit(filter, e.Module.Path, h) {
				continue
			}
			row := graphRow{Module: e.Module.Path, Version: e.Module.Version, Path: h.RelPath, License: h.License}
			switch {
			case h.License == unclassifiedLicense || h.License == oversizeLicense:
				sections[unclassified].Rows = append(sections[unclassified].Rows, row)
			case h.IsRootGrant():
				sections[grants].Rows = append(sections[grants].Rows, row)
			default:
				sections[nested].Rows = append(sections[nested].Rows, row)
			}
		}
	}
	return sections
}

// rootGrantLicense names the license a coordinate scanner would report for a
// module: the one carried by a license file at its root. A module with several
// root files (LICENSE plus LICENSE.docs, say) has no single such answer, and
// saying so is more useful than picking one silently. [LAW:no-silent-failure]
func rootGrantLicense(hits []LicenseHit) string {
	var found []string
	for _, h := range hits {
		if h.IsRootGrant() {
			found = append(found, h.License)
		}
	}
	switch len(found) {
	case 0:
		return "(no root grant)"
	case 1:
		return found[0]
	default:
		return fmt.Sprintf("%s (+%d more root files)", found[0], len(found)-1)
	}
}

// WriteGraphReport renders the module-graph audit: how much was measured, then
// every license the committed policy does not already permit, grouped by what
// a reader has to do about it.
//
// It reports and does not gate, and that is a decision the measurement made
// rather than a step left undone. Run against this repository, the graph's
// non-permissive rows are dominated by material that binds nobody — a license
// DETECTOR's reference corpus of every SPDX text, a vendored libc conformance
// suite, the unchosen half of a dual license — while the genuine obligations
// number a handful. A gate wired to fail on all of it would fail on master on
// the day it landed, for reasons that are mostly noise, and a gate that cries
// wolf gets switched off within a month. Deciding WHICH of these rows a gate
// should fail on is what the written finding this report feeds exists to
// settle. [LAW:verifiable-goals] done here means the graph is measured by a
// re-runnable tool instead of by hand; it does not mean the build now enforces
// what the measurement found.
func WriteGraphReport(w io.Writer, entries []GraphEntry, filter LicenseFilter) error {
	hits := 0
	for _, e := range entries {
		hits += len(e.Hits)
	}

	sections := partitionGraph(entries, filter)
	reported := 0
	for _, s := range sections {
		reported += len(s.Rows)
	}

	// "found", not "classified": a handful of hits are files past
	// maxLicenseFileSize that were recorded without ever being read, and a
	// count that called those classified would overstate what the audit knows.
	if _, err := fmt.Fprintf(w,
		"license graph audit: %d modules in the go.mod build list, %d license texts found, %d rows needing a reader\n",
		len(entries), hits, reported); err != nil {
		return err
	}

	for _, s := range sections {
		if _, err := fmt.Fprintf(w, "\n%s (%d)\n%s\n\n", s.Title, len(s.Rows), s.Note); err != nil {
			return err
		}
		// An empty section still prints its heading and an explicit "none",
		// so a reader can tell "this audit found nothing here" apart from
		// "this audit does not look here". [LAW:no-silent-failure]
		if len(s.Rows) == 0 {
			if _, err := fmt.Fprintln(w, "  none"); err != nil {
				return err
			}
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range elidePerModule(s.Rows, rowsPerModule) {
			if _, err := fmt.Fprintf(tw, "  %s@%s\t%s\t%s\n", r.Module, r.Version, r.Path, r.License); err != nil {
				return err
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// runGraph is the CLI entry for the module-graph audit: resolve and classify
// the full go.mod build list, then report every license the policy does not
// permit. It writes no artifacts and shares both the classifier and the policy
// with the link-closure gate, so "permissive" means exactly one thing across
// the two modes. [LAW:single-enforcer] [LAW:effects-at-boundaries]
func runGraph(stdout io.Writer) error {
	entries, err := buildGraphEntries()
	if err != nil {
		return err
	}
	policy, err := LoadPolicy()
	if err != nil {
		return err
	}
	return WriteGraphReport(stdout, entries, policy.Filter())
}
