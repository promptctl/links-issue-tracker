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
		_, err := fmt.Fprintf(w, "%s\n%s %s\nLicense: %s\n%s%s\n\n%s\n\n",
			bundleRule,
			e.Module.Path, e.Module.Version,
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
