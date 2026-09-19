// Package doccites resolves the v1 specification's file:line citations against
// the code they point at.
//
// doc-v1-total cites source by line number, and a line number is a map of a
// location rather than the location itself. Any edit above a cited line
// silently redraws the territory and leaves the map untouched, so a reader who
// follows a stale citation lands in unrelated code and reads it as the thing
// the sentence described. That is worse than no citation, because it is
// followed with confidence. [LAW:one-source-of-truth] the declaration is the
// fact; the citation is a map of it, and this package is what re-checks the map.
//
// # The shapes, and why they do not survive parsing
//
// A citation is written three ways, and only the first carries a filename:
//
//	named       `internal/model/priority.go:18-21`
//	comma tail  `error_output.go:113,117`      one file, several line spans
//	bare cont   `:47-54`                       inherits the file named earlier
//
// Bare continuations are 26% of the corpus and cluster in the dense prose,
// where the most checkable citations live. Every instrument built here so far
// was written against the named shape and reported success over the rest:
// three separate sweeps on one afternoon, then a fourth by an author who had
// the blind spot written down and cited it while reproducing it. All four asked
// "what follows `file.go:`" instead of "what is a citation here".
//
// So the shape is resolved away at the boundary and nothing downstream can ask
// about it. [LAW:parse-dont-validate] a parsed Citation always carries the file
// its sentence meant, because a bare continuation *is* a named citation whose
// filename was recovered from context. There is no per-shape branch to forget,
// and a checker blind to a quarter of the corpus is not something a later
// author can write by accident.
//
// # What can and cannot be decided mechanically
//
// Whether a citation is *right* depends on the claim the sentence is making
// about it, which no rule over line numbers can see. A span of pure comments
// looks structurally impossible and is correct when the prose cites a comment
// for stating a deliberation. So this package does not judge correctness. It
// answers the decidable question: the prose names a symbol beside the citation,
// and the cited lines either cover that symbol's declaration — its doc comment
// included — or use it by name, or have nothing to do with it.
//
// Where no symbol binds, the verdict is Unbound, and that is a fact about the
// citation rather than a gap in the instrument: 46.1% of this corpus is
// Unbound, and such a citation asserts nothing a reader can check either. It is
// the measurement most directly behind CONTRIBUTING.md asking new citations to
// name their symbol.
//
// Scope: line numbers, not text. Whether a quoted message still ships is
// internal/docclaims. Whether a chapter names identifiers that exist at all is
// links-doc-v1-mujn.
package doccites

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Span is an inclusive range of source lines. A single-line citation has
// End == Start, so every consumer reads one shape and none of them has to know
// that `:87` and `:95-101` were written differently.
type Span struct{ Start, End int }

func (s Span) String() string {
	if s.Start == s.End {
		return strconv.Itoa(s.Start)
	}
	return fmt.Sprintf("%d-%d", s.Start, s.End)
}

// Citation is one resolved line reference: a span of one source file, the
// chapter sentence that points at it, and the identifiers that sentence offers
// as its subject.
//
// Named is the path the sentence meant, recovered from the document: an
// abbreviation inherits from the paths named before it, so every Citation
// carries a file whatever spelling it was written in. It is still the prose's
// own path and may be absolute or partial, because turning it into a file that
// exists needs the tree, which Check has and Parse does not.
type Citation struct {
	Doc     string
	DocLine int
	Text    string
	Named   string
	Span    Span

	// Nearby holds the backticked identifiers standing before this citation in
	// its own block, nearest first. The prose binds a citation to its subject by
	// adjacency — "`priorityEntry` resolves an int onto it (`:47-54`)" — and
	// which of them is a real declaration is a question only the tree answers.
	Nearby []string
}

// Verdict is the complete set of answers this package can give about one
// citation. Findings are produced only by Check, so the set is closed: a
// consumer that handles these five has handled every citation in the corpus.
type Verdict int

const (
	// Holds: the span contains the declaration of the symbol the prose names.
	Holds Verdict = iota
	// Moved: that symbol is declared in the file, outside the span. This is the
	// drift the ticket exists for — the citation still resolves to real lines,
	// and they are the wrong lines.
	Moved
	// Unresolved: the citation does not identify one file. Either the tree has
	// no such path, or the chapter abbreviated to a basename that several files
	// answer to and the specification never says which — three packages here
	// hold a sync.go, and "sync.go:20" names none of them.
	Unresolved
	// PastEOF: the span runs past the end of the file. Decidable without a
	// symbol, so it is the one check that reaches the unbound citations too.
	PastEOF
	// Unbound: no named symbol is declared in the cited file, so the citation
	// asserts nothing this package can check — and nothing a reader can either.
	Unbound
)

func (v Verdict) String() string {
	return [...]string{"holds", "moved", "unresolved", "past-eof", "unbound"}[v]
}

// Finding is a citation and what the tree says about it.
type Finding struct {
	Citation
	Verdict Verdict
	// Symbol is the identifier that bound, and Declared the line it is declared
	// on. Both are set exactly when the verdict is Holds or Moved, which are the
	// two verdicts reached by binding a symbol.
	Symbol   string
	Declared int
	// Lines is the cited file's length, set exactly when the verdict is
	// PastEOF. Its own field rather than a second meaning for Declared: one
	// int that is a declaration line under two verdicts and a file length
	// under a third cannot be read without reading Verdict first, and a
	// consumer that trusts the name prints a length where a line belongs.
	// [LAW:one-type-per-behavior]
	Lines int
	// File is the resolved repository-relative path, empty only for Unresolved.
	File string
}

func (f Finding) String() string {
	switch f.Verdict {
	case Moved:
		return fmt.Sprintf("%s:%d: %s cites %s:%s for `%s`, which is declared at :%d",
			f.Doc, f.DocLine, f.Text, f.File, f.Span, f.Symbol, f.Declared)
	case Unresolved:
		return fmt.Sprintf("%s:%d: %s names %q, which does not identify one file in this tree",
			f.Doc, f.DocLine, f.Text, f.Named)
	case PastEOF:
		return fmt.Sprintf("%s:%d: %s cites %s:%s, past the end of a %d-line file",
			f.Doc, f.DocLine, f.Text, f.File, f.Span, f.Lines)
	default:
		return fmt.Sprintf("%s:%d: %s (%s)", f.Doc, f.DocLine, f.Text, f.Verdict)
	}
}

// citeRe matches all three shapes at once: the file part is optional, and the
// line part admits a comma list. Writing them as one pattern is deliberate —
// two patterns is how one of them ends up maintained and the other forgotten.
var citeRe = regexp.MustCompile("`([A-Za-z0-9_./-]*\\.(?:go|sql|mod|sum|yml|yaml|json|md|txt|tmpl))?:(\\d+(?:-\\d+)?(?:\\s*,\\s*\\d+(?:-\\d+)?)*)`")

var spanRe = regexp.MustCompile(`(\d+)(?:-(\d+))?`)

// tickRe finds backticked spans; identRe decides which of them could name a Go
// declaration. `--take`, `0` and `no ready work` are all backticked and none is
// a symbol, so the citation they stand beside binds on something further back
// or not at all. A trailing argument list is dropped — the prose writes
// `ParsePriority(int)` for what the source declares as ParsePriority — and a
// qualified name binds on its last element, since `model.Attribution` is
// declared as Attribution.
var (
	tickRe  = regexp.MustCompile("`[^`\n]+`")
	identRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:\.([A-Za-z_][A-Za-z0-9_]*))*(?:\(.*\))?$`)
)

// Parse reads every citation out of one document, resolving each one's file
// against the paths the document itself names and collecting the identifiers
// its sentence offers as the citation's subject.
//
// A comma list yields one Citation per span rather than one carrying several,
// so a count of citations is a count of the line references a reader can
// follow, and no consumer has to remember to walk a tail.
func Parse(doc, text string) []Citation {
	var out []Citation
	lineOf := lineIndex(text)
	seen := mentions(text)

	for _, m := range citeRe.FindAllStringSubmatchIndex(text, -1) {
		at := m[0]
		cite := Citation{
			Doc:     doc,
			DocLine: lineOf(at),
			Text:    text[at:m[1]],
			Named:   resolveWritten(group(text, m, 1), at, seen),
			Nearby:  identsBefore(text[blockStart(text, at):at]),
		}
		for _, sp := range spanRe.FindAllStringSubmatch(text[m[4]:m[5]], -1) {
			first, _ := strconv.Atoi(sp[1])
			last := first
			if sp[2] != "" {
				last, _ = strconv.Atoi(sp[2])
			}
			cite.Span = Span{Start: first, End: last}
			out = append(out, cite)
		}
	}
	return out
}

// blockStart is where the citation's own block begins: its paragraph, or its
// own list item or table row when it sits inside one.
//
// The scope matters in both directions. Too narrow and the wrapping defeats it:
// these chapters routinely name a symbol on one line and carry the citation on
// the next, and a line-scoped reader called 279 such citations unverifiable
// where a block-scoped one found 5. Too wide and a citation binds to a symbol
// from a different bullet — an inventory's list of field changes bound
// `:233-239` to planFields three items further up, and produced a verdict about
// a function that item never mentioned.
//
// A markdown list item and a table row are blocks, so the innermost one wins.
//
// Derived from the citation's position rather than by splitting the document
// and tracking offsets as they accumulate. That arithmetic is only correct
// while every paragraph is separated by exactly one blank line, and a document
// with a longer run silently shifts every offset after it.
func blockStart(text string, at int) int {
	start := 0
	if i := strings.LastIndex(text[:at], "\n\n"); i >= 0 {
		start = i + 2
	}
	for off := start; off < at; {
		nl := strings.IndexByte(text[off:at], '\n')
		if itemRe.MatchString(text[off:at]) {
			start = off
		}
		if nl < 0 {
			break
		}
		off += nl + 1
	}
	return start
}

// itemRe recognises the start of a markdown list item or table row.
var itemRe = regexp.MustCompile(`^[ \t]*(?:[-*+]|\d+\.|\|)[ \t]`)

// pathRe matches a backticked path anywhere in the prose, whether or not a line
// number follows it. Headings carry most of them — "## PART 1 — Sync entry
// points (`internal/store/sync.go`)" — and a later `sync.go:25` under that
// heading means that file, exactly as a reader takes it.
var pathRe = regexp.MustCompile("`(/?[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*\\.(?:go|sql|mod|sum|yml|yaml|json|md|txt|tmpl))(?::\\d|`)")

// mention is a path the document names, and where it names it.
type mention struct {
	at   int
	path string
}

// mentions lists every path the document names, in the order it names them.
//
// This is the whole of the resolver's context, and it is deliberately the
// document rather than the filesystem. An earlier draft resolved a bare
// basename by looking for a unique file of that name in the tree, which made
// the answer depend on what happened to be lying beside the checkout: this
// working tree holds sibling worktrees under .claude and a gitignored vendored
// project with more Go files than lit has itself, and against them nearly every
// basename in the corpus collided. That instrument measures one number locally
// and another in CI. A document that names its own files cannot drift that way.
// [LAW:one-source-of-truth]
func mentions(text string) []mention {
	var out []mention
	qualified := map[string]string{}
	for _, m := range pathRe.FindAllStringSubmatchIndex(text, -1) {
		p := text[m[2]:m[3]]
		// Each mention is resolved as the document is read, so a later
		// abbreviation inherits the directory instead of erasing it. Without
		// this the second `model.go` in a chapter overwrites the
		// internal/model/model.go that introduced it, and every citation after
		// that point reports a file the tree does not have — 6,008 of them,
		// which reads like a catastrophic corpus rather than one greedy write.
		if strings.Contains(p, "/") {
			qualified[path.Base(p)] = p
		} else if q, ok := qualified[p]; ok {
			p = q
		}
		out = append(out, mention{at: m[0], path: p})
	}
	return out
}

// resolveWritten turns the file part of a citation at offset `at` into the path
// its sentence meant.
//
// Four spellings reach here and three are abbreviations. A path-qualified
// citation says everything. A bare continuation (`:47-54`) means the file the
// previous mention named. A bare basename (`sync.go:25`) means the most recent
// mention ending in that name — three packages in this tree hold a sync.go, so
// the basename alone names none of them.
//
// The basename shape is the one a census misses most quietly: it carries a
// filename, so a finder anchored on "what follows `file.go:`" does not skip it,
// it resolves it against the wrong file and reports a confident verdict about a
// file the sentence never mentioned.
func resolveWritten(written string, at int, seen []mention) string {
	if strings.Contains(written, "/") {
		return written
	}
	last := ""
	for _, m := range seen {
		// Strictly before: a citation is itself a path mention, and at its own
		// offset `sync.go:25` would answer "sync.go" — resolving the
		// abbreviation to the abbreviation, and losing the directory that makes
		// it nameable.
		if m.at >= at {
			break
		}
		if written == "" || path.Base(m.path) == written {
			last = m.path
		}
	}
	if last == "" {
		return written
	}
	return last
}

func group(text string, m []int, n int) string {
	if m[2*n] < 0 {
		return ""
	}
	return text[m[2*n]:m[2*n+1]]
}

// lineIndex returns a function from byte offset to 1-based line number, so a
// finding can name the sentence a reader has to go and fix.
func lineIndex(text string) func(int) int {
	var starts []int
	for i, r := range text {
		if r == '\n' {
			starts = append(starts, i+1)
		}
	}
	return func(off int) int {
		return sort.SearchInts(starts, off+1) + 1
	}
}

// identsBefore lists the backticked identifiers in a block prefix, nearest to
// the end first — the end being where the citation sits.
//
// The prose binds a citation to its subject by adjacency: "`priorityEntry`
// resolves an int onto it (`:47-54`)". Nearest-first is therefore the order of
// preference, and Check takes the first one the cited file actually declares,
// so a sentence full of prose backticks still binds on its real symbol.
func identsBefore(prefix string) []string {
	var out []string
	for _, loc := range tickRe.FindAllStringIndex(prefix, -1) {
		tok := strings.TrimSpace(prefix[loc[0]+1 : loc[1]-1])
		m := identRe.FindStringSubmatch(tok)
		if m == nil {
			continue
		}
		name := m[1]
		if m[2] != "" {
			name = m[2]
		}
		out = append(out, name)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Glossary maps a bare basename to the path the specification qualifies it with.
//
// Chapters abbreviate freely, and not always after introducing the full path
// themselves: chapter 04 cites `store.go:1742-1745` and never names
// internal/store/store.go anywhere in the document. A reader resolves that from
// the chapter's subject; the document does not carry it. The rest of the
// specification does, so the corpus as a whole is the dictionary.
//
// A basename the corpus qualifies two different ways is left out rather than
// guessed at. Picking one is how a citation gets a confident verdict against a
// file its sentence never mentioned.
type Glossary map[string]string

// BuildGlossary reads every document's path mentions and keeps the basenames
// that resolve one way across the whole specification.
func BuildGlossary(docs map[string]string) Glossary {
	seen := map[string]map[string]bool{}
	for _, text := range docs {
		for _, m := range mentions(text) {
			if !strings.Contains(m.path, "/") {
				continue
			}
			base := path.Base(m.path)
			if seen[base] == nil {
				seen[base] = map[string]bool{}
			}
			seen[base][m.path] = true
		}
	}
	g := Glossary{}
	for base, paths := range seen {
		if len(paths) != 1 {
			continue
		}
		for p := range paths {
			g[base] = p
		}
	}
	return g
}

// Apply qualifies the citations a document left bare.
func (g Glossary) Apply(cites []Citation) []Citation {
	out := make([]Citation, 0, len(cites))
	for _, c := range cites {
		if !strings.Contains(c.Named, "/") {
			if full, ok := g[c.Named]; ok {
				c.Named = full
			}
		}
		out = append(out, c)
	}
	return out
}

// Tree is the territory the citations are maps of: which files exist, how long
// they are, and where each Go declaration sits.
type Tree struct {
	lines  map[string]int
	decls  map[string]map[string]extent
	text   map[string][]string
	suffix map[string][]string
}

// extent is the line range a declaration occupies: a function including its
// body, a type including its fields, a single const or var line.
type extent struct{ start, end int }

func (e extent) overlaps(s Span) bool { return e.start <= s.End && s.Start <= e.end }

// Index walks the whole repository once and records what Check needs from it.
//
// The whole repository, not a list of source directories: the specification
// cites .github/workflows and .goreleaser.yml alongside Go, and a hand-kept
// list of roots reported 100 of those citations as naming a file that does not
// exist. A list of where code lives is a second map of the tree, and it drifts.
//
// Declarations come from go/ast rather than a pattern over the text, because a
// regex for "func at the start of a line" also matches the word inside a string
// or a comment, and a citation that binds on a false declaration is reported as
// drift that nobody can find. Files that do not parse contribute their length
// and no declarations: a citation into one is still checkable for PastEOF.
func Index(fsys fs.FS) (*Tree, error) {
	t := &Tree{
		lines:  map[string]int{},
		decls:  map[string]map[string]extent{},
		text:   map[string][]string{},
		suffix: map[string][]string{},
	}
	skip := ignoredDirs(fsys)
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[name] || path.Base(name) == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		t.lines[name] = lineCount(string(body))
		// Multi-segment tails only. Registering the bare basename too would
		// give resolve the "unique file of that name in the tree" fallback it
		// says it does not have: `sync.go:20` would answer with whichever
		// sync.go happens to lie in this checkout, which is the filesystem
		// answering a question the prose never did.
		for seg := strings.Split(name, "/"); len(seg) > 2; seg = seg[1:] {
			tail := strings.Join(seg[1:], "/")
			t.suffix[tail] = append(t.suffix[tail], name)
		}
		if strings.HasSuffix(name, ".go") {
			t.decls[name] = declarations(name, body)
			t.text[name] = strings.Split(string(body), "\n")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// lineCount is how many lines a file has, which is not one more than its
// newlines: a file ending in a newline has no line after it. The difference is
// exactly one, and one is the whole of the PastEOF test — counting the empty
// fragment after the final newline accepts a citation of line 4 in a 3-line
// file, and misstates the length in the message that explains the refusal.
func lineCount(body string) int {
	if body == "" {
		return 0
	}
	n := strings.Count(body, "\n")
	if strings.HasSuffix(body, "\n") {
		return n
	}
	return n + 1
}

// ignoredDirs reads the directories the repository declares are not part of it.
//
// Three of this tree's top-level directories hold code that is not lit's:
// sibling agent worktrees under .claude, a gitignored vendored project with
// more Go files than lit has itself, and a built site. Indexing them makes a
// citation's verdict depend on what is lying beside the checkout, so the same
// instrument measures one number locally and another in CI.
//
// .gitignore is the repository's own answer and is checked in, so this reads it
// rather than keeping a second list beside it. [LAW:one-source-of-truth] Only
// its plain directory entries are honoured — a pattern with a glob in it is not
// a directory to skip, and the corpus cites nothing inside one.
func ignoredDirs(fsys fs.FS) map[string]bool {
	out := map[string]bool{}
	body, err := fs.ReadFile(fsys, ".gitignore")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasSuffix(line, "/") {
			continue
		}
		if dir := strings.Trim(line, "/"); !strings.ContainsAny(dir, "*?[!") {
			out[dir] = true
		}
	}
	return out
}

// declarations maps every identifier a Go file declares to the lines it
// occupies: functions including their bodies, types including their fields, and
// the names bound by var and const.
//
// The extent, not just the opening line, because a chapter cites a passage
// inside a function at least as often as it cites the function's signature —
// "planFields ... (`apply.go:233-239`)" points at the field-change block in the
// middle of planFields, and a rule demanding the declaration line itself calls
// that drift.
//
// Struct fields are included, because these inventories name a struct and then
// walk its members. Function parameters are not: `moving` is a parameter of
// roomBesideTx, and binding a citation to it returns a verdict about a name
// that is not the one the sentence is naming.
//
// Declarations come from go/ast rather than a pattern over the text, because a
// regex for "func at the start of a line" also matches the word inside a string
// or a comment, and a citation binding on a false declaration is reported as
// drift nobody can find. Files that do not parse contribute their length and no
// declarations, so a citation into one is still checkable for PastEOF.
func declarations(name string, body []byte) map[string]extent {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, body, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	out := map[string]extent{}
	// Keep the first extent seen for a name. A later redeclaration in another
	// scope would otherwise move the answer to lines the prose never meant.
	put := func(id *ast.Ident, n ast.Node, doc *ast.CommentGroup) {
		if id == nil || id.Name == "_" {
			return
		}
		start := n.Pos()
		// The doc comment is part of the declaration for citation purposes: a
		// chapter citing a claim cites the sentence that states it. `rank.go:83-84`
		// is the comment above Midpoint saying what its empty arguments mean, and
		// the function itself opens on line 85. Excluding it calls the most
		// carefully chosen citations in the corpus drift, and it is a blind spot
		// recorded on this ticket before this package existed.
		if doc != nil {
			start = doc.Pos()
		}
		if _, seen := out[id.Name]; !seen {
			out[id.Name] = extent{fset.Position(start).Line, fset.Position(n.End()).Line}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			put(d.Name, d, d.Doc)
			// The signature is a declaration; the body is not. Descending into
			// it indexes a function's local var and const names as though the
			// file declared them, and because the first extent seen wins, a
			// local shadows a top-level declaration of the same name further
			// down — reporting a correct citation as drift and pointing the
			// reader into an unrelated function to find it.
			return false
		case *ast.TypeSpec:
			put(d.Name, d, d.Doc)
		case *ast.ValueSpec:
			for _, id := range d.Names {
				put(id, d, d.Doc)
			}
		case *ast.StructType:
			for _, f := range d.Fields.List {
				for _, id := range f.Names {
					put(id, f, f.Doc)
				}
			}
		}
		return true
	})
	return out
}

// resolve turns the path a sentence wrote into a path in the tree.
//
// A repository-relative path is already the answer. An absolute one is not
// portable: eight inventories were written with one machine's checkout baked
// in, and because the abbreviations under them inherit that prefix, 167
// citations name a directory that exists on exactly one computer. The longest
// suffix that is a real file recovers them anywhere. A partial path —
// `storage/sync.go` — is the mirror case, missing leading components rather
// than carrying extra ones, and is matched against the tail of each real path.
//
// A basename the specification never qualifies anywhere also reaches here and
// resolves to nothing, which is the honest answer: the prose does not say which
// file it means.
//
// There is deliberately no "unique file of that name in the tree" fallback. It
// makes the verdict depend on what is lying beside the checkout rather than on
// what the specification says, and beside this one sit sibling worktrees and a
// gitignored vendored project with more Go files than lit has itself. An
// instrument that answers differently on two machines is not measuring the
// corpus.
func (t *Tree) resolve(named string) (string, bool) {
	if _, ok := t.lines[named]; ok {
		return named, true
	}
	parts := strings.Split(strings.TrimPrefix(named, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if _, ok := t.lines[strings.Join(parts[i:], "/")]; ok {
			return strings.Join(parts[i:], "/"), true
		}
	}
	// Two files sharing a tail leave it unresolved rather than picking one.
	if cands := t.suffix[strings.TrimPrefix(named, "/")]; len(cands) == 1 {
		return cands[0], true
	}
	return "", false
}

// Check answers every citation against the tree.
//
// [LAW:dataflow-not-control-flow] every citation walks the same path and the
// verdict is a value, so there is no shape, no file type and no missing symbol
// that causes a citation to be skipped instead of answered. A corpus-wide count
// of the findings is therefore a count of the corpus.
func (t *Tree) Check(cites []Citation) []Finding {
	out := make([]Finding, 0, len(cites))
	for _, c := range cites {
		out = append(out, t.check(c))
	}
	return out
}

func (t *Tree) check(c Citation) Finding {
	file, ok := t.resolve(c.Named)
	if !ok {
		return Finding{Citation: c, Verdict: Unresolved}
	}
	f := Finding{Citation: c, File: file}
	if n := t.lines[file]; c.Span.End > n {
		f.Verdict, f.Lines = PastEOF, n
		return f
	}
	decls := t.decls[file]
	for _, name := range c.Nearby {
		e, declared := decls[name]
		if !declared {
			continue
		}
		f.Symbol, f.Declared = name, e.start
		f.Verdict = Moved
		// Two conventions, both legitimate, and a citation satisfying either is
		// about the symbol its sentence names. A chapter cites the declaration
		// — the span overlaps its extent — or it cites a passage that uses the
		// symbol, which is how exit.go's dispatch arms are cited: `:62-64` is
		// the errors.As arm returning ExitValidation, three lines that do not
		// contain its declaration and are exactly what the sentence means.
		//
		// Requiring the declaration line alone flagged both of those as drift.
		// That flood is the documented failure of boundary-shaped rules over
		// this corpus — an earlier endpoint checker flagged 33 citations of
		// which nearly all were legitimate — and a gate crying wolf at that
		// rate is read once and then ignored.
		if e.overlaps(c.Span) || t.names(file, c.Span, name) {
			f.Verdict = Holds
		}
		return f
	}
	f.Verdict = Unbound
	return f
}

// names reports whether the cited span mentions the symbol, as a whole word.
//
// Whole-word, because `Export` is a substring of `ExportDelta` and of
// `exported`, and a substring match would accept a citation using the wrong one
// as evidence for itself.
func (t *Tree) names(file string, s Span, sym string) bool {
	lines := t.text[file]
	if lines == nil {
		return false
	}
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(sym) + `\b`)
	for i := s.Start; i <= s.End && i <= len(lines); i++ {
		if word.MatchString(lines[i-1]) {
			return true
		}
	}
	return false
}

// Tally counts findings by verdict.
type Tally map[Verdict]int

func Count(findings []Finding) Tally {
	t := Tally{}
	for _, f := range findings {
		t[f.Verdict]++
	}
	return t
}

// Total is every citation counted, which is the denominator any share of the
// corpus has to be reported against.
func (t Tally) Total() int {
	n := 0
	for _, c := range t {
		n += c
	}
	return n
}

// Bound is the citations that name a symbol the cited file declares — the only
// ones whose truth is decidable, and so the honest denominator for a share that
// holds.
func (t Tally) Bound() int { return t[Holds] + t[Moved] }

// Survey resolves every citation in the specification against the tree.
//
// Which documents are the specification is docclaims' answer, not a second list
// here: two lists of the same set drift, and the one that drifts is whichever
// the next chapter is not added to. [LAW:one-source-of-truth]
func Survey(root fs.FS) ([]Finding, error) {
	names, err := docclaims.SpecFiles(root)
	if err != nil {
		return nil, err
	}
	docs := map[string]string{}
	for _, name := range names {
		body, err := fs.ReadFile(root, name)
		if err != nil {
			return nil, err
		}
		docs[name] = string(body)
	}
	tree, err := Index(root)
	if err != nil {
		return nil, err
	}
	glossary := BuildGlossary(docs)
	var out []Finding
	for _, name := range names {
		out = append(out, tree.Check(glossary.Apply(Parse(name, docs[name])))...)
	}
	return out, nil
}

// Shape is how a citation was spelled. It exists for the census alone and is
// deliberately not carried on Citation: downstream code that can ask which
// shape a citation had is downstream code that can handle one and skip another,
// which is the blindness this package is built to make unwritable.
type Shape int

const (
	// Qualified carries a path: `internal/model/priority.go:18-21`.
	Qualified Shape = iota
	// Abbreviated carries a bare basename: `priority.go:35-41`.
	Abbreviated
	// Continued carries no filename at all: `:47-54`.
	Continued
	// Tailed is a further span after a comma: the `117` of `x.go:113,117`.
	Tailed
)

func (s Shape) String() string {
	return [...]string{"qualified", "abbreviated", "continued", "comma-tailed"}[s]
}

// Census counts the citation spans in a document by how they were written.
//
// The corpus has been counted by hand at least five times and come out
// different every time, because a finder anchored on "what follows `file.go:`"
// reports only the qualified shape and its author reads the total as the
// corpus. A census that misses a quarter of its own subject understates
// coverage while the green stays green.
func Census(text string) map[Shape]int {
	out := map[Shape]int{}
	for _, m := range citeRe.FindAllStringSubmatchIndex(text, -1) {
		written := group(text, m, 1)
		switch {
		case written == "":
			out[Continued]++
		case strings.Contains(written, "/"):
			out[Qualified]++
		default:
			out[Abbreviated]++
		}
		out[Tailed] += len(spanRe.FindAllString(text[m[4]:m[5]], -1)) - 1
	}
	return out
}

// SpecText reads the specification's documents.
//
// Exported because the census and the survey are two questions over one corpus,
// and a caller that re-derived the file list would be keeping a second map of
// which documents are the specification. [LAW:one-source-of-truth]
func SpecText(root fs.FS) (map[string]string, error) {
	names, err := docclaims.SpecFiles(root)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		body, err := fs.ReadFile(root, name)
		if err != nil {
			return nil, err
		}
		out[name] = string(body)
	}
	return out, nil
}
