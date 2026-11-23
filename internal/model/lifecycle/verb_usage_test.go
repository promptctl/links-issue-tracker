package lifecycle

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// An ActionName is a string type, so `%s` interpolates it silently and renders
// the PERSISTED EVENT ENCODING — the wrong name in any sentence a caller reads.
// Nothing in the compiler objects, and the two names agree for seven of the
// eight actions, so a wrong site reads correctly almost always and is found by a
// reader holding a command that does not exist.
//
// The gate asks go/types, not the source text, which expressions are
// ActionNames: a name match cannot see an ActionName held by a local made with
// `:=` or returned by a call, and flags every unrelated field that happens to
// share a bearer's name (go test's JSON `Action`, a string). Every package of
// the module is type-checked and examined; none is skipped for not importing
// this one, since a package that never names the type can still be handed a
// value of it. [LAW:parse-dont-validate]
//
// THE RULE, which has no exceptions. An expression whose type is ActionName
// may reach an argument of a formatting call only as:
//
//   - `x.Verb()`, the word the caller typed; or
//   - `string(x)`, written out to say the persisted encoding is meant here.
//
// Anything else is a site to look at. Every argument of every fmt call is
// examined, and every expression inside it, so mixing the two in one call, or
// wrapping an ActionName in another expression (`any(x)`), is caught.
//
// WHAT IT DOES NOT COVER, in full, because an incomplete account of a gate's
// blind spots is the same defect as a gate that overclaims:
//
//   - String building outside fmt. `lit bulk <verb>` builds its usage line by
//     concatenation; that site is pinned behaviorally by
//     TestBulkUsageNamesTheTypedVerb instead.
//   - An ActionName stored into an interface value before the call that
//     formats it (`var v any = x; fmt.Sprint(v)`): at the call, its type is
//     the interface's.
//   - Files the module's default build does not compile: other build tags,
//     other GOOS, and test files.

// formatters are the fmt functions whose output a caller can read.
var formatters = map[string]bool{
	"Errorf": true, "Sprintf": true, "Fprintf": true, "Printf": true,
	"Sprint": true, "Fprint": true, "Print": true,
	"Sprintln": true, "Fprintln": true, "Println": true,
}

// isActionName reports whether t is this package's ActionName, through any
// alias of it (model.ActionName). The package's path is read off the type
// itself, so the gate cannot drift from the type it protects.
func isActionName(t types.Type) bool {
	want := reflect.TypeFor[ActionName]()
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == want.PkgPath() && n.Obj().Name() == want.Name()
}

// formattedActionNames applies the rule to files, whose types info holds. It
// returns each argument that breaks it, by position, and how many arguments
// carry an ActionName the allowed way.
func formattedActionNames(fset *token.FileSet, files []*ast.File, info *types.Info) (bad []string, allowed int) {
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			fn, ok := info.Uses[sel.Sel].(*types.Func)
			if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "fmt" || !formatters[fn.Name()] {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(n ast.Node) bool {
					e, ok := n.(ast.Expr)
					if !ok {
						return true
					}
					if verbOrString(e, info) {
						allowed++
						return false
					}
					if isActionName(info.TypeOf(e)) {
						bad = append(bad, fmt.Sprintf("%s: argument to fmt.%s", fset.Position(e.Pos()), fn.Name()))
						return false
					}
					return true
				})
			}
			return true
		})
	}
	return bad, allowed
}

// verbOrString reports whether e is one of the rule's two allowed shapes.
func verbOrString(e ast.Expr, info *types.Info) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) > 1 {
		return false
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Verb" && len(call.Args) == 0 {
		return isActionName(info.TypeOf(sel.X))
	}
	tv, ok := info.Types[call.Fun]
	return ok && tv.IsType() && len(call.Args) == 1 &&
		types.Identical(tv.Type, types.Typ[types.String]) && isActionName(info.TypeOf(call.Args[0]))
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

func TestNoActionNameReachesAMessageAsItsPersistedEncoding(t *testing.T) {
	root := repoRootForVerbTest(t)
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  root,
	}, "./...")
	if err != nil {
		t.Fatalf("loading the module's packages: %v", err)
	}
	// A package that did not type-check is one the gate cannot see into: it
	// fails, rather than passing over it. [LAW:no-silent-failure]
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	})
	if len(loadErrs) > 0 {
		t.Fatalf("the module did not type-check, so the gate cannot examine it:\n  %s", strings.Join(loadErrs, "\n  "))
	}
	var bad []string
	allowed := 0
	for _, p := range pkgs {
		b, a := formattedActionNames(p.Fset, p.Syntax, p.TypesInfo)
		for _, s := range b {
			if rel, err := filepath.Rel(root, s); err == nil {
				s = rel
			}
			bad = append(bad, s)
		}
		allowed += a
	}
	// A gate that found no ActionName at any formatting call is not looking
	// where they are: the tree formats verbs, so it must see them.
	if allowed == 0 {
		t.Fatalf("type-checked %d packages and found no ActionName formatted the allowed way; the gate examines nothing", len(pkgs))
	}
	slices.Sort(bad)
	if len(bad) > 0 {
		t.Fatalf("an action's persisted event encoding reaches a message a caller reads.\nUse x.Verb() for the word the caller typed, or write string(x) to say the persisted encoding is meant:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestTheGateCatchesEveryShapeOfAFormattedActionName plants each shape the
// rule names in a package that imports this one, and requires the gate to
// flag exactly the ones it forbids.
func TestTheGateCatchesEveryShapeOfAFormattedActionName(t *testing.T) {
	const src = `package probe

import (
	"fmt"
	f "fmt"

	"github.com/promptctl/links-issue-tracker/internal/model/lifecycle"
)

type event struct{ Action lifecycle.ActionName }

func name() lifecycle.ActionName { return "" }

func sites(e event, n lifecycle.ActionName) {
	local := n
	_ = fmt.Sprintf("%s", e.Action)            // bad: a field
	_ = fmt.Sprintf("%s %s", n.Verb(), n)      // bad: the second
	_ = fmt.Errorf("cannot :) %s", local)      // bad: a := local
	_ = fmt.Sprint(name())                     // bad: a call's result
	_ = fmt.Sprintf("%v", any(e.Action))       // bad: through any
	_ = f.Sprintf("%s",
		n)                                     // bad: an aliased fmt, wrapped
	_ = fmt.Sprintf("%s", n.Verb())            // allowed
	_ = fmt.Sprintf("%s", string(e.Action))    // allowed
	_ = fmt.Sprintf("%s", "unrelated")         // nothing
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("example.com/probe", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	bad, allowed := formattedActionNames(fset, []*ast.File{file}, info)
	var lines []int
	for _, b := range bad {
		var line, col int
		if _, err := fmt.Sscanf(strings.TrimPrefix(b, "probe.go:"), "%d:%d", &line, &col); err != nil {
			t.Fatalf("unreadable position in %q: %v", b, err)
		}
		lines = append(lines, line)
	}
	if want := []int{16, 17, 18, 19, 20, 22}; !slices.Equal(lines, want) {
		t.Errorf("flagged lines %v, want %v:\n  %s", lines, want, strings.Join(bad, "\n  "))
	}
	if allowed != 3 {
		t.Errorf("allowed %d arguments, want 3 (n.Verb() twice, string(e.Action) once)", allowed)
	}
}
