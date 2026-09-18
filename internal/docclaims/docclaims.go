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
	"go/build/constraint"
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

	"golang.org/x/mod/modfile"
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
	// quoted words appear SOMEWHERE in the tree is far too weak: hundreds of the
	// entries have text contained in two or more distinct corpus sources, and
	// one sits in 29 of them (measured 2026-09-18). Delete the exact message
	// a chapter cites and a coincidental substring elsewhere keeps the gate
	// green — the precise failure this package exists to end.
	// [LAW:types-are-the-program] the claim carries its own evidence.
	//
	// The two kinds do not anchor equally tightly, and the weaker one is worth
	// knowing about rather than discovering. A literal is the message itself,
	// so the check is nearly exact. An asset is keyed by path and matched
	// against its whole contents, so a quotation is only held to "still
	// somewhere in this file" — delete the sentence a chapter describes and a
	// stray recurrence elsewhere in the same file keeps it green. The gap is
	// narrow because the files are small: the largest an entry anchors to is
	// 6.3 KB, of 12 such files (measured 2026-09-18). Closing it properly means
	// anchoring to a span rather than to a whole file (links-doc-v1-tepa).
	//
	// It also runs the other way, which is easier to meet and worth expecting.
	// `SHOW CREATE TABLE` is recorded for three chapters against
	// internal/store/migrations/00001_baseline.sql, where the phrase occurs
	// only in a SQL comment and in no shipped Go literal at all — so those
	// entries record protection that does not exist, and reflowing that comment
	// fails the gate naming three chapters the edit has nothing to do with.
	Src string
}

// Corpus is the shipped text a quotation can be checked against, keyed by the
// handle a Claim records in Src: a Go literal keys itself, an embedded asset
// keys its path. One map rather than two, so every caller asks the same
// question of both kinds. [LAW:no-mode-explosion]
type Corpus map[string]string

// add records one source under the handle a claim will anchor to, refusing a
// handle that already stands for different text.
//
// The corpus holds two kinds of source in one key space: a Go literal, whose
// handle is the text itself, and an embedded asset, whose handle is its path.
// Nothing makes those spaces disjoint — a literal is collected only when it
// contains a space, and embedPatterns accepts quoted patterns that contain one
// — so an asset path can equal a literal. Overwriting silently would leave Src
// naming a handle that no longer identifies one body, and `stillHolds` would
// then test a claim against the wrong text while `tightest` offered the wrong
// evidence. No collision exists today; this is what makes that a checked fact
// rather than an assumed one. [LAW:no-silent-failure]
func (c Corpus) add(handle, body string) error {
	if was, ok := c[handle]; ok && was != body {
		return fmt.Errorf("two shipped sources share the handle %q, so a claim anchored to it names no single body: rename the embedded asset whose path collides with the literal", ellipsis(handle, 80))
	}
	c[handle] = body
	return nil
}

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
			// The root module's own prefix maps to the tree root, which io/fs
			// spells "." and never "".
			if src.dir == "" {
				return ".", true
			}
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
	// go.mod is parsed by the parser the go command uses, not by reading its
	// syntax a second time here. The hand-rolled version treated any line
	// holding an arrow as a directive, which reads a commented-out `replace`
	// as a live one. [LAW:one-source-of-truth]
	mod, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parsing go.mod: %w", err)
	}
	// Guarded on the condition the message names. `len(out) == 0` is a
	// different question: a go.mod with no module line but any local replace
	// leaves out non-empty, so the guard stays silent while every import of
	// this repository's own packages fails to resolve — and the walk then
	// reports the whole specification false rather than itself broken, which
	// is exactly what the sibling guard in entryPackages exists to prevent.
	// [LAW:no-silent-failure]
	if mod.Module == nil {
		return nil, fmt.Errorf("go.mod declares no module path, so no import can be identified as source this repository ships")
	}
	out := sources{{prefix: mod.Module.Mod.Path}}
	for _, r := range mod.Replace {
		// A replacement carrying a version is another module, fetched into the
		// module cache and not in this tree; only a bare filesystem path names
		// something here. A path that climbs out of the tree (../sibling) is a
		// checkout this repository does not carry, and following it would fail
		// the gate on a directory that is not part of it.
		if r.New.Version != "" {
			continue
		}
		dir := path.Clean(r.New.Path)
		if path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") {
			continue
		}
		out = append(out, source{prefix: r.Old.Path, dir: dir})
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
		file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", name, err)
		}
		// The same rule the other two readers apply, because a `main` no build
		// ever compiles is not a binary. Counting one as an entry point also
		// disarms the guard below: the walk would start from a package whose
		// files are all skipped, yield an empty corpus, and report every
		// documented quotation as drifted prose — with err == nil.
		// [LAW:single-enforcer]
		if excludedFromEveryBuild(file) {
			return nil
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
		file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", name, err)
		}
		if excludedFromEveryBuild(file) {
			return nil
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

// excludedFromEveryBuild reports whether a file's build constraints are
// satisfied by no build at all — in practice the `ignore` tag, the conventional
// marker for a generator run by hand with `go run`. Following such a file's
// imports would pull packages nothing links into the corpus, and collecting its
// literals would count text no user can reach.
//
// Only unsatisfiability excludes a file here, and the limit is deliberate
// rather than overlooked. GOOS-suffixed files and tagged variants are all
// collected
// together — internal/dbsnapshot/clone_linux.go, clone_darwin.go and
// clone_other.go at once — because "the text that reaches a user" is the union
// over the platforms lit ships on, not whichever one this test happens to run
// on. The residual risk is narrow and worth stating: an identical message in
// two platform variants means deleting one leaves the entry anchored to the
// other. No manifest entry is anchored into those files today.
func excludedFromEveryBuild(file *ast.File) bool {
	// Both spellings, because the go tool honours both: a //go:build line
	// decides alone when one is present, and a file carrying only the legacy
	// // +build form is still excluded by it. Reading only the modern spelling
	// walks a hand-run generator's imports and collects its literals — text no
	// binary contains, which then competes to anchor a chapter's quotation.
	// AND-ed as one expression rather than tested a line at a time. Asking
	// whether any single line is unsatisfiable misses the file that no build
	// compiles because its lines contradict each other — `// +build linux`
	// over `// +build !linux` — where each line alone is perfectly
	// satisfiable. Such a file would stay in the corpus and its literals would
	// compete to anchor a chapter's quotation, which is the leak class this
	// package exists to close.
	var legacy constraint.Expr
scan:
	for _, group := range file.Comments {
		for _, c := range group.List {
			if c.Pos() > file.Package {
				break scan
			}
			// constraint.Parse decides what a constraint line is, and
			// neverBuilt decides whether anything satisfies it.
			// [LAW:one-source-of-truth] the go tool's own parser rather than a
			// second reading of its syntax.
			switch {
			case constraint.IsGoBuild(c.Text):
				return neverBuilt(c.Text)
			case constraint.IsPlusBuild(c.Text):
				expr, err := constraint.Parse(c.Text)
				if err != nil {
					continue
				}
				if legacy == nil {
					legacy = expr
				} else {
					legacy = &constraint.AndExpr{X: legacy, Y: expr}
				}
			}
		}
	}
	return legacy != nil && unsatisfiable(legacy)
}

// maxFreeTags bounds the assignment search in neverBuilt. A build constraint
// names a handful of tags; this is room to spare, and crossing it means the
// line is not a constraint anyone wrote by hand.
const maxFreeTags = 16

// neverBuilt reports whether a constraint line excludes its file from every
// build there is — that no assignment of build tags satisfies it.
//
// Satisfiability is the question, so the answer is a search over assignments
// rather than a reading of one. Evaluating the expression a single time with
// every tag but `ignore` set true answers a different question, and gets
// negation exactly backwards: `//go:build !windows` comes out false under that
// one assignment and its file reads as excluded. That file is
// internal/cli/detach_posix.go — the one compiled into every macOS and Linux
// lit — dropping out of the corpus, while detach_windows.go, which no lit a
// user runs here contains, stays in. Sampling a second assignment repairs those
// two spellings and still mis-reads a formula satisfiable only at a point
// neither sample visits, such as `(linux && !windows) || (windows && !linux)`.
// Enumerating is not a third sample; it is the predicate itself.
//
// `ignore` is pinned false because it is the tag no ordinary build sets — the
// conventional marker for a generator run by hand with `go run`. Every other
// tag is free, which is what makes `//go:build linux` satisfiable and keeps
// clone_linux.go and clone_darwin.go both in: the text reaching a user is the
// union over the platforms lit ships on, not whichever one this test runs on.
// [LAW:one-source-of-truth] the constraint language's own evaluator decides
// what a constraint means, and its tags are the whole of what a build can vary.
func neverBuilt(line string) bool {
	expr, err := constraint.Parse(line)
	if err != nil {
		return false
	}
	return unsatisfiable(expr)
}

// unsatisfiable reports whether no assignment of build tags satisfies expr,
// with `ignore` pinned false. Separate from neverBuilt because the legacy
// `// +build` path holds an expression it AND-ed together rather than a line.
func unsatisfiable(expr constraint.Expr) bool {
	free, walked := freeTags(expr)
	// An expression shape the walk does not know, or more tags than any
	// hand-written constraint carries, leaves the assignments unenumerable.
	// Keeping the file is the one direction that cannot blind the gate: its
	// text stays in the corpus and a chapter can still anchor to it.
	// [LAW:no-silent-failure] the bias is stated here, not discovered later as
	// a chapter that lost its evidence.
	if !walked || len(free) > maxFreeTags {
		return false
	}
	for mask := 0; mask < 1<<len(free); mask++ {
		satisfied := expr.Eval(func(tag string) bool {
			for i, name := range free {
				if name == tag {
					return mask&(1<<i) != 0
				}
			}
			// Only `ignore` reaches here: freeTags saw every tag in the
			// expression, or reported that it could not.
			return false
		})
		if satisfied {
			return false
		}
	}
	return true
}

// freeTags lists the tags a build could set to satisfy expr: every tag the
// expression names except `ignore`, which no build sets.
//
// The second result is false when the walk met an expression shape it does not
// know. That matters more than it looks: a tag missed here is an assignment
// never tried, and an assignment never tried reads as unsatisfiable, which
// would drop a file the build does compile. The caller keeps such a file rather
// than trusting a partial walk.
func freeTags(expr constraint.Expr) ([]string, bool) {
	var free []string
	seen := map[string]bool{"ignore": true}
	var walk func(constraint.Expr) bool
	walk = func(e constraint.Expr) bool {
		switch t := e.(type) {
		case *constraint.TagExpr:
			if !seen[t.Tag] {
				seen[t.Tag] = true
				free = append(free, t.Tag)
			}
			return true
		case *constraint.NotExpr:
			return walk(t.X)
		case *constraint.AndExpr:
			return walk(t.X) && walk(t.Y)
		case *constraint.OrExpr:
			return walk(t.X) && walk(t.Y)
		}
		return false
	}
	return free, walk(expr)
}

// isProductGo reports whether a file is Go source the product build compiles.
// A test file ships nothing, and the go tool ignores testdata entirely, so a
// package stored there is not reachable however it is imported.
//
// It also leaves out what the go tool leaves out by name: a file whose basename
// begins with "_" or "." is invisible to the build — `go list` does not even
// report it among a package's ignored files. Collecting one would put text no
// binary contains into the corpus, and because a quotation anchors to the
// shortest source holding it, that text would become the evidence for a chapter
// and survive deleting the real message. [LAW:one-source-of-truth] the go tool
// decides what compiles; this mirrors its rule rather than inventing a second.
func isProductGo(name string) bool {
	if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
		return false
	}
	if base := path.Base(name); strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
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
	if excludedFromEveryBuild(file) {
		return nil
	}
	var collision error
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
			if err := into.add(text, text); err != nil {
				collision = err
				return false
			}
		}
		return true
	})
	if collision != nil {
		return collision
	}
	return collectEmbeds(fsys, path.Dir(name), file, into)
}

// collectEmbeds reads the assets a file embeds, keyed by their paths.
//
// The patterns are taken from the //go:embed directives themselves rather than
// guessed from file extensions: what ships is what the compiler is told to
// embed, and a .md beside it that nothing embeds does not.
func collectEmbeds(fsys fs.FS, dir string, file *ast.File, into Corpus) error {
	for _, group := range embedDocs(file) {
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

// closingQuote returns the index of the double quote that ends the interpreted
// string literal starting at s[0], or -1 when none does.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
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
		case '"':
			// Scanned with escapes honoured, because \" does not end the
			// literal. Cutting at the first inner quote instead splits a legal
			// operand into fragments that match no file, and an unmatched
			// pattern is a hard error — so the gate would fail the build over
			// source the compiler accepts.
			end := closingQuote(rest)
			if end < 0 {
				token, rest = rest[1:], ""
				break
			}
			token, rest = rest[1:end], rest[end+1:]
			if unquoted, err := strconv.Unquote(`"` + token + `"`); err == nil {
				token = unquoted
			}
		case '`':
			// A raw literal has no escapes: the next backquote ends it.
			end := strings.IndexByte(rest[1:], '`')
			if end < 0 {
				token, rest = rest[1:], ""
				break
			}
			token, rest = rest[1:1+end], rest[2+end:]
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

// embedDocs returns the comment groups that can legally carry a //go:embed
// directive: the doc comment of a var declaration, and of each spec inside a
// var block. Scanning every comment in the file instead would treat prose that
// quotes a directive as one, and pull in assets the compiler never embeds —
// which, now that an unmatched pattern is an error, would fail the gate over a
// sentence in a comment. [LAW:parse-dont-validate]
func embedDocs(file *ast.File) []*ast.CommentGroup {
	var out []*ast.CommentGroup
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		if gen.Doc != nil {
			out = append(out, gen.Doc)
		}
		for _, spec := range gen.Specs {
			if v, ok := spec.(*ast.ValueSpec); ok && v.Doc != nil {
				out = append(out, v.Doc)
			}
		}
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
	if len(matches) == 0 {
		// The compiler refuses a directive that matches no files, so this is
		// unreachable for code that builds — which is exactly why it must be
		// loud here rather than free. Silence would drop the asset from the
		// corpus and report every chapter quoting it as a message that stopped
		// shipping: a parse gap wearing the costume of the drift this package
		// hunts. [LAW:no-silent-failure]
		return fmt.Errorf("embed pattern %q matched no files", pattern)
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
	return into.add(name, string(data))
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
	open, opened := "", 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if open == "" {
			if marker := fenceMarker(trimmed); marker != "" {
				open, opened = marker, i+1
				out = append(out, "")
				continue
			}
			out = append(out, line)
			continue
		}
		if closesFence(open, trimmed) {
			open = ""
		}
		out = append(out, "")
	}
	if open != "" {
		return "", fmt.Errorf("unclosed code fence opened at line %d", opened)
	}
	return strings.Join(out, "\n"), nil
}

// fenceMarker returns the run of backticks or tildes that opens or closes a
// fence, or "" for an ordinary line.
func fenceMarker(trimmed string) string {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == c {
			n++
		}
		if n < 3 {
			continue
		}
		// CommonMark: a backtick fence's info string may not contain a
		// backtick. That rule is what separates a fence from a prose line
		// opening with an inline code span — ```lit next``` prints … —
		// and without it such a line opens a fence that nothing closes,
		// so stripFences either blanks real prose to the next lone fence
		// line, dropping its claims as an ordinary "entries left the
		// manifest" diff, or raises an unclosed-fence error naming a line
		// that is not one. Both are the silent failure closesFence exists
		// to prevent.
		if c == '`' && strings.ContainsRune(trimmed[n:], '`') {
			return ""
		}
		return trimmed[:n]
	}
	return ""
}

// closesFence reports whether a line ends the fence that open began.
//
// CommonMark's rule, not "any marker ends any fence": the closer must use the
// same character, be at least as long, and carry no trailing text. A single
// boolean toggled by either marker inverts on a nested fence — a ~~~ inside a
// ``` block reopens what it should have left alone — and from there the parity
// is wrong for the rest of the file. The loud outcome is a spurious unclosed
// fence error naming the wrong line; the quiet one is real prose below the
// nesting silently treated as code, dropping its claims from the manifest as an
// ordinary "entries left" diff, which is what CONTRIBUTING tells a reviewer
// means a sentence stopped describing the binary. [LAW:no-silent-failure]
func closesFence(open, trimmed string) bool {
	marker := fenceMarker(trimmed)
	if marker == "" || marker != trimmed {
		return false
	}
	return marker[0] == open[0] && len(marker) >= len(open)
}

// Matched keeps the claims whose text appears in shipped text, recording which
// source that was. This is what makes the manifest self-maintaining: a chapter
// that starts quoting a real message joins the protected set on the next sync,
// and one that never quoted code is simply absent rather than allowlisted.
func Matched(claims []Claim, corpus Corpus) []Claim {
	// tightest is a function of the text and the corpus alone, and the chapters
	// repeat themselves: a sixth of the spans quote a phrase another span
	// already quoted (1,197 of 7,073, measured 2026-09-18), and each repeat
	// re-scanned every shipped literal and asset to reach the same answer. The
	// table lives and dies inside this call, so it cannot outlast the corpus it
	// was computed against and there is no staleness to reason about.
	// [LAW:dataflow-not-control-flow] one question, asked once.
	type match struct {
		src string
		ok  bool
	}
	answered := make(map[string]match, len(claims))
	var out []Claim
	for _, c := range claims {
		m, asked := answered[c.Text]
		if !asked {
			m.src, m.ok = tightest(c.Text, corpus)
			answered[c.Text] = m
		}
		if !m.ok {
			continue
		}
		c.Src = m.src
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
	was := anchors(prior)
	out := make([]Claim, 0, len(fresh))
	for _, c := range fresh {
		if src, ok := was[[2]string{c.Doc, c.Text}]; ok && stillHolds(src, c.Text, corpus) {
			c.Src = src
		}
		out = append(out, c)
	}
	return out
}

// classify decides what a contributor should do about one departed entry.
//
// Two independent facts decide it, and either one alone gives the wrong
// instruction in a case that happens routinely:
//
//   - do the chapters still quote these words? — asked of the spans the
//     documents yield, before any of them are matched against shipped text, so
//     the answer survives the message being deleted. Asked of this chapter and
//     of all of them, which are different questions;
//   - does any shipped source still carry them? — asked of the corpus, because
//     whether a message ships is a question about the product.
//
// Reading only the corpus is what made the prescribed workflow fire the one
// warning that must never be wrong. Deleting a message and the sentence that
// quoted it — together, which is what CONTRIBUTING asks for — leaves no trace
// of either in the corpus, and the report told the contributor "Do NOT
// regenerate: that drops the entry and leaves the sentence false" about a
// sentence they had just removed. Regenerating was the only way to green and it
// was correct. A warning that is wrong on the ordinary path is a warning people
// learn to step over, and this is the one they must not.
//
// The source that carries the words now is whichever a derivation would pick —
// `tightest`, the same choice Matched makes — so a report and a derivation
// cannot name different sources. [LAW:one-source-of-truth]
//
// It returns the finding rather than a kind and a loose string for the caller
// to reassemble. Which fact the report must name follows from the kind — the
// source carrying the words for AnchorMoved, the chapters still asserting them
// for Stopped — and assembling that pairing at the callsite is the shape that
// has produced a wrong report here every previous time.
// [LAW:types-are-the-program]
func classify(c Claim, quoted quotations, corpus Corpus) Drift {
	now, ships := tightest(c.Text, corpus)
	// The destructive case is tested first and against every chapter, because
	// "this chapter stopped quoting it" is not the same fact as "nothing quotes
	// it any more", and only the second makes dropping the entry safe. A
	// sentence moved between chapters — a renumbering, a section lifted into
	// another file — in the same change that deletes the message it quotes
	// leaves the recorded (doc, text) key absent, and asking only about this
	// chapter answered QuoteDropped: regenerate, entry gone, the chapter it
	// moved to still asserting a message the binary no longer has, every check
	// green. [LAW:no-silent-failure]
	if quoting := quoted.anywhere(c.Text); len(quoting) > 0 && !ships {
		return Drift{Claim: c, Kind: Stopped, QuotedBy: quoting}
	}
	// Nobody asserts it here any more, and dropping it erases no live claim —
	// either the words still ship or no chapter quotes them. This is also the
	// prescribed workflow's landing place: delete a message and every sentence
	// quoting it together and no document quotes the text, so the benign
	// remedy is what a contributor gets.
	if !quoted.inDoc(c.Doc, c.Text) {
		return Drift{Claim: c, Kind: QuoteDropped}
	}
	// What remains: this chapter still quotes the words and something still
	// ships them, or the branch above would have caught it.
	return Drift{Claim: c, Kind: AnchorMoved, Now: now}
}

// quotations is what the chapters assert, read before any of it is matched
// against shipped text so the answers survive a message being deleted.
//
// It is indexed both ways because classify needs both, and conflating them
// breaks in opposite directions: asking only "does this chapter quote it"
// misses a sentence that moved to another chapter, and asking only "does any
// chapter quote it" reports a re-anchor against a source that never moved when
// one chapter of several drops a quotation the others keep.
type quotations struct {
	byDoc  map[[2]string]bool
	byText map[string][]string
}

func newQuotations(claims []Claim) quotations {
	q := quotations{
		byDoc:  make(map[[2]string]bool, len(claims)),
		byText: make(map[string][]string, len(claims)),
	}
	for _, c := range claims {
		key := [2]string{c.Doc, c.Text}
		if q.byDoc[key] {
			continue
		}
		q.byDoc[key] = true
		q.byText[c.Text] = append(q.byText[c.Text], c.Doc)
	}
	for _, docs := range q.byText {
		sort.Strings(docs)
	}
	return q
}

// inDoc reports whether this document still quotes these words.
func (q quotations) inDoc(doc, text string) bool { return q.byDoc[[2]string{doc, text}] }

// anywhere names every document that still quotes these words, sorted, so a
// report reads the same whatever order the walk happened to find them in.
// The report needs the names and not a count: when a sentence moves between
// chapters, the entry's own chapter is precisely the one that stopped quoting
// it, so naming that chapter would state the opposite of what happened.
func (q quotations) anywhere(text string) []string { return q.byText[text] }

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

// anchors indexes claims by the pair that identifies a quotation — the chapter
// and the words it quotes — mapping it to the source it is recorded against.
// The stabiliser and the drift report both need to ask "where is this
// quotation anchored now", and asking it the same way is what keeps them from
// disagreeing about what drift is. [LAW:one-source-of-truth]
func anchors(claims []Claim) map[[2]string]string {
	out := make(map[[2]string]string, len(claims))
	for _, c := range claims {
		out[[2]string{c.Doc, c.Text}] = c.Src
	}
	return out
}

// DriftKind names why a committed entry is absent from a fresh derivation.
//
// There are three reasons, not two, and they call for different actions — one
// of which is destructive. Collapsing them onto a single question ("does the
// recorded source still carry the words?") answers a contributor's question
// with the wrong instruction whenever a literal is reworded around a quotation,
// which is the ordinary edit. [LAW:types-are-the-program] the report carries
// its own discriminator instead of leaving a reader to infer one.
type DriftKind int

const (
	// QuoteDropped: the recorded source still ships and still carries the
	// words — the chapter simply stopped quoting them. An ordinary prose edit;
	// regenerating is exactly right.
	QuoteDropped DriftKind = iota

	// AnchorMoved: the recorded source no longer carries the words, but some
	// other shipped source does. Two different things look identical from here
	// and nothing in this package can tell them apart: the literal was reworded
	// around the quotation, or the documented message was deleted and an
	// unrelated string happens to contain the same words. The second is not
	// theoretical — hundreds of entries have text sitting in two or more
	// distinct sources (measured 2026-09-18) — so the report names the source
	// that carries the words now and leaves the judgment to a reader.
	// [LAW:no-silent-failure] neither answer is guessed.
	AnchorMoved

	// Stopped: nothing shipped carries the words any more. Regenerating drops
	// the entry, turns both checks green, and leaves the specification
	// describing a message the binary no longer has.
	Stopped
)

// Drift is one committed entry that a fresh derivation no longer yields,
// carrying why it went and the evidence a reader needs to act on it.
type Drift struct {
	Claim

	Kind DriftKind

	// Now is the source carrying Text today. It is set when Kind is
	// AnchorMoved, and empty otherwise: the other two kinds have no such
	// source, and inventing one would be the guess this type exists to avoid.
	Now string

	// QuotedBy names the chapters still asserting Text, set when Kind is
	// Stopped. Claim.Doc is the wrong name to print there: a sentence that
	// moved between chapters leaves the entry recorded against the chapter
	// that no longer quotes it, and the live false assertion — the thing a
	// contributor has to go and fix — is in the chapter it moved to.
	//
	// A list rather than a rendered sentence, because it is a list; joining it
	// is Explain's business, and a caller that wants the chapters themselves
	// should not have to split them back out.
	QuotedBy []string
}

// Explain is the line a report prints for this drift: what changed, and what to
// do about it.
//
// It lives here rather than in each caller because the two reports describe the
// same failure — the freshness test and the sync tool — and a contributor who
// sees them contradict each other learns to disregard whichever one is louder.
// [LAW:one-source-of-truth] the remedy is a fact about the failure, not about
// who is printing it.
// NowBrief is the source carrying the quotation today, shortened the way a
// report must show it. A Go-literal handle is the whole literal the words were
// found inside, which runs to kilobytes here, and printing one unedited buries
// the sentence the report is about.
//
// [LAW:single-enforcer] every report of a re-anchor goes through this, so the
// test's Explain and the writer's own line cannot disagree about how much of a
// handle a reader is shown.
func (d Drift) NowBrief() string { return ellipsis(d.Now, 120) }

func (d Drift) Explain() string {
	switch d.Kind {
	case QuoteDropped:
		return fmt.Sprintf("no longer quoted by %s: %q — the prose changed; run `go run ./tools/docclaims-sync`", d.Doc, d.Text)
	case AnchorMoved:
		return fmt.Sprintf("%s quotes %q, and the source it was recorded against no longer carries it; the words ship today in %q — if that is the same message reworded, run `go run ./tools/docclaims-sync`; if it is an unrelated string, the documented message is gone: fix the code or the chapter", d.Doc, d.Text, d.NowBrief())
	default:
		return fmt.Sprintf("%s quotes a message that no longer ships: %q — fix the code or the chapter. Do NOT regenerate: that drops the entry and leaves the sentence false", strings.Join(d.QuotedBy, ", "), d.Text)
	}
}

// ellipsis shortens a source handle for a report line. A Go literal keys itself
// in the corpus, so an untruncated one can bury the sentence the error is about.
func ellipsis(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Comparison is the whole difference between a committed manifest and a fresh
// derivation, arranged so that one quotation produces one line.
type Comparison struct {
	// Drifted holds the committed entries the derivation no longer yields,
	// each carrying why.
	Drifted []Drift

	// Added holds the quotations the derivation yields that the manifest does
	// not record — excluding the ones already reported as a moved anchor,
	// which are the same quotation seen from its other side. Reported twice
	// they read as two unrelated facts, one of them ("only in a fresh
	// derivation") inviting the regeneration the other is warning against.
	Added []Claim
}

// Clean reports whether the manifest and the derivation agree entirely.
func (c Comparison) Clean() bool { return len(c.Drifted) == 0 && len(c.Added) == 0 }

// Stopped returns the drifts that regenerating would erase: messages the
// specification still quotes that nothing in the product carries any more.
//
// Named once, because it is the one case that must never be written past, and
// three places need to recognise it — the freshness test, the -check report,
// and the write path that would do the erasing. [LAW:single-enforcer]
func (c Comparison) Stopped() []Drift { return c.ofKind(Stopped) }

// Reanchored returns the drifts a regeneration would resolve by moving an
// entry's recorded source: the quotation still ships, inside different words.
//
// It is named for the write path, which would otherwise perform exactly the
// judgement its own report describes as one "only a reader can settle" — and
// perform it in silence. Rewording a literal around a quoted fragment is the
// ordinary edit and must not be refused, but the contributor has to be told
// that the regeneration decided the reworded literal is the same message, so
// that the manifest diff is read rather than waved through. Regenerating over a
// genuine replacement is how a chapter keeps a sentence about a message that
// left, with every check green.
func (c Comparison) Reanchored() []Drift { return c.ofKind(AnchorMoved) }

func (c Comparison) ofKind(k DriftKind) []Drift {
	var out []Drift
	for _, d := range c.Drifted {
		if d.Kind == k {
			out = append(out, d)
		}
	}
	return out
}

// Compare is the whole manifest-versus-tree comparison every report prints.
//
// It is a method on Derivation rather than a function over loose slices because
// each of its inputs was, at some point, assembled at a callsite out of what
// that caller happened to have — and each time the report came out wrong in a
// different way: one quotation printed as two findings with opposite remedies,
// then the prescribed workflow firing the destructive-remedy warning. A
// comparison needs the derivation, the quotations the chapters make, and the
// shipped text; travelling together, a caller cannot hold two thirds of one.
// [LAW:types-are-the-program]
func (d Derivation) Compare(manifest []Claim) Comparison {
	quoted := newQuotations(d.quoted)
	var out Comparison
	moved := make(map[[2]string]bool)
	for _, c := range Diff(manifest, d.fresh) {
		drift := classify(c, quoted, d.corpus)
		if drift.Kind == AnchorMoved {
			moved[[2]string{c.Doc, c.Text}] = true
		}
		out.Drifted = append(out.Drifted, drift)
	}
	for _, c := range Diff(d.fresh, manifest) {
		if moved[[2]string{c.Doc, c.Text}] {
			continue
		}
		out.Added = append(out.Added, c)
	}
	return out
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
func Derive(fsys fs.FS, prior []Claim) (Derivation, error) {
	corpus, err := ShippedText(fsys)
	if err != nil {
		return Derivation{}, err
	}
	names, err := SpecFiles(fsys)
	if err != nil {
		return Derivation{}, err
	}
	claims, err := DocClaims(fsys, names)
	if err != nil {
		return Derivation{}, err
	}
	return Derivation{
		fresh:  Dedupe(Stable(Matched(claims, corpus), prior, corpus)),
		quoted: claims,
		corpus: corpus,
	}, nil
}

// Derivation is what one pass over the tree yields: the manifest it would
// write, every span the chapters quote before any of them is matched against
// shipped text, and the text the product ships.
//
// The three travel together because a comparison needs all three, and each is
// derived from the same pass — a caller that fetched one of them separately
// could compare a manifest against one tree using the corpus of another.
//
// The fields are unexported so that Derive is the only way to obtain one
// outside this package, which is what makes the sentence above enforced rather
// than merely asserted. An unexported marker field would not have done it: Go
// permits a keyed composite literal from another package to omit unexported
// fields, so `docclaims.Derivation{Fresh: x, Corpus: y}` stayed legal and
// classify then read the missing quotations as "no chapter quotes this" — every
// departed entry reported as QuoteDropped, the benign remedy, and the
// destructive-case warning unable to fire at all. A gate that fails open on its
// one dangerous case is the failure this type was introduced to end.
// The zero value stays constructible, as it does for every Go struct, and it
// needs no guard: with no fresh manifest to diff against, Compare reports the
// whole committed manifest as drifted rather than a handful of entries with the
// wrong remedy. That fails closed and at full volume. It was the partial literal
// that was dangerous, because a real Fresh and a real Corpus make the report
// look ordinary while the missing quotations quietly pick the mild remedy.
// [LAW:parse-dont-validate] the only Derivation that exists is one Derive built.
type Derivation struct {
	fresh  []Claim
	quoted []Claim
	corpus Corpus
}

// Fresh is the manifest this tree yields — what a regeneration would write.
func (d Derivation) Fresh() []Claim { return d.fresh }

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
