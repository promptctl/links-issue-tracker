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
package docclaims

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"regexp"
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
func ShippedLiterals(fsys fs.FS) (map[string]bool, error) {
	literals := map[string]bool{}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if shippedSkipDir(name) {
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
	return literals, nil
}

// shippedSkipDir names the directories outside the shipped binary. Keeping it a
// predicate rather than a walk condition means one place decides what "ships"
// means here. [LAW:single-enforcer]
func shippedSkipDir(name string) bool {
	base := path.Base(name)
	// Any dotted directory, which is what keeps .claude/worktrees out. Those
	// hold entire checkouts of other branches, and a literal deleted from this
	// tree but alive in one of them would read as still shipping — the gate
	// would go green over precisely the drift it exists to catch.
	if name != "." && strings.HasPrefix(base, ".") {
		return true
	}
	switch base {
	case "testdata", "tools", "doc-v1-total", "docs", "design-docs", "scratchpad":
		return true
	case "docclaims":
		// This package itself. manifest_gen.go holds every documented literal
		// as a Go string, so counting it as shipped would let each entry match
		// itself: the gate would pass over any drift, and a regenerated
		// manifest could never drop a stale entry. [LAW:no-silent-failure]
		return true
	}
	return false
}

// collectLiterals parses one file and records its multi-word string literals.
// A file that does not parse is skipped rather than failing the walk: this
// package is a documentation check, and it must not be the thing that reports a
// Go syntax error the compiler and every other gate will report better.
func collectLiterals(name string, src []byte, into map[string]bool) error {
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		return nil
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

// Matched keeps the claims whose text appears in a shipped literal, which is
// what makes the manifest self-maintaining: a chapter that starts quoting a
// real message joins the protected set on the next sync, and one that never
// quoted code is simply absent rather than allowlisted.
//
// A claim matches when a shipped literal contains it. Containment rather than
// equality, because a chapter routinely quotes the sentence a format string
// carries while the literal also holds the verb around it.
func Matched(claims []Claim, shipped map[string]bool) []Claim {
	var out []Claim
	for _, c := range claims {
		if containedIn(c.Text, shipped) {
			out = append(out, c)
		}
	}
	return out
}

// Missing reports the manifest entries whose text no longer appears in any
// shipped literal — the documented messages that have drifted.
func Missing(manifest []Claim, shipped map[string]bool) []Claim {
	var out []Claim
	for _, c := range manifest {
		if !containedIn(c.Text, shipped) {
			out = append(out, c)
		}
	}
	return out
}

func containedIn(text string, shipped map[string]bool) bool {
	if shipped[text] {
		return true
	}
	for lit := range shipped {
		if strings.Contains(lit, text) {
			return true
		}
	}
	return false
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
