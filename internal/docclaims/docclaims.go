// Package docclaims ties the v1 specification's quoted message literals to the
// code that actually ships them.
//
// doc-v1-total describes what lit does, and where it quotes a user-facing
// message it is holding a second copy of a string whose original lives in Go.
// Two copies of one fact drift, and here they drifted silently: three merged
// tickets (links-claims-1b0p, links-cli-cpou, links-cli-q7hg) each falsified a
// documented claim, and all eleven CI checks stayed green over every one of
// them, because nothing in the repository compared the two.
// [LAW:one-source-of-truth] the Go literal is the fact; the chapter is a map of
// it, and this package is what re-checks the map.
//
// The check is deliberately one-directional. Every literal the manifest records
// must still appear in the shipped tree; a chapter is never required to quote
// any particular string. Drift is a documented quote that stops existing, which
// is exactly the failure the three tickets above produced, and it cannot be
// confused with prose that merely never quoted code.
//
// Scope: literals, not line numbers. Whether `file.go:12-33` still brackets the
// declaration its sentence names is a different question over a different
// corpus, owned by links-docs-gwlf. Whether a chapter's claim about a type's
// shape or a command's exit code still holds is a third, and this package does
// not answer it. CONTRIBUTING.md records what this gate covers and what it
// deliberately does not.
//
// One blind spot is worth naming here rather than leaving to be rediscovered: a
// message assembled by concatenation, "… some text " + v + " more text", is
// several literals to the parser and never seen whole. A chapter quoting across
// that seam matches nothing, so it never enters the manifest and is silently
// unprotected — indistinguishable from prose that quotes no code at all.
package docclaims

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// minClaimLen and the multi-word requirement are what separate a quoted message
// from the symbols and flags that fill these chapters. `NoWork`, `--take` and
// `cli.go` are all backticked and none of them is a claim about message text;
// a span short enough or single-word enough to be one of those is not worth
// protecting, because a match would as often be a coincidence as a quotation.
const minClaimLen = 12

// docSpanDouble matches a “…“ span, which the corpus uses when the quoted
// string itself contains a backtick. It is tried first: a single-backtick
// pattern would otherwise cut such a span at its inner backtick and protect a
// fragment rather than the message.
var docSpanDouble = regexp.MustCompile("``(.+?)``")

// docSpanSingle matches a `…` span that is not part of a “…“ pair. The
// lookarounds Go's regexp lacks are done by scanning, not by the pattern.
var docSpanSingle = regexp.MustCompile("`([^`\n]+)`")

// Claim is one documented quotation: the literal, and the chapter that quotes
// it. The file travels with the string so a failure names the page to correct
// rather than only the sentence that broke. [LAW:no-silent-failure]
type Claim struct {
	Doc  string
	Text string
	// Lit is the whole shipped literal this quotation was observed inside.
	//
	// Anchoring to it is what gives the gate its teeth. Asking only whether the
	// quoted words appear SOMEWHERE in the tree is far too weak: measured over
	// this corpus, 174 of 1,054 quotations are contained in two or more distinct
	// literals, one in twenty-two of them. Delete the exact query or message a
	// chapter cites and a coincidental substring elsewhere keeps the gate green
	// — the precise failure this package exists to end.
	// [LAW:types-are-the-program] the claim carries its own evidence.
	Lit string
}

// ShippedLiterals collects every multi-word string literal in the Go that
// ships, keyed by the literal itself.
//
// It reads literals from source rather than scanning rendered output, because
// every message this package exists to protect is an error-path string: a
// refusal, a diagnostic, a remediation. Nothing prints them on a successful
// run, so a gate built on rendered output would see none of them and report
// green. [LAW:no-silent-failure]
//
// Test files and the tools/ tree are excluded: neither ships in the binary the
// specification describes, and a literal that lives only in a test would let a
// documented message survive its own deletion from the product.
// shippedRoots is where this module's product code lives. A positive list, not
// a blacklist of what to skip: "everything in the tree except the names I
// thought of" admits every directory nobody enumerated, and this one admitted
// three before it was replaced — .claude/worktrees (whole checkouts of other
// branches), this package's own generated manifest, and artifacts/, a gitignored
// vendored copy of an unrelated project whose literals outnumbered lit's by
// three to one. [LAW:parse-dont-validate] the set is constructed, not filtered.
var shippedRoots = []string{"cmd", "internal"}

// selfPkg is the one path inside those roots that must not count as shipped.
// manifest_gen.go holds every documented literal as a Go string, so counting it
// would let each entry match itself and the gate would pass over any drift.
const selfPkg = "internal/docclaims"

// ShippedLiterals collects every multi-word string literal in the Go that
// ships, keyed by the literal itself.
//
// It reads literals from source rather than scanning rendered output, because
// every message this package exists to protect is an error-path string: a
// refusal, a diagnostic, a remediation. Nothing prints them on a successful
// run, so a gate built on rendered output would see none of them and report
// green. [LAW:no-silent-failure]
//
// Only tracked product code counts. Test files do not ship, and neither does
// tools/, so a literal living in either could otherwise let a documented
// message survive its own deletion from the product.
func ShippedLiterals(fsys fs.FS) (map[string]bool, error) {
	literals := map[string]bool{}
	for _, root := range shippedRoots {
		if _, err := fs.Stat(fsys, root); err != nil {
			// A root that is not present is not an empty corpus, it is a
			// misconfigured one, and silently returning fewer literals would
			// report every claim under it as drifted. [LAW:no-silent-failure]
			return nil, fmt.Errorf("shipped root %q: %w", root, err)
		}
		err := fs.WalkDir(fsys, root, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name == selfPkg || path.Base(name) == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			src, err := fs.ReadFile(fsys, name)
			if err != nil {
				return err
			}
			return collectLiterals(name, src, literals)
		})
		if err != nil {
			return nil, err
		}
	}
	return literals, nil
}

// collectLiterals parses one file and records its multi-word string literals.
// A parse failure is returned, not swallowed. Skipping the file would drop
// every literal that lived only there, and each of them would then be reported
// as a chapter quoting a message that no longer ships — prose that is in fact
// correct, named as false, with the real cause discarded. One unreadable error
// beats a cascade of confidently wrong ones. [LAW:no-silent-failure]
func collectLiterals(name string, src []byte, into map[string]bool) error {
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		// Unquote rather than trim: an escaped quote, a \n, or a \u in the
		// source is one character in the message a user reads, and the chapter
		// quotes the message. [LAW:parse-dont-validate]
		text, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if strings.Contains(text, " ") {
			into[text] = true
		}
		return true
	})
	return nil
}

// DocClaims collects every backticked span in the given markdown files that is
// long enough and wordy enough to be a quoted message.
//
// It reports candidates, not verified claims. Deciding which of them actually
// quote shipped code is Matched's job, and that split is the whole design: the
// manifest records only spans that were observed to match, so prose that never
// quoted code needs no allowlist and adds no upkeep.
func DocClaims(fsys fs.FS, names []string) ([]Claim, error) {
	var claims []Claim
	for _, name := range names {
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		for _, text := range spansIn(string(src)) {
			claims = append(claims, Claim{Doc: name, Text: text})
		}
	}
	return claims, nil
}

// spansIn pulls the candidate spans out of one document, double-backtick spans
// first so their contents cannot be re-cut by the single-backtick pattern.
func spansIn(src string) []string {
	src = stripFences(src)
	var out []string
	keep := func(text string) {
		text = strings.TrimSpace(text)
		if len(text) >= minClaimLen && strings.Contains(text, " ") {
			out = append(out, text)
		}
	}
	rest := docSpanDouble.ReplaceAllStringFunc(src, func(m string) string {
		keep(strings.TrimSuffix(strings.TrimPrefix(m, "``"), "``"))
		// Replaced by blanks of equal length so the remaining scan sees no
		// backticks here and no span straddles the hole left behind.
		return strings.Repeat(" ", len(m))
	})
	for _, m := range docSpanSingle.FindAllStringSubmatch(rest, -1) {
		keep(m[1])
	}
	return out
}

// Matched keeps the claims whose text appears in a shipped literal, recording
// which literal that was. This is what makes the manifest self-maintaining: a
// chapter that starts quoting a real message joins the protected set on the
// next sync, and one that never quoted code is simply absent rather than
// allowlisted.
func Matched(claims []Claim, shipped map[string]bool) []Claim {
	var out []Claim
	for _, c := range claims {
		lit, ok := tightest(c.Text, shipped)
		if !ok {
			continue
		}
		c.Lit = lit
		out = append(out, c)
	}
	return out
}

// tightest picks the shortest literal containing the quotation, ties broken
// lexicographically. Shortest is the tightest evidence — the literal that is
// most nearly the sentence itself rather than a larger one that happens to
// enclose it — and fixing the choice keeps the generated manifest a function of
// the tree alone. [LAW:dataflow-not-control-flow]
func tightest(text string, shipped map[string]bool) (string, bool) {
	best, found := "", false
	for lit := range shipped {
		if !strings.Contains(lit, text) {
			continue
		}
		if !found || len(lit) < len(best) || (len(lit) == len(best) && lit < best) {
			best, found = lit, true
		}
	}
	return best, found
}

// Missing reports the manifest entries that have drifted: the literal the
// quotation was recorded against no longer ships verbatim, or it no longer
// contains the words the chapter puts in quotes.
//
// Both halves are checked because either can rot on its own. A message can be
// deleted outright, or it can be reworded around a fragment the chapter quotes.
func Missing(manifest []Claim, shipped map[string]bool) []Claim {
	var out []Claim
	for _, c := range manifest {
		if !shipped[c.Lit] || !strings.Contains(c.Lit, c.Text) {
			out = append(out, c)
		}
	}
	return out
}

// SpecDir is the corpus this gate covers. It is named once so the sync tool and
// the CI test cannot disagree about which files are under the gate — a
// disagreement would regenerate a manifest the test never checks.
// [LAW:single-enforcer]
const SpecDir = "doc-v1-total"

// SpecFiles lists the markdown under SpecDir, in walk order.
func SpecFiles(fsys fs.FS) ([]string, error) {
	var names []string
	err := fs.WalkDir(fsys, SpecDir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(name, ".md") {
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// stripFences removes fenced code blocks before any span is read. What sits in
// a fence is a transcript — a shell command, a schema, an example session — not
// a chapter asserting what message the binary prints, and reading inside them
// put `go test -short ./...`, `set -euo pipefail` and `golangci-lint run` into
// the manifest as though they were claims about lit.
func stripFences(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	fenced := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			out = append(out, "")
			continue
		}
		if fenced {
			out = append(out, "")
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Dedupe sorts by document then text and drops repeats, so a derivation is a
// function of the tree alone and a re-run with no source change produces no
// diff.
//
// It lives here rather than in the sync tool because the tool and the freshness
// test must derive the manifest identically; when only the tool deduped, the
// two disagreed by 216 entries and the test demanded a regeneration that the
// tool had already performed. [LAW:single-enforcer]
func Dedupe(claims []Claim) []Claim {
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].Doc != claims[j].Doc {
			return claims[i].Doc < claims[j].Doc
		}
		return claims[i].Text < claims[j].Text
	})
	var out []Claim
	for i, c := range claims {
		if i > 0 && claims[i-1] == c {
			continue
		}
		out = append(out, c)
	}
	return out
}
