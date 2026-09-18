package lifecycle

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// An ActionName is a string type, so `%s` interpolates it silently and renders
// the PERSISTED EVENT ENCODING — the wrong name in any sentence a caller reads.
// Nothing in the compiler objects, and the two names agree for seven of the
// eight actions, so a wrong site reads correctly almost always and is found by a
// reader holding a command that does not exist.
//
// Writing links-cli-errors-nvmd I swept for this by hand three times and missed
// a site each time. So the rule gets a gate — and the gate gets a parser.
//
// Two earlier versions of this file scanned text, and both failed open in ways
// that took a reviewer to find: one read a single physical line, so a call
// wrapped across lines was invisible; the next matched balanced parentheses over
// raw bytes, so a `)` inside a format string closed the call early and every
// argument after it went unexamined. `fmt.Sprintf("cannot :) %s", e.Action)`
// passed that version. Both holes have the same cause — Go source was being
// treated as text — so this version asks go/parser where the calls and their
// arguments actually are. [LAW:parse-dont-validate]
//
// THE RULE, which now has no exceptions. An expression that produces an
// ActionName may reach a formatting call only as:
//
//   - `x.Verb()`, the word the caller typed; or
//   - `string(x)`, written out to say the persisted encoding is meant here.
//
// Anything else is a site to look at. Every argument of every fmt call is
// examined, not the first match, so mixing the two in one call is caught.
//
// WHAT IT DOES NOT COVER, in full, because an incomplete account of a gate's
// blind spots is the same defect as a gate that overclaims:
//
//   - String building outside fmt. `lit bulk <verb>` builds its usage line by
//     concatenation, and that is the site of this class a reviewer found after
//     two of my own sweeps missed it. Widening the rule to all expressions was
//     measured, not assumed: it flags 82 sites, nearly all unrelated fields
//     named Action, and a gate that cries wolf 82 times gets switched off. That
//     site is pinned behaviorally by TestBulkUsageNamesTheTypedVerb instead.
//   - An ActionName reached through a name no declaration introduces: assigned
//     to a local by `:=`, or returned through an interface. Declared locals,
//     parameters, struct fields and package vars are all covered.

// formatters are the fmt functions whose output a caller can read.
var formatters = map[string]bool{
	"Errorf": true, "Sprintf": true, "Fprintf": true, "Printf": true,
	"Sprint": true, "Fprint": true, "Print": true,
	"Sprintln": true, "Fprintln": true, "Println": true,
}

func repoRootForVerbTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory; cannot locate the repo")
		}
		dir = parent
	}
}

// parsedTree returns every tracked non-test Go file, parsed. It fails rather
// than returning an empty set: a scanner handed nothing to scan reports success,
// which is indistinguishable from one that found nothing wrong.
// [LAW:no-silent-failure]
func parsedTree(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	root := repoRootForVerbTest(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			// A tracked source file that will not parse is a real fault, not
			// something to skip past.
			t.Fatalf("parsing %s: %v", rel, err)
		}
		files[rel] = f
	}
	if len(files) == 0 {
		t.Fatal("no tracked non-test Go files parsed — this scan would pass over an empty set")
	}
	return fset, files
}

// typeName renders the type of a declaration as its final identifier, so both
// `ActionName` and `model.ActionName` answer to the same name.
func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.StarExpr:
		return typeName(t.X)
	}
	return ""
}

// bearerNames collects the names an ActionName is reached through, kept in
// three sets because they are matched differently and conflating them broke the
// gate outright: a first version put every declaration in one set, which swept
// in the RECEIVER of `func (n ActionName) Verb()`, so the bare name `n` became
// a bearer and every `n.anything` in the repository matched. The gate flagged
// `n.name`, `n.license`, `n.scope` — a scanner that fires on unrelated code is
// a scanner that gets deleted.
//
//   - fields: struct fields typed ActionName, matched only as `x.<name>`.
//   - locals: parameters, results, receivers and vars typed ActionName, matched
//     as a bare identifier WITHIN THE FUNCTION THAT DECLARES THEM. This is what
//     makes a parameter visible, which the regex version could not see at all.
//     The scoping is not fastidiousness: a bare identifier's type is unknowable
//     without go/types, and `n` is a parameter name throughout this repository,
//     so matching it package-wide flagged `humanBytes(n int64)`.
//   - consts: package-level constants typed ActionName, matched anywhere, since
//     a constant's name is unique in its package.
//   - values: names declared as an Action, whose `.Name()` produces one.
//
// Receivers are collected like any other declaration. The problem they caused —
// `n.name` in unrelated code matching because the bare `n` was a bearer — is
// fixed where it belongs, in the walk below: a selector that is NOT itself a
// bearer is not descended into, so its receiver is never examined on its own.
// Excluding receiver names instead was aimed at the right problem from the
// wrong end, and blinded the gate to any parameter sharing the name.
func bearerNames(files map[string]*ast.File) (fields, locals, values []string) {
	fs, ls, vs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	add := func(into map[string]bool, names []*ast.Ident, typ ast.Expr) {
		switch typeName(typ) {
		case "ActionName":
			for _, n := range names {
				into[n.Name] = true
			}
		case "Action", "StatusAction", "RetentionAction":
			for _, n := range names {
				vs[n.Name] = true
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.StructType:
				for _, fld := range d.Fields.List {
					add(fs, fld.Names, fld.Type)
				}
			case *ast.FuncDecl:
				if d.Recv != nil {
					for _, fld := range d.Recv.List {
						add(ls, fld.Names, fld.Type)
					}
				}
			case *ast.FuncType:
				for _, grp := range []*ast.FieldList{d.Params, d.Results} {
					if grp == nil {
						continue
					}
					for _, fld := range grp.List {
						add(ls, fld.Names, fld.Type)
					}
				}
			case *ast.ValueSpec:
				add(ls, d.Names, d.Type)
			}
			return true
		})
	}
	for n := range fs {
		fields = append(fields, n)
	}
	for n := range ls {
		locals = append(locals, n)
	}
	for n := range vs {
		values = append(values, n)
	}
	sort.Strings(fields)
	sort.Strings(locals)
	sort.Strings(values)
	return fields, locals, values
}

// producesActionName reports whether e reaches an ActionName: a selector ending
// in a field typed ActionName (`w.action`), or Name() called on a value declared
// as an Action (`action.Name()`).
func producesActionName(e ast.Expr, fields, locals, values map[string]bool) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return fields[x.Sel.Name]
	case *ast.Ident:
		return locals[x.Name]
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Name" {
			return false
		}
		recv, ok := sel.X.(*ast.Ident)
		return ok && values[recv.Name]
	}
	return false
}

// argIsAllowed applies the rule to one argument of one fmt call.
func argIsAllowed(e ast.Expr, fields, locals, values map[string]bool) (allowed, relevant bool) {
	if call, ok := e.(*ast.CallExpr); ok {
		// `x.Verb()` — the word the caller typed.
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Verb" {
			if producesActionName(sel.X, fields, locals, values) {
				return true, true
			}
		}
		// `string(x)` — the persisted encoding, said out loud.
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "string" && len(call.Args) == 1 {
			if producesActionName(call.Args[0], fields, locals, values) {
				return true, true
			}
		}
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if n == nil || found {
			return false
		}
		ex, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		if producesActionName(ex, fields, locals, values) {
			found = true
			return false
		}
		// A selector that is not itself a bearer is somebody else's field, and
		// its receiver is not under discussion: descending into `n.name` to find
		// a bare `n` is how this gate once flagged `n.license`.
		if _, isSel := ex.(*ast.SelectorExpr); isSel {
			return false
		}
		return true
	})
	return !found, found
}

// scopedLocals returns, for one file, the byte ranges of each function together
// with the ActionName-typed names declared inside it.
type localScope struct {
	start, end token.Pos
	names      map[string]bool
}

func scopedLocals(f *ast.File) []localScope {
	var out []localScope
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		names := map[string]bool{}
		collect := func(list *ast.FieldList) {
			if list == nil {
				return
			}
			for _, fld := range list.List {
				if typeName(fld.Type) != "ActionName" {
					continue
				}
				for _, nm := range fld.Names {
					names[nm.Name] = true
				}
			}
		}
		collect(fd.Recv)
		if fd.Type != nil {
			collect(fd.Type.Params)
			collect(fd.Type.Results)
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			if vs, ok := n.(*ast.ValueSpec); ok && typeName(vs.Type) == "ActionName" {
				for _, nm := range vs.Names {
					names[nm.Name] = true
				}
			}
			return true
		})
		if len(names) > 0 {
			out = append(out, localScope{fd.Pos(), fd.End(), names})
		}
	}
	return out
}

func TestNoActionNameReachesAMessageAsItsPersistedEncoding(t *testing.T) {
	fset, files := parsedTree(t)
	fieldDecls, _, valueDecls := bearerNames(files)
	if len(fieldDecls) == 0 || len(valueDecls) == 0 {
		t.Fatalf("derived %d ActionName fields and %d Action values; with neither, this test checks nothing", len(fieldDecls), len(valueDecls))
	}
	fields, values := map[string]bool{}, map[string]bool{}
	for _, n := range fieldDecls {
		fields[n] = true
	}
	for _, n := range valueDecls {
		values[n] = true
	}
	// Package-level constants typed ActionName: unique names, matched anywhere.
	consts := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok && typeName(vs.Type) == "ActionName" {
					for _, nm := range vs.Names {
						consts[nm.Name] = true
					}
				}
			}
		}
	}
	bad := []string{}
	for rel, f := range files {
		scopes := scopedLocals(f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || !formatters[sel.Sel.Name] {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
				return true
			}
			locals := map[string]bool{}
			for k := range consts {
				locals[k] = true
			}
			for _, sc := range scopes {
				if call.Pos() >= sc.start && call.Pos() < sc.end {
					for k := range sc.names {
						locals[k] = true
					}
				}
			}
			for _, arg := range call.Args {
				allowed, relevant := argIsAllowed(arg, fields, locals, values)
				if relevant && !allowed {
					pos := fset.Position(arg.Pos())
					bad = append(bad, fmt.Sprintf("%s:%d: argument to fmt.%s", rel, pos.Line, sel.Sel.Name))
				}
			}
			return true
		})
	}
	if len(bad) > 0 {
		t.Fatalf("an action's persisted event encoding reaches a message a caller reads.\nUse x.Verb() for the word the caller typed, or write string(x) to say the persisted encoding is meant:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestActionNameSpellingsAreAccountedFor keeps the gate honest. It derives what
// it scans for, but a derivation is still a claim about the tree, and the claim
// is that these are ALL the names an ActionName is reached through. Pinning the
// sets means a new one fails here and sends someone to re-read the rule, rather
// than quietly widening the set the scan never looks at.
func TestActionNameSpellingsAreAccountedFor(t *testing.T) {
	_, files := parsedTree(t)
	fields, _, values := bearerNames(files)
	// The scoped locals are recomputed the way the GATE computes them, not the
	// way a sibling helper happens to. A tripwire that guards a different
	// derivation from the one the gate reads guards nothing, which is the
	// mistake this whole ticket is about, one level up.
	scoped := map[string]bool{}
	for _, f := range files {
		for _, sc := range scopedLocals(f) {
			for n := range sc.names {
				scoped[n] = true
			}
		}
	}
	got := []string{}
	for n := range scoped {
		got = append(got, n)
	}
	sort.Strings(got)

	// ContainerActionError.Action, and the status and retention writers in the
	// Dolt store, whose fields are both named `action`.
	wantFields := []string{"Action", "action"}
	// The parameter every Apply and plan path names its action.
	wantValues := []string{"Action", "action"}
	// Verb()'s own receiver: the only ActionName declared inside a function.
	wantScoped := []string{"n"}

	check := func(what string, got, want []string) {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v -- a surprise here is either a real new spelling to add, or a derivation matching something that is not a declaration; both need reading before this gate can be trusted", what, got, want)
		}
	}
	check("struct fields typed ActionName", fields, wantFields)
	check("values typed Action", values, wantValues)
	check("function-scoped locals typed ActionName", got, wantScoped)
}
