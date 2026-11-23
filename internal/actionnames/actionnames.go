// Package actionnames finds an ActionName formatted into a message as its
// persisted event encoding.
//
// An ActionName is a string type, so `%s` interpolates it silently and renders
// the PERSISTED EVENT ENCODING — the wrong name in any sentence a caller reads.
// Nothing in the compiler objects, and the two names agree for seven of the
// eight actions, so a wrong site reads correctly almost always and is found by
// a reader holding a command that does not exist.
//
// The check reads types, not source text: a name match cannot see an
// ActionName held by a local made with `:=` or returned by a call, and it
// flags every unrelated field that shares a bearer's name.
// [LAW:parse-dont-validate]
//
// THE RULE, which has no exceptions. An expression whose type is ActionName
// may reach an argument of a fmt formatting call only as:
//
//   - `x.Verb()`, the word the caller typed; or
//   - `string(x)`, written out to say the persisted encoding is meant here.
//
// Every expression inside every argument is examined, so mixing the two in
// one call, or wrapping an ActionName in another expression (`any(x)`), is
// caught.
//
// WHAT IT DOES NOT COVER:
//
//   - String building outside fmt. `lit bulk <verb>` builds its usage line by
//     concatenation; that site is pinned behaviorally by
//     TestBulkUsageNamesTheTypedVerb instead.
//   - An ActionName stored into an interface value before the call that
//     formats it (`var v any = x; fmt.Sprint(v)`): at the call, its type is
//     the interface's.
package actionnames

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Finding is an argument of a formatting call that carries an ActionName
// other than as x.Verb() or string(x).
type Finding struct {
	Pos  token.Position
	Call string // the formatting call, as "fmt.Sprintf"
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: argument to %s", f.Pos, f.Call)
}

// formatters are the fmt functions whose output a caller can read.
var formatters = map[string]bool{
	"Errorf": true, "Sprintf": true, "Fprintf": true, "Printf": true,
	"Sprint": true, "Fprint": true, "Print": true,
	"Sprintln": true, "Fprintln": true, "Println": true,
}

// isActionName reports whether t is lifecycle's ActionName, through any
// alias of it (model.ActionName). The type's identity is read off the type
// itself, so the check cannot drift from the type it protects.
func isActionName(t types.Type) bool {
	want := reflect.TypeFor[model.ActionName]()
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == want.PkgPath() && n.Obj().Name() == want.Name()
}

// Check applies the rule to files, the type-checked files of one package. It
// returns each argument that breaks it, and how many arguments carry an
// ActionName the allowed way.
func Check(fset *token.FileSet, files []*ast.File, info *types.Info) (found []Finding, allowed int) {
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
						found = append(found, Finding{Pos: fset.Position(e.Pos()), Call: "fmt." + fn.Name()})
						return false
					}
					return true
				})
			}
			return true
		})
	}
	return found, allowed
}

// verbOrString reports whether e is one of the rule's two allowed shapes.
func verbOrString(e ast.Expr, info *types.Info) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Verb" && len(call.Args) == 0 {
		return isActionName(info.TypeOf(sel.X))
	}
	tv, ok := info.Types[call.Fun]
	return ok && tv.IsType() && len(call.Args) == 1 &&
		types.Identical(tv.Type, types.Typ[types.String]) && isActionName(info.TypeOf(call.Args[0]))
}
