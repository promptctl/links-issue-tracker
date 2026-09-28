// Package docnames checks that every Go name the v1 specification writes in a
// code span is an identifier somewhere in this repository's source.
//
// doc-v1-total is what an agent reads to find code it has not seen, so a
// confident `runSyncPush` is followed with confidence. A name that exists
// nowhere sends that reader searching for a successor, and the reasonable guess
// that it was renamed is wrong when it never existed. No line citation guards
// against this, because a name that cites nothing has nothing to resolve.
// [LAW:one-source-of-truth] the source tree is the fact; a named identifier in
// a chapter is a map of it, and this package is what re-checks the map.
//
// What counts as a Go name is decided by shape alone: a code span whose callee
// is one or more dot-joined identifiers with a mixed-case segment, optionally
// followed by a call's arguments, whose own mixed-case identifiers are names
// too. `*T`, `[]T` and `(*T).M` read as the names they are built on. Lowercase
// words (`open`, `dolt_log()`) and all-caps words (`GOCACHE`, `VARCHAR(64)`)
// are left alone. In this corpus they are column names, statuses, commands,
// SQL and environment variables, which the shape cannot tell apart from Go.
//
// What counts as existing is an identifier in the code of any Go file of the
// source this repository carries, tests and tools included, and for a name
// qualified by one of its packages, a top-level declaration of that package. A
// name that survives only in a comment or a string does not count, because a
// comment naming a deleted function is exactly the residue the check is for.
//
// Some names are legitimately not in the tree: a standard-library or
// third-party symbol, an agent-harness name, a unit or a format pattern. Those
// are listed by name, one per line with the reason, in ExclusionsFile. The list
// is data rather than conditionals so the next chapter author extends it by
// adding a line, and the gate keeps it honest in both directions: an entry no
// chapter still writes, or one the tree now has, is reported as stale.
package docnames

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
)

// ExclusionsFile lists the names a chapter may write that are not identifiers
// in this tree. It lives beside the chapters because a chapter's author is the
// one who adds to it.
const ExclusionsFile = docclaims.SpecDir + "/names-outside-the-tree.txt"

// goName is a span shaped like a Go name: a dotted callee, optionally followed
// by a call's parenthesized arguments.
var goName = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)(\(.*\))?$`)

// methodExpr is the receiver form `(*T).M` or `(T).M`, read as `T.M`.
var methodExpr = regexp.MustCompile(`^\(\*?([A-Za-z_][A-Za-z0-9_]*)\)\.`)

// chain is one dotted identifier inside a call's arguments.
var chain = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)

// quoted is a string literal inside a call's arguments; its words are text,
// not names.
var quoted = regexp.MustCompile(`"[^"]*"|'[^']*'`)

// pathLike is a filesystem path or glob inside a call's arguments, as in a
// permission rule's `Read(//Users/…/**)`; its segments are not names.
var pathLike = regexp.MustCompile(`[^\s,()]*/[^\s,()]*`)

// Ref is one Go name a chapter writes: the dotted callee of a Go-shaped span,
// or a mixed-case identifier among its arguments.
type Ref struct {
	Doc  string
	Line int
	// Span is the code span as written; Name is the dotted name inside it.
	Span string
	Name string
}

// refsIn returns the Go names one span writes, or none when the span is not
// Go-shaped. Pointer, slice and method-expression forms (`*T`, `[]T`,
// `(*T).M`) are read as the names they are built on.
func refsIn(doc string, s docclaims.Span) []Ref {
	text := methodExpr.ReplaceAllString(strings.TrimLeft(s.Text, "*&[]"), "$1.")
	m := goName.FindStringSubmatch(text)
	if m == nil || !named(m[1]) {
		return nil
	}
	ref := func(name string) Ref { return Ref{Doc: doc, Line: s.Line, Span: s.Text, Name: name} }
	out := []Ref{ref(m[1])}
	for _, arg := range chain.FindAllString(pathLike.ReplaceAllString(quoted.ReplaceAllString(m[2], ""), ""), -1) {
		if named(arg) {
			out = append(out, ref(arg))
		}
	}
	return out
}

// named reports whether a dotted name is one the check judges: some segment
// mixes upper and lower case.
func named(dotted string) bool {
	return slices.ContainsFunc(strings.Split(dotted, "."), func(s string) bool {
		return strings.ToLower(s) != s && strings.ToUpper(s) != s
	})
}

// Refs reads the Go names the given chapters write, in line order within each
// chapter so a report reads top to bottom.
func Refs(fsys fs.FS, docs []string) ([]Ref, error) {
	var out []Ref
	for _, doc := range docs {
		src, err := fs.ReadFile(fsys, doc)
		if err != nil {
			return nil, err
		}
		spans, err := docclaims.CodeSpans(string(src))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", doc, err)
		}
		slices.SortStableFunc(spans, func(a, b docclaims.Span) int { return a.Line - b.Line })
		for _, s := range spans {
			out = append(out, refsIn(doc, s)...)
		}
	}
	return out, nil
}

// Tree is what the Go source this repository carries declares: every
// identifier in its code, and each package's top-level names.
type Tree struct {
	idents map[string]bool
	// pkgs maps a package name to the names declared at its top level and the
	// methods declared on its types, across every directory holding a package
	// of that name. Methods count because the corpus quotes code, and code
	// names a variable after its package: `store.Downgrade` is a call on a
	// *store.Store as often as a package function.
	pkgs map[string]map[string]bool
	// types maps a type name to its members: its fields, its methods, the
	// methods of an interface, and the types it embeds, whose members it
	// promotes. Keyed by bare name across every package, so two types sharing
	// a name share one member set.
	types map[string]*members
}

type members struct {
	names map[string]bool
	// embeds are the embedded types named in this tree; opaque is set when an
	// embedded type is from outside it, whose members the check cannot see.
	embeds []string
	opaque bool
}

func (t Tree) typeNamed(name string) *members {
	m := t.types[name]
	if m == nil {
		m = &members{names: map[string]bool{}}
		t.types[name] = m
	}
	return m
}

// hasMember reports whether a type has a field or method of that name, its
// own or promoted from an embedded type.
func (t Tree) hasMember(typ, name string, seen map[string]bool) bool {
	m := t.types[typ]
	if m == nil || seen[typ] {
		return false
	}
	seen[typ] = true
	if m.opaque || m.names[name] {
		return true
	}
	return slices.ContainsFunc(m.embeds, func(e string) bool { return t.hasMember(e, name, seen) })
}

// Has reports whether a dotted name exists. Every segment must be an
// identifier in the tree; a name qualified by a package this tree holds must
// be declared in that package, so `storage.IssueOrdering` is not satisfied by
// an `IssueOrdering` that moved to another package; and a member written on a
// type this tree declares must be that type's, so `Store.Close` is not
// satisfied by some other type's `Close`.
func (t Tree) Has(dotted string) bool {
	segs := strings.Split(dotted, ".")
	if slices.ContainsFunc(segs, func(s string) bool { return !t.idents[s] }) {
		return false
	}
	typ := 0
	if decls, isPkg := t.pkgs[segs[0]]; isPkg && len(segs) > 1 {
		if !decls[segs[1]] {
			return false
		}
		typ = 1
	}
	// Only an exported type name is read as a type. A lowercase one is as
	// often a variable — the corpus quotes `result.Head` from code where
	// `result` is a local — and a variable's members are its type's, which
	// the name alone does not say.
	if _, isType := t.types[segs[typ]]; !isType || !token.IsExported(segs[typ]) || typ+1 >= len(segs) {
		return true
	}
	return t.hasMember(segs[typ], segs[typ+1], map[string]bool{})
}

// ReadTree parses every Go file of the source this repository carries. A
// directory holding its own go.mod is another module and is walked only if
// go.mod replaces a dependency onto it; directories the go tool ignores by
// name (leading "." or "_", testdata) are skipped as the go tool skips them.
func ReadTree(fsys fs.FS) (Tree, error) {
	roots, err := docclaims.SourceDirs(fsys)
	if err != nil {
		return Tree{}, err
	}
	t := Tree{idents: map[string]bool{}, pkgs: map[string]map[string]bool{}, types: map[string]*members{}}
	for _, root := range roots {
		err := fs.WalkDir(fsys, root, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name != root && (ignoredByGo(d.Name()) || isModuleRoot(fsys, name)) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || ignoredByGo(d.Name()) {
				return nil
			}
			return t.add(fsys, name)
		})
		if err != nil {
			return Tree{}, err
		}
	}
	// An empty tree would report every name in the corpus as missing — the
	// whole specification false rather than this walk broken.
	// [LAW:no-silent-failure]
	if len(t.idents) == 0 {
		return Tree{}, fmt.Errorf("no Go identifiers found under %v", roots)
	}
	return t, nil
}

func ignoredByGo(base string) bool {
	return strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata"
}

func isModuleRoot(fsys fs.FS, dir string) bool {
	_, err := fs.Stat(fsys, path.Join(dir, "go.mod"))
	return err == nil
}

// add parses one file into the tree. Build constraints are not consulted: a
// file excluded from this platform's build still holds names a reader can
// find, which is the question asked.
//
// A parse failure is returned, not skipped: skipping would drop the file's
// names, and each would report as a chapter naming something that does not
// exist. [LAW:no-silent-failure]
func (t Tree) add(fsys fs.FS, name string) error {
	src, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			t.idents[id.Name] = true
		}
		return true
	})
	decls := t.pkgs[file.Name.Name]
	if decls == nil {
		decls = map[string]bool{}
		t.pkgs[file.Name.Name] = decls
	}
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			decls[d.Name.Name] = true
			if d.Recv != nil && len(d.Recv.List) == 1 {
				if recv, ok := typeName(d.Recv.List[0].Type); ok {
					t.typeNamed(recv).names[d.Name.Name] = true
				}
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					decls[spec.Name.Name] = true
					t.addMembers(t.typeNamed(spec.Name.Name), spec.Type)
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						decls[n.Name] = true
					}
				}
			}
		}
	}
	return nil
}

// addMembers records a type's fields, interface methods and embedded types. A
// type defined as another type (`type A B`) has no members of its own here and
// promotes nothing, which is Go's rule for methods; its fields are read through
// the struct it names only when that struct is written inline.
func (t Tree) addMembers(m *members, expr ast.Expr) {
	var fields *ast.FieldList
	switch e := expr.(type) {
	case *ast.StructType:
		fields = e.Fields
	case *ast.InterfaceType:
		fields = e.Methods
	default:
		return
	}
	for _, f := range fields.List {
		for _, n := range f.Names {
			m.names[n.Name] = true
		}
		if len(f.Names) > 0 {
			continue
		}
		// An embedded field: its type name is also the field's name, and its
		// members are promoted.
		if name, ok := typeName(f.Type); ok {
			m.names[name] = true
			m.embeds = append(m.embeds, name)
		} else {
			m.opaque = true
		}
		if _, external := f.Type.(*ast.SelectorExpr); external {
			m.opaque = true
		}
	}
}

// typeName is the bare name of a named type expression — `T`, `*T`, `T[K]` —
// and false for anything else.
func typeName(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name, true
	case *ast.StarExpr:
		return typeName(e.X)
	case *ast.IndexExpr:
		return typeName(e.X)
	case *ast.IndexListExpr:
		return typeName(e.X)
	}
	return "", false
}

// Exclusion is one line of ExclusionsFile: a dotted name as chapters write it,
// without any call, and why it is not in the tree.
type Exclusion struct {
	Line int
	Name string
	Why  string
}

// ParseExclusions reads ExclusionsFile. Blank lines and lines starting with
// "#" are commentary; every other line is a name, whitespace, then the reason.
// A line with no reason is refused, because the reason is what lets the next
// reader judge whether the entry still holds.
func ParseExclusions(src string) ([]Exclusion, error) {
	var out []Exclusion
	seen := map[string]int{}
	for i, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		text, why := line, ""
		if i := strings.IndexFunc(line, unicode.IsSpace); i >= 0 {
			text, why = line[:i], strings.TrimSpace(line[i:])
		}
		// An entry the check would never judge could never match a name, so it
		// would sit in the list excusing nothing. [LAW:parse-dont-validate]
		if m := goName.FindStringSubmatch(text); m == nil || m[2] != "" || !named(text) {
			return nil, fmt.Errorf("%s:%d: `%s` is not a name the check judges; list a dotted name with a mixed-case segment, without its call", ExclusionsFile, i+1, text)
		}
		if why == "" {
			return nil, fmt.Errorf("%s:%d: `%s` gives no reason it is outside the tree", ExclusionsFile, i+1, text)
		}
		if prev, dup := seen[text]; dup {
			return nil, fmt.Errorf("%s:%d: `%s` is already listed at line %d", ExclusionsFile, i+1, text, prev)
		}
		seen[text] = i + 1
		out = append(out, Exclusion{Line: i + 1, Name: text, Why: why})
	}
	return out, nil
}

// StaleExclusion is an entry that no longer excuses anything.
type StaleExclusion struct {
	Exclusion
	// Declared is true when the tree now has the name, false when no chapter
	// writes it any more.
	Declared bool
}

// Report is the outcome of one check.
type Report struct {
	// Missing are the names the tree does not have, in chapter and line
	// order.
	Missing []Ref
	Stale   []StaleExclusion
}

// Clean reports whether the check found nothing.
func (r Report) Clean() bool { return len(r.Missing) == 0 && len(r.Stale) == 0 }

// Check compares the chapters' names against the tree. It is a pure function of
// its inputs so a test can hand it a fabricated corpus.
func Check(refs []Ref, tree Tree, exclusions []Exclusion) Report {
	excluded := map[string]bool{}
	for _, e := range exclusions {
		excluded[e.Name] = true
	}
	written := map[string]bool{}
	var r Report
	for _, ref := range refs {
		written[ref.Name] = true
		if !excluded[ref.Name] && !tree.Has(ref.Name) {
			r.Missing = append(r.Missing, ref)
		}
	}
	for _, e := range exclusions {
		switch {
		case !written[e.Name]:
			r.Stale = append(r.Stale, StaleExclusion{Exclusion: e})
		case tree.Has(e.Name):
			r.Stale = append(r.Stale, StaleExclusion{Exclusion: e, Declared: true})
		}
	}
	return r
}

// CheckTree runs the check over the specification in fsys.
func CheckTree(fsys fs.FS) (Report, error) {
	docs, err := docclaims.SpecFiles(fsys)
	if err != nil {
		return Report{}, err
	}
	refs, err := Refs(fsys, docs)
	if err != nil {
		return Report{}, err
	}
	tree, err := ReadTree(fsys)
	if err != nil {
		return Report{}, err
	}
	src, err := fs.ReadFile(fsys, ExclusionsFile)
	if err != nil {
		return Report{}, err
	}
	exclusions, err := ParseExclusions(string(src))
	if err != nil {
		return Report{}, err
	}
	return Check(refs, tree, exclusions), nil
}
