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
//
// The two replacement shapes say different things, for the same reason the SBOM
// pedigree does: a module target is a coordinate the recipient can fetch, and a
// directory target is not published anywhere, so a line that named it without
// saying so would invite a reader to go looking for something that does not
// exist. [LAW:one-source-of-truth] the wording differs from the SBOM's notes
// because the media differ, but neither may state what the other denies.
func sourceLine(r Replacement) string {
	switch r.Kind {
	case NotReplaced:
		return ""
	case ReplacedByFork:
		return headerLine("Source", r.String()+sourceSuffixFork)
	case ReplacedByVersion:
		return headerLine("Source", r.String()+sourceSuffixVersion)
	case ReplacedByDirectory:
		return headerLine("Source", r.String()+sourceSuffixDirectory)
	default:
		panic(fmt.Sprintf(unhandledReplacementKind, r.Kind, r.Path))
	}
}

// The parenthetical each Source line carries. They are named constants for the
// same reason their SBOM counterparts (pedigreeNoteFork and friends) are: the
// two documents describe one fact, and a bare literal buried in a switch arm is
// the easy thing to reword until they disagree.
//
// The subject is stated as "the module named above" rather than "this
// coordinate", because the coordinate nearest the reader's eye is the
// SUBSTITUTE printed immediately to the left — so a deictic there reads as
// saying the fork's source was replaced by itself, the reverse of the truth.
//
// The directory wording claims no containment. modfile.IsDirectoryPath accepts
// `../sibling` and absolute paths as readily as `./internal/...`, so a line
// asserting the copy sits "inside lit's own repository" would be false for a
// sibling checkout or /opt/src — a claim the format cannot back.
const (
	sourceSuffixFork      = " (a go.mod replace directive substitutes the source of the module named above with that fork; see FORKS.md in lit's source repository)"
	sourceSuffixVersion   = " (a go.mod replace directive substitutes the source of the module named above with the same module at that version; no fork is involved)"
	sourceSuffixDirectory = " (a go.mod replace directive substitutes the source of the module named above with that patched local directory; no published coordinate identifies it — see FORKS.md in lit's source repository)"
)

// noteLine renders a curated note as its own header line. An entry without a
// note yields the empty string, which concatenates into the header as nothing
// at all — which is what keeps 149 note-free Go modules from each emitting a
// bare "Note:" line.
func noteLine(note string) string { return headerLine("Note", note) }

// headerLine is the one renderer of a labelled, optional line in a bundle
// section's header, terminated so it slots between the lines around it. Both
// callers had their own copy of "empty means emit nothing, otherwise label,
// colon, space, value, newline" until the second one arrived and made the
// duplication visible. [LAW:one-source-of-truth]
//
// [LAW:dataflow-not-control-flow] absence is a value this returns, so
// WriteBundle formats every section with ONE unconditional Fprintf rather than
// branching around the lines a given section happens to lack.
func headerLine(label, value string) string {
	if value == "" {
		return ""
	}
	return label + ": " + value + "\n"
}
