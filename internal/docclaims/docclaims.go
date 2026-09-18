// Package docclaims ties the v1 specification's quoted messages to the code
// that actually ships them.
//
// doc-v1-total describes what lit does, and where it quotes a user-facing
// message it is holding a second copy of a string whose original lives in the
// product. Two copies of one fact drift, and here they drifted silently: three
// merged tickets (links-claims-1b0p, links-cli-cpou, links-cli-q7hg) each
// falsified a documented claim, and all eleven CI checks stayed green over
// every one of them, because nothing in the repository compared the two.
// [LAW:one-source-of-truth] the shipped text is the fact; the chapter is a map
// of it, and this package is what re-checks the map.
//
// The check is deliberately one-directional. Every quotation the manifest
// records must still ship; a chapter is never required to quote any particular
// string. Drift is a documented quote that stops existing, which is exactly the
// failure the three tickets above produced, and it cannot be confused with
// prose that merely never quoted code.
//
// Scope: text, not line numbers. Whether `file.go:12-33` still brackets the
// declaration its sentence names is a different question over a different
// corpus, owned by links-docs-gwlf. Whether a chapter's claim about a type's
// shape or a command's exit code still holds is a third, and this package does
// not answer it. CONTRIBUTING.md records what this gate covers and what it
// deliberately does not.
//
// One blind spot is worth naming rather than leaving to be rediscovered: a
// message assembled by concatenation, "… some text " + v + " more text", is
// several literals to the parser and is never seen whole. A chapter quoting
// across that seam matches nothing, so it never enters the manifest and is
// silently unprotected — indistinguishable from prose that quotes no code.
package docclaims

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// minClaimLen and the multi-word requirement are what separate a quoted message
// from the symbols and flags that fill these chapters. `NoWork`, `--take` and
// `cli.go` are all backticked and none of them is a claim about message text; a
// span short enough or single-word enough to be one of those is not worth
// protecting, because a match would as often be a coincidence as a quotation.
const minClaimLen = 12

// entryRoot is where this module's binaries live. The search for what ships
// starts here rather than at the module root because tools/ also holds main
// packages, and a development utility's messages never reach a user of lit.
const entryRoot = "cmd"

// docSpanDouble matches a “…“ span, which the corpus uses when the quoted
// string itself contains a backtick. It is tried first: a single-backtick
// pattern would otherwise cut such a span at its inner backtick and protect a
// fragment rather than the message.
var docSpanDouble = regexp.MustCompile("``(.+?)``")

// docSpanSingle matches a `…` span that is not part of a “…“ pair.
var docSpanSingle = regexp.MustCompile("`([^`\n]+)`")

// embedDirective matches a //go:embed line and captures its patterns.
var embedDirective = regexp.MustCompile(`^//go:embed\s+(.*)$`)

// Claim is one documented quotation: the text, the chapter that quotes it, and
// the shipped source it was observed in. The chapter travels with the quotation
// so a failure names the page to correct rather than only the sentence that
// broke. [LAW:no-silent-failure]
type Claim struct {
	Doc  string
	Text string
	// Src identifies the one shipped thing this quotation was found inside: a
	// Go string literal, recorded verbatim, or the path of an embedded text
	// asset.
	//
	// Anchoring to it is what gives the gate teeth. Asking only whether the
	// quoted words appear SOMEWHERE in the tree is far too weak: measured over
	// this corpus, 174 of the quotations are contained in two or more distinct
	// sources, one in twenty-two of them. Delete the exact message a chapter
	// cites and a coincidental substring elsewhere keeps the gate green — the
	// precise failure this package exists to end.
	// [LAW:types-are-the-program] the claim carries its own evidence.
	//
	// The two kinds do not anchor equally tightly, and the weaker one is worth
	// knowing about rather than discovering. A literal is the message itself,
	// so the check is nearly exact. An asset is keyed by path and matched
	// against its whole contents, so a quotation is only held to "still
	// somewhere in this file" — delete the sentence a chapter describes and a
	// stray recurrence elsewhere in the same file keeps it green. The assets
	// here are small enough (the largest is 6.3 KB) that the gap is narrow, and
	// closing it properly means anchoring to a span rather than a file.
	Src string
}

// Corpus is the shipped text a quotation can be checked against, keyed by the
// handle a Claim records in Src: a Go literal keys itself, an embedded asset
// keys its path. One map rather than two, so every caller asks the same
// question of both kinds. [LAW:no-mode-explosion]
type Corpus map[string]string

// ShippedText collects the text that reaches a user from the product: every
// multi-word Go string literal, and the contents of every embedded text asset.
//
// It reads sources rather than rendered output, because most messages this
// protects are error-path strings — a refusal, a diagnostic, a remediation —
// and nothing prints them on a successful run, so a gate built on rendered
// output would see none of them and report green. [LAW:no-silent-failure]
//
// Embedded assets are included because this repository is actively moving user
// text out of Go literals and into them: links-help-h0di moved whole help pages
// into internal/cli/helptext. A gate that read only literals would go blind in
// exactly the direction the corpus is travelling.
//
// Test files do not ship, and neither does tools/, so a message living only in
// either could otherwise survive its own deletion from the product.
func ShippedText(fsys fs.FS) (Corpus, error) {
	shipped, err := shippedPackages(fsys)
	if err != nil {
		return nil, err
	}
	corpus := Corpus{}
	for _, dir := range shipped {
		err := eachProductFile(fsys, dir, func(name string, src []byte) error {
			return collectFile(fsys, name, src, corpus)
		})
		if err != nil {
			return nil, err
		}
	}
	return corpus, nil
}

// shippedPackages lists this module's package directories that a binary under
// cmd/ actually links, by walking the import graph out from each main package.
//
// What this replaced was a hand-kept list of root directories, and it drifted
// twice inside one ticket: first admitting artifacts/, a gitignored checkout of
// an unrelated project, then internal/vendor/dolthub-driver, a separate module
// whose example program nothing imports. Neither was merely noise in the
// corpus. Matched anchors a quotation to the SHORTEST source containing it, so
// a coincidental copy of the words in an unlinked tree becomes the recorded
// evidence for a chapter's claim about lit's own message — and deleting that
// message then leaves the gate green, which is the precise failure this package
// exists to end.
//
// [LAW:one-source-of-truth] the import graph is where "what ships" is already
// written down; a list of directories is a second copy of that fact, and the
// copy is the half that rots. [LAW:parse-dont-validate] the set is constructed
// by following imports rather than filtered by naming trees to skip, so a
// vendored module dropped inside internal/ is out because nothing links it —
// not because somebody remembered to name it. This package's own generated
// manifest is excluded by the same rule, for the same reason.
//
// The test is linked AND sourced here, not "belongs to this module". A module
// replaced onto a local path is source this repository carries and ships:
// github.com/dolthub/driver lives in internal/vendor/dolthub-driver, and
// internal/store imports it, so its eighteen documented error messages reach a
// user of lit and are gated like any other. Its example/ program is in the same
// tree and is linked by nothing, so it is out. Being vendored was never the
// disqualifier — being unreachable is.
func shippedPackages(fsys fs.FS) ([]string, error) {
	sources, err := localSources(fsys)
	if err != nil {
		return nil, err
	}
	queue, err := entryPackages(fsys)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(queue))
	for _, dir := range queue {
		seen[dir] = true
	}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		imports, err := importsOf(fsys, dir)
		if err != nil {
			return nil, err
		}
		for _, imp := range imports {
			next, ok := sources.dir(imp)
			if !ok || seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}
	out := make([]string, 0, len(seen))
	for dir := range seen {
		out = append(out, dir)
	}
	// Sorted so a derivation is a function of the tree alone: map order would
	// otherwise decide which of two equally short sources an error names first.
	sort.Strings(out)
	return out, nil
}

// sources maps an import path to the directory in this tree that holds it, for
// every package whose source the repository carries.
//
// It is read from go.mod rather than stated here, because go.mod is where both
// facts are already declared — the module's own import prefix, and each module
// replaced onto a local path. A constant would be a second copy that a rename
// or a new vendored dependency silently falsifies, and the failure mode is
// quiet: an import that resolves to nothing is a package never walked, whose
// messages then report as drifted prose. [LAW:one-source-of-truth]
type sources []source

type source struct {
	prefix string // import path of the module
	dir    string // directory in this tree holding it, "" for the root module
}

// dir resolves an import path to the directory holding its source, reporting
// whether this tree holds it at all. Longest prefix wins, so a module replaced
// into a subdirectory of the root module resolves to the replacement rather
// than to the path it would otherwise occupy.
func (s sources) dir(imp string) (string, bool) {
	for _, src := range s {
		if imp == src.prefix {
			return src.dir, true
		}
		if rest, ok := strings.CutPrefix(imp, src.prefix+"/"); ok {
			return path.Join(src.dir, rest), true
		}
	}
	return "", false
}

func localSources(fsys fs.FS) (sources, error) {
	data, err := fs.ReadFile(fsys, "go.mod")
	if err != nil {
		return nil, fmt.Errorf("reading go.mod: %w", err)
	}
	var out sources
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			out = append(out, source{prefix: strings.TrimSpace(rest)})
			continue
		}
		// Every replacement is read, in either of go.mod's two spellings — a
		// bare `replace a => b` line and a line inside a `replace ( … )` block
		// — because the arrow is what makes it one, not the keyword.
		module, target, ok := strings.Cut(line, "=>")
		if !ok {
			continue
		}
		dir := strings.Fields(strings.TrimSpace(target))
		// A replacement onto another module is that module's source, fetched
		// into the module cache and not in this tree; only a filesystem path
		// names something here.
		if len(dir) == 0 || !strings.HasPrefix(dir[0], ".") {
			continue
		}
		prefix := strings.Fields(strings.TrimPrefix(strings.TrimSpace(module), "replace "))
		if len(prefix) == 0 {
			continue
		}
		out = append(out, source{prefix: prefix[0], dir: path.Clean(dir[0])})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("go.mod declares no module path, so no import can be identified as source this repository ships")
	}
	// Longest prefix first, so `dir` can return on its first match.
	sort.Slice(out, func(i, j int) bool { return len(out[i].prefix) > len(out[j].prefix) })
	return out, nil
}

// entryPackages finds the main packages under entryRoot — the binaries whose
// text reaches a user, and the roots of the import walk.
func entryPackages(fsys fs.FS) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, entryRoot, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isProductGo(name) {
			return err
		}
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.PackageClauseOnly)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", name, err)
		}
		if dir := path.Dir(name); file.Name.Name == "main" && !slices.Contains(out, dir) {
			out = append(out, dir)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		// An empty set of entry points is a misconfigured corpus, not an empty
		// product: every documented quotation would report as drifted, naming
		// the whole specification false rather than this walk broken.
		// [LAW:no-silent-failure]
		return nil, fmt.Errorf("no main package under %s/, so nothing is identifiable as shipping", entryRoot)
	}
	return out, nil
}

// importsOf returns the import paths of one package's product files.
func importsOf(fsys fs.FS, dir string) ([]string, error) {
	var out []string
	err := eachProductFile(fsys, dir, func(name string, src []byte) error {
		file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", name, err)
		}
		for _, spec := range file.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			out = append(out, imp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// eachProductFile calls fn for every Go file of one package that the product
// build compiles. One traversal for both callers, so the set of files whose
// imports are followed cannot differ from the set whose literals are collected
// — a package reached but not read would report its own messages as drifted.
// [LAW:single-enforcer]
func eachProductFile(fsys fs.FS, dir string, fn func(name string, src []byte) error) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("reading package %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := path.Join(dir, e.Name())
		if !isProductGo(name) {
			continue
		}
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if err := fn(name, src); err != nil {
			return err
		}
	}
	return nil
}

// isProductGo reports whether a file is Go source the product build compiles.
// A test file ships nothing, and the go tool ignores testdata entirely, so a
// package stored there is not reachable however it is imported.
func isProductGo(name string) bool {
	if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
		return false
	}
	return !slices.Contains(strings.Split(path.Dir(name), "/"), "testdata")
}

// collectFile parses one Go file, recording its multi-word string literals and
// the contents of whatever it embeds.
//
// A parse failure is returned, not swallowed. Skipping the file would drop
// every message that lived only there, and each would then be reported as a
// chapter quoting something that no longer ships — prose that is in fact
// correct, named as false, with the real cause discarded. One unreadable error
// beats a cascade of confidently wrong ones. [LAW:no-silent-failure]
func collectFile(fsys fs.FS, name string, src []byte, into Corpus) error {
	file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		// Unquote rather than trim: an escaped quote, a \n or a \u in the
		// source is one character in the message a user reads, and the chapter
		// quotes the message. [LAW:parse-dont-validate]
		text, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if strings.Contains(text, " ") {
			into[text] = text
		}
		return true
	})
	return collectEmbeds(fsys, path.Dir(name), file, into)
}

// collectEmbeds reads the assets a file embeds, keyed by their paths.
//
// The patterns are taken from the //go:embed directives themselves rather than
// guessed from file extensions: what ships is what the compiler is told to
// embed, and a .md beside it that nothing embeds does not.
func collectEmbeds(fsys fs.FS, dir string, file *ast.File, into Corpus) error {
	for _, group := range file.Comments {
		for _, c := range group.List {
			m := embedDirective.FindStringSubmatch(c.Text)
			if m == nil {
				continue
			}
			for _, pattern := range embedPatterns(m[1]) {
				// all: is what tells the compiler to include the names a bare
				// directory pattern omits, so it has to survive as far as the
				// walk that does the omitting.
				all := strings.HasPrefix(pattern, "all:")
				pattern = strings.TrimPrefix(pattern, "all:")
				if err := addEmbedded(fsys, path.Join(dir, pattern), all, into); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// embedPatterns splits a //go:embed directive's operands.
//
// Splitting on whitespace is not enough: go:embed accepts a quoted pattern, in
// either of Go's two string syntaxes, and a quoted pattern may contain spaces.
// Cutting one in half yields two patterns that match nothing, and a pattern
// matching nothing is silent here — the asset simply never enters the corpus,
// and every chapter quoting it is then reported as prose describing a message
// that no longer ships. A parse gap would arrive wearing the exact costume of
// the drift this package hunts. [LAW:no-silent-failure]
func embedPatterns(operands string) []string {
	var out []string
	rest := strings.TrimSpace(operands)
	for rest != "" {
		var token string
		switch rest[0] {
		case '"', '`':
			quote := rest[0]
			end := strings.IndexByte(rest[1:], quote)
			if end < 0 {
				token, rest = rest[1:], ""
				break
			}
			token, rest = rest[1:1+end], rest[2+end:]
			if quote == '"' {
				if unquoted, err := strconv.Unquote(`"` + token + `"`); err == nil {
					token = unquoted
				}
			}
		default:
			if i := strings.IndexAny(rest, " \t"); i >= 0 {
				token, rest = rest[:i], rest[i:]
			} else {
				token, rest = rest, ""
			}
		}
		if token != "" {
			out = append(out, token)
		}
		rest = strings.TrimSpace(rest)
	}
	return out
}

// addEmbedded resolves one embed pattern and records every text file it names.
// A pattern may name a directory, which embeds everything beneath it.
//
// all carries the directive's all: prefix, because the two differ in what a
// directory contributes: without it the go tool omits every name beginning with
// "." or "_" from the subtree. Measured against the compiler rather than read
// off the documentation, since the rule is not the one a reader expects — the
// omission applies to a directory pattern only, and an explicit glob like
// `defaults/*` does embed a _draft.md. Treating the two alike in either
// direction puts text in the corpus that no user can reach, or leaves out text
// that ships.
func addEmbedded(fsys fs.FS, pattern string, all bool, into Corpus) error {
	matches, err := fs.Glob(fsys, pattern)
	if err != nil {
		return fmt.Errorf("embed pattern %q: %w", pattern, err)
	}
	for _, match := range matches {
		info, err := fs.Stat(fsys, match)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if err := addAsset(fsys, match, into); err != nil {
				return err
			}
			continue
		}
		err = fs.WalkDir(fsys, match, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !all && name != match && omittedFromDirectoryEmbed(path.Base(name)) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			return addAsset(fsys, name, into)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// omittedFromDirectoryEmbed reports whether the go tool leaves a name out of a
// directory pattern's subtree. Verified against the compiler: `//go:embed sub`
// omits sub/_x.txt and sub/.y.txt, while `//go:embed d/*` embeds both.
func omittedFromDirectoryEmbed(base string) bool {
	return strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_")
}

// addAsset records one embedded file, skipping anything that is not text. A
// binary asset holds no sentence a chapter could quote, and indexing it would
// only invite coincidental byte matches.
func addAsset(fsys fs.FS, name string, into Corpus) error {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return nil
	}
	into[name] = string(data)
	return nil
}

// DocClaims collects every backticked span in the given markdown files that is
// long enough and wordy enough to be a quoted message.
//
// It reports candidates, not verified claims. Deciding which of them actually
// quote shipped text is Matched's job, and that split is the whole design: the
// manifest records only spans observed to match, so prose that never quoted
// code needs no allowlist and adds no upkeep.
func DocClaims(fsys fs.FS, names []string) ([]Claim, error) {
	var claims []Claim
	for _, name := range names {
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		spans, err := spansIn(string(src))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, text := range spans {
			claims = append(claims, Claim{Doc: name, Text: text})
		}
	}
	return claims, nil
}

// spansIn pulls the candidate spans out of one document, double-backtick spans
// first so their contents cannot be re-cut by the single-backtick pattern.
func spansIn(src string) ([]string, error) {
	stripped, err := stripFences(src)
	if err != nil {
		return nil, err
	}
	var out []string
	keep := func(text string) {
		text = strings.TrimSpace(text)
		if len(text) >= minClaimLen && strings.Contains(text, " ") {
			out = append(out, text)
		}
	}
	rest := docSpanDouble.ReplaceAllStringFunc(stripped, func(m string) string {
		keep(strings.TrimSuffix(strings.TrimPrefix(m, "``"), "``"))
		// Replaced by blanks of equal length so the remaining scan sees no
		// backticks here and no span straddles the hole left behind.
		return strings.Repeat(" ", len(m))
	})
	for _, m := range docSpanSingle.FindAllStringSubmatch(rest, -1) {
		keep(m[1])
	}
	return out, nil
}

// stripFences removes fenced code blocks before any span is read. What sits in
// a fence is a transcript — a shell command, a schema, an example session — not
// a chapter asserting what message the binary prints, and reading inside them
// put `go test -short ./...` and `set -euo pipefail` into the manifest as
// though they were claims about lit.
//
// An unclosed fence is an error rather than a silent truncation. Blanking the
// rest of the file would drop every claim below it, and the regeneration would
// look like an ordinary "entries left the manifest" diff — which CONTRIBUTING
// tells a reviewer means a sentence stopped describing the binary.
// [LAW:no-silent-failure]
func stripFences(src string) (string, error) {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	fenced := false
	opened := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			if !fenced {
				opened = i + 1
			}
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
	if fenced {
		return "", fmt.Errorf("unclosed code fence opened at line %d", opened)
	}
	return strings.Join(out, "\n"), nil
}

// Matched keeps the claims whose text appears in shipped text, recording which
// source that was. This is what makes the manifest self-maintaining: a chapter
// that starts quoting a real message joins the protected set on the next sync,
// and one that never quoted code is simply absent rather than allowlisted.
func Matched(claims []Claim, corpus Corpus) []Claim {
	var out []Claim
	for _, c := range claims {
		src, ok := tightest(c.Text, corpus)
		if !ok {
			continue
		}
		c.Src = src
		out = append(out, c)
	}
	return out
}

// tightest picks the shortest source containing the quotation, ties broken
// lexicographically by handle. Shortest is the tightest evidence — the literal
// that is most nearly the sentence itself rather than a larger one enclosing it
// — and fixing the choice keeps a derivation a function of the tree alone.
// [LAW:dataflow-not-control-flow]
func tightest(text string, corpus Corpus) (string, bool) {
	best, bestLen, found := "", 0, false
	for src, body := range corpus {
		if !strings.Contains(body, text) {
			continue
		}
		if !found || len(body) < bestLen || (len(body) == bestLen && src < best) {
			best, bestLen, found = src, len(body), true
		}
	}
	return best, found
}

// Stable re-uses each claim's previously recorded source whenever that source
// still ships and still carries the quotation.
//
// Without it, adding any shorter literal anywhere in the shipped tree silently
// retargets existing entries, and the freshness test then fails on a branch
// that changed no documented message — wording indistinguishable from real
// drift. That trains a contributor to regenerate without reading the diff,
// which is the one habit that defeats this gate.
func Stable(fresh, prior []Claim, corpus Corpus) []Claim {
	was := make(map[[2]string]string, len(prior))
	for _, c := range prior {
		was[[2]string{c.Doc, c.Text}] = c.Src
	}
	out := make([]Claim, 0, len(fresh))
	for _, c := range fresh {
		if src, ok := was[[2]string{c.Doc, c.Text}]; ok && stillHolds(src, c.Text, corpus) {
			c.Src = src
		}
		out = append(out, c)
	}
	return out
}

// Missing reports the manifest entries that have drifted: the source the
// quotation was recorded against no longer ships, or no longer carries the
// words the chapter puts in quotes.
//
// Both halves are checked because either can rot alone. A message can be
// deleted outright, or reworded around a fragment the chapter quotes.
func Missing(manifest []Claim, corpus Corpus) []Claim {
	var out []Claim
	for _, c := range manifest {
		if !stillHolds(c.Src, c.Text, corpus) {
			out = append(out, c)
		}
	}
	return out
}

func stillHolds(src, text string, corpus Corpus) bool {
	body, ok := corpus[src]
	return ok && strings.Contains(body, text)
}

// Diff returns the entries of a that are absent from b, so a disagreement can
// be reported entry by entry rather than as two totals. The sync tool and the
// freshness test both report through it. [LAW:single-enforcer]
func Diff(a, b []Claim) []Claim {
	in := make(map[Claim]bool, len(b))
	for _, c := range b {
		in[c] = true
	}
	var out []Claim
	for _, c := range a {
		if !in[c] {
			out = append(out, c)
		}
	}
	return out
}

// Vanished splits the committed entries a fresh derivation no longer yields
// into the two cases that call for opposite responses, so a report can name the
// right one instead of guessing.
//
// An entry can leave a derivation two ways. The chapter stopped quoting the
// message — an ordinary prose edit, and regenerating is exactly right. Or the
// message stopped shipping, and regenerating is the one action that defeats the
// gate: it drops the entry, both tests go green, and the sentence stays in the
// specification describing a message the binary no longer has. Telling a
// contributor to regenerate in that second case is worse than saying nothing,
// because it is an instruction to erase the evidence.
//
// The discriminator is the same question Missing asks — does the recorded
// source still carry the words — so the two reports cannot disagree about what
// drift is. [LAW:single-enforcer]
func Vanished(manifest, fresh []Claim, corpus Corpus) (stopped, rephrased []Claim) {
	for _, c := range Diff(manifest, fresh) {
		if stillHolds(c.Src, c.Text, corpus) {
			rephrased = append(rephrased, c)
			continue
		}
		stopped = append(stopped, c)
	}
	return stopped, rephrased
}

// Dedupe sorts by document then text and drops repeats, so a derivation is a
// function of the tree alone and a re-run with no source change produces no
// diff.
//
// It lives here rather than in the sync tool because the tool and the freshness
// test must derive the manifest identically; when only the tool deduped, the
// two disagreed by 216 entries and the test demanded a regeneration the tool
// had already performed. [LAW:single-enforcer]
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

// Derive produces the manifest this tree yields, stabilised against prior.
// One function so the sync tool and the freshness test cannot disagree about
// what "current" means. [LAW:single-enforcer]
func Derive(fsys fs.FS, prior []Claim) ([]Claim, error) {
	corpus, err := ShippedText(fsys)
	if err != nil {
		return nil, err
	}
	names, err := SpecFiles(fsys)
	if err != nil {
		return nil, err
	}
	claims, err := DocClaims(fsys, names)
	if err != nil {
		return nil, err
	}
	return Dedupe(Stable(Matched(claims, corpus), prior, corpus)), nil
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
