// docclaims-sync regenerates internal/docclaims/manifest_gen.go: the record of
// which quoted message literals in doc-v1-total were last seen to match code
// that actually ships.
//
// Run it from the repository root after deliberately changing a documented
// message:
//
//	go run ./tools/docclaims-sync
//
// The diff it produces is the point. An entry that disappears is a sentence in
// the specification that no longer describes the binary, and reviewing that
// diff is how the chapter and the code are re-tied. Regenerating to silence
// TestDocumentedClaimsStillShip without reading what left the manifest is the
// one use that defeats the gate.
//
// Unlike tools/lawtokens-sync this reads only the working tree — no network, so
// it behaves the same in CI, in a container, and on a laptop offline.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
)

const manifestPath = "internal/docclaims/manifest_gen.go"

// check reports whether to verify the committed manifest instead of rewriting
// it, without writing: the form to reach for in a script, or before committing
// a regeneration.
//
// It is not what guards CI. TestDocumentedClaimsStillShip asks the same question on
// every run and is the single enforcer of it; a nightly job running this flag
// would be a second answer to one question, which is the shape of drift this
// package exists to remove. What it adds is a check you can run deliberately —
// the omission it covers is real, since a manifest generated over a dirty
// working tree carrying a gitignored vendored project was committed once and
// broke every clean checkout. [LAW:single-enforcer]
var check = flag.Bool("check", false, "verify the committed manifest matches a regeneration; write nothing")

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "docclaims-sync:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}

	// Derived against the committed manifest so an entry keeps the source it
	// was anchored to while that source still carries the quotation. Without
	// it, any shorter literal added anywhere retargets unrelated entries.
	derived, err := docclaims.Derive(os.DirFS(root), docclaims.Manifest)
	if err != nil {
		return err
	}
	matched := derived.Fresh()
	if len(matched) == 0 {
		// An empty manifest turns the gate off while leaving every sign of it
		// in place, which is worse than failing. [LAW:no-silent-failure]
		return fmt.Errorf("no documented quotation matched shipped text; refusing to trust an empty derivation")
	}

	cmp := derived.Compare(docclaims.Manifest)
	if *check {
		return verify(cmp)
	}

	return write(manifestPath, matched, cmp)
}

// write records a fresh derivation, unless doing so would erase the evidence
// that a documented message stopped shipping.
//
// The write is the one action that can turn this gate green over prose that is
// now false, and it was the only report in the package making no comparison at
// all: a contributor sent here by a legitimate failure, who also had an
// unrelated message that had genuinely stopped shipping, took both away in
// silence and left every check green. It refuses instead, which is also the
// order CONTRIBUTING prescribes — correct the chapter first, then regenerate.
// [LAW:no-silent-failure]
//
// The refusal has no override, and that is the design rather than an omission.
// A review asked for one on the grounds that a documented false-positive class
// reaches it: `SHOW CREATE TABLE` is anchored to a SQL COMMENT in
// 00001_baseline.sql, so reflowing that comment fails the gate naming three
// chapters the edit has nothing to do with. The noise is real; the deadlock is
// not. Master is green or the freshness test is red, so an entry reaching this
// refusal was stopped by something in the contributor's own working tree, and
// both remedies it names are in their hands — restore the message, or correct
// the three sentences, which by then genuinely are describing text no binary
// carries. A flag that writes past it restores the exact hazard the refusal
// closes, a legitimate regeneration carrying an unrelated stopped message away
// with every check green, at the cost of typing one more word. The right answer
// to the noise is to anchor a quotation to its span rather than to a whole
// asset, which is links-doc-v1-tepa, not a hole in the one path that can turn
// this gate green over prose that is false.
func write(manifestPath string, matched []docclaims.Claim, cmp docclaims.Comparison) error {
	if stopped := cmp.Stopped(); len(stopped) > 0 {
		for _, d := range stopped {
			fmt.Fprintf(os.Stderr, "  %s\n", d.Explain())
		}
		return fmt.Errorf("refusing to write: %d documented message(s) the specification still quotes no longer ship. Regenerating would drop them and leave those sentences describing a binary that does not have them — correct the chapter, or restore the message, then run this again",
			len(stopped))
	}
	// A re-anchor is the ordinary edit — a literal reworded around a quotation
	// the chapter still makes — so it is reported rather than refused. Reported
	// it must be: writing moves the entry's Src, no entry leaves the manifest,
	// and "an entry leaving the manifest is the review signal" is what
	// CONTRIBUTING tells a reviewer to watch. Before this, the prescribed
	// command settled in silence the one case the package's own report calls
	// "only a reader can settle", and both checks went green over it.
	//
	// The sentence is the writer's own rather than Drift.Explain(), which asks
	// a reader to confirm a rewording and then run this tool — the wrong tense
	// for the tool that is running. That is not a second remedy competing with
	// the first: the remedies stayed single-homed in Explain, and this reports
	// an action already taken. [LAW:no-silent-failure]
	//
	// Which is why it is reported after the write and not before. Said first,
	// the past tense is a guess: a failing WriteFile — a read-only checkout, a
	// full disk — would print "re-anchored" about a manifest that was never
	// touched, and the reader would go looking in a diff that does not exist.
	if err := os.WriteFile(manifestPath, []byte(render(matched)), 0o644); err != nil {
		return err
	}
	if moved := cmp.Reanchored(); len(moved) > 0 {
		fmt.Fprintf(os.Stderr, "docclaims-sync: %d entry(ies) re-anchored — the quotation still ships, inside different words:\n", len(moved))
		for _, d := range moved {
			fmt.Fprintf(os.Stderr, "  %s %q is now carried by %q\n", d.Claim.Doc, d.Claim.Text, d.NowBrief())
		}
		fmt.Fprintln(os.Stderr, "  Read the manifest diff: each of these is this tool judging the new literal to be the same message. If one of them is a different string that happens to contain the words, the chapter quoting it is now describing a message the binary no longer has.")
	}
	fmt.Printf("docclaims-sync: %d documented quotations across %d files -> %s\n",
		len(matched), countDocs(matched), manifestPath)
	return nil
}

func countDocs(claims []docclaims.Claim) int {
	seen := map[string]bool{}
	for _, c := range claims {
		seen[c.Doc] = true
	}
	return len(seen)
}

func render(claims []docclaims.Claim) string {
	var b strings.Builder
	b.WriteString("// Code generated by tools/docclaims-sync. DO NOT EDIT.\n")
	b.WriteString("//\n")
	b.WriteString("// Each entry is a message literal that doc-v1-total quotes and that was\n")
	b.WriteString("// present in the shipped Go at the time of generation. TestDocumentedClaimsStillShip\n")
	b.WriteString("// fails when one of them no longer ships, which is a chapter describing a\n")
	b.WriteString("// message the binary no longer has.\n\n")
	b.WriteString("package docclaims\n\n")
	b.WriteString("// Manifest is the documented-literal record. Regenerate with\n")
	b.WriteString("// `go run ./tools/docclaims-sync`; never hand-edit.\n")
	b.WriteString("var Manifest = []Claim{\n")
	for _, c := range claims {
		fmt.Fprintf(&b, "\t{Doc: %s, Text: %s, Src: %s},\n",
			strconv.Quote(c.Doc), strconv.Quote(c.Text), strconv.Quote(c.Src))
	}
	b.WriteString("}\n")
	return b.String()
}

// verify reports the comparison between the committed manifest and a fresh
// derivation, naming the entries that differ rather than only reporting that
// they do.
func verify(cmp docclaims.Comparison) error {
	got := docclaims.Manifest
	if cmp.Clean() {
		fmt.Printf("docclaims-sync: manifest is current (%d quotations)\n", len(got))
		return nil
	}
	for _, c := range cmp.Added {
		fmt.Fprintf(os.Stderr, "  only in a fresh derivation: %s %q\n", c.Doc, c.Text)
	}
	// Each line carries its own instruction, from the same Explain the
	// freshness test prints, because two reports of one failure that word the
	// remedy differently are how a contributor learns to ignore both.
	// [LAW:single-enforcer] Reanchored() is what decides a re-anchor, here as
	// in the write path. Counting `Kind == AnchorMoved` inline again is a
	// second copy of that rule, and the two would diverge silently the first
	// time the classification changes.
	moved := len(cmp.Reanchored())
	for _, d := range cmp.Drifted {
		fmt.Fprintf(os.Stderr, "  %s\n", d.Explain())
	}
	// The exit line counts what actually differs. Reporting the two totals
	// instead stated them as evidence of a difference even when they were
	// equal — one chapter dropping a quotation while another adds one is an
	// ordinary prose edit, and "committed 1102, the tree yields 1102" is not
	// something a reader can act on.
	switch stopped := len(cmp.Stopped()); {
	case stopped > 0:
		return fmt.Errorf("%d documented message(s) no longer ship: fix the code or the chapter. Regenerating would drop them and leave the specification false",
			stopped)
	case moved > 0:
		return fmt.Errorf("%d documented message(s) are no longer carried by the source they were recorded against: read the lines above and confirm each is the same message reworded before regenerating",
			moved)
	}
	return fmt.Errorf("manifest is stale: %d recorded quotation(s) the tree no longer yields, %d the tree yields that it does not record; run `go run ./tools/docclaims-sync`",
		len(cmp.Drifted), len(cmp.Added))
}
