package main

import (
	"fmt"
	"io"
	"strings"
)

// bundleRule visually separates one module's section from the next in the
// flat THIRD_PARTY_LICENSES text file.
const bundleRule = "================================================================================"

// WriteBundle renders the third-party attribution bundle: one section per
// linked module, holding its module coordinates, classified license name, any
// curated note, and the full verbatim text of its license file. Section order
// follows entries exactly as given — LinkedModules already sorted it by module
// path, so this function adds no ordering logic of its own.
// [LAW:one-source-of-truth] entries carries the one canonical module order;
// this function doesn't re-derive or re-sort it.
//
// The note matters most here, of all three renderers. This is the file that
// legally accompanies the binary, so a dual-licensed component's verbatim text
// tells the recipient they may choose either license while saying nothing about
// which one lit chose. Without the note the election lives only in documents a
// recipient may never receive.
func WriteBundle(w io.Writer, entries []Entry) error {
	for _, e := range entries {
		_, err := fmt.Fprintf(w, "%s\n%s %s\n%sLicense: %s\n%s%s\n\n%s\n\n",
			bundleRule,
			e.Module.Path, e.Module.Version,
			sourceLine(e.Module.Replacement),
			e.LicenseName,
			noteLine(e.Note),
			bundleRule,
			strings.TrimRight(e.Text, "\n"),
		)
		if err != nil {
			return fmt.Errorf("write bundle entry for %s: %w", e.Module.Path, err)
		}
	}
	return nil
}

// sourceLine renders the fact that a section's source came from a coordinate
// other than the one its header names, directly beneath that header — the only
// place a reader of THIS file would look, since the bundle is a flat text
// document with no index and nothing else to consult.
//
// It sits immediately after the module line and before the license, because it
// qualifies WHICH COMPONENT the section is about, and every line below it
// describes that component. This file is the one that legally accompanies the
// binary, so it is the worst of the three artifacts in which to name a
// coordinate whose source the recipient would not get if they fetched it.
// [LAW:dataflow-not-control-flow] the unreplaced case is the empty string,
// which concatenates into the header as nothing at all, so WriteBundle keeps
// its single unconditional Fprintf.
func sourceLine(r Replacement) string {
	if r.Kind == NotReplaced {
		return ""
	}
	return "Source: " + r.String() + " (lit's go.mod substitutes this coordinate's source with a replace directive; see FORKS.md)\n"
}

// noteLine renders a curated note as its own header line, terminated so it
// slots between the license line and the closing rule. An entry without a note
// yields the empty string, which concatenates into the header as nothing at
// all. [LAW:dataflow-not-control-flow] the caller formats every section with
// one unconditional Fprintf; the presence of a note is a value flowing through
// it, not a branch around it — which is what keeps 149 note-free Go modules
// from each emitting a bare "Note:" line.
func noteLine(note string) string {
	if note == "" {
		return ""
	}
	return "Note: " + note + "\n"
}
