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
// THE RULE, which has no exceptions. An expression whose type is ActionName,
// or holds one (a slice, array, map, pointer or struct with an ActionName in
// it, which %v prints), may reach an argument of a formatting call only as:
//
//   - `x.Verb()`, the word the caller typed; or
//   - `string(x)`, written out to say the persisted encoding is meant here.
//
// A formatting call is a call of fmt's Print, Fprint, Sprint and Append
// families and Errorf, or of a printf wrapper: a function or method whose
// final parameter `...any` follows a string parameter, and which forwards
// that parameter (`args...`) to a printf function — fmt's or another
// wrapper's — in any package Wrappers is given.
//
// Every expression inside every argument is examined for an ActionName, so
// mixing the two in one call, or wrapping an ActionName in another
// expression (`any(x)`), is caught. A container holding one is caught where
// it is the value formatted (`ns`, `any(ns)`, `&e`), not where it is read for
// another value (`e.Progress`, `len(ns)`).
//
// WHAT IT DOES NOT COVER:
//
//   - String building outside fmt. `lit bulk <verb>` builds its usage line by
//     concatenation; that site is pinned behaviorally by
//     TestBulkUsageNamesTheTypedVerb instead.
//   - An ActionName stored into an interface value before the call that
//     formats it (`var v any = x; fmt.Sprint(v)`): at the call, its type is
//     the interface's.
//   - A wrapper that is not printf-shaped: one that forwards to Sprint or
//     Println, takes its arguments as a []any rather than `...any`, or is a
//     function literal.
package actionnames

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"strings"

	"golang.org/x/tools/go/types/typeutil"

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
	"Errorf": true, "Sprintf": true, "Fprintf": true, "Printf": true, "Appendf": true,
	"Sprint": true, "Fprint": true, "Print": true, "Append": true,
	"Sprintln": true, "Fprintln": true, "Println": true, "Appendln": true,
}

// Package is one type-checked package's files.
type Package struct {
	Files []*ast.File
	Info  *types.Info
}

// Wrappers is every printf wrapper declared in pkgs, by its full name
// (types.Func.FullName). A wrapper forwarding to another wrapper is one too,
// wherever among pkgs the other is declared.
func Wrappers(pkgs []Package) map[string]bool {
	wrappers := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, p := range pkgs {
			for _, f := range p.Files {
				for _, d := range f.Decls {
					fd, ok := d.(*ast.FuncDecl)
					if !ok || fd.Body == nil {
						continue
					}
					fn, ok := p.Info.Defs[fd.Name].(*types.Func)
					if !ok || wrappers[fn.FullName()] {
						continue
					}
					if args := printfArgs(fn); args != nil && forwards(fd.Body, args, p.Info, wrappers) {
						wrappers[fn.FullName()] = true
						changed = true
					}
				}
			}
		}
	}
	return wrappers
}

// printfArgs is fn's final `...any` parameter when a string parameter
// precedes it, else nil.
func printfArgs(fn *types.Func) *types.Var {
	sig := fn.Signature()
	n := sig.Params().Len()
	if !sig.Variadic() || n < 2 {
		return nil
	}
	last := sig.Params().At(n - 1)
	s, ok := types.Unalias(last.Type()).(*types.Slice)
	if !ok {
		return nil
	}
	elem, ok := types.Unalias(s.Elem()).(*types.Interface)
	if !ok || !elem.Empty() || !types.Identical(sig.Params().At(n-2).Type().Underlying(), types.Typ[types.String]) {
		return nil
	}
	return last
}

// forwards reports whether body passes args on, as `args...`, to a printf
// function: fmt's or a wrapper's.
func forwards(body *ast.BlockStmt, args *types.Var, info *types.Info, wrappers map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if found || !ok || !call.Ellipsis.IsValid() || len(call.Args) == 0 {
			return !found
		}
		last, ok := ast.Unparen(call.Args[len(call.Args)-1]).(*ast.Ident)
		if !ok || info.Uses[last] != args {
			return true
		}
		name, ok := formatting(call, info, wrappers)
		found = ok && (wrappers[name] || strings.HasSuffix(name, "f"))
		return !found
	})
	return found
}

// formatting names call's callee when it is a formatting call.
func formatting(call *ast.CallExpr, info *types.Info, wrappers map[string]bool) (string, bool) {
	fn, ok := typeutil.Callee(info, call).(*types.Func)
	if !ok {
		return "", false
	}
	fn = fn.Origin()
	if fn.Pkg() != nil && fn.Pkg().Path() == "fmt" && formatters[fn.Name()] {
		return "fmt." + fn.Name(), true
	}
	return fn.FullName(), wrappers[fn.FullName()]
}

// isActionName reports whether t is lifecycle's ActionName, through any
// alias of it (model.ActionName). The type's identity is read off the type
// itself, so the check cannot drift from the type it protects.
func isActionName(t types.Type) bool {
	want := reflect.TypeFor[model.ActionName]()
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == want.PkgPath() && n.Obj().Name() == want.Name()
}

// holdsActionName reports whether a value of type t is an ActionName or
// holds one that %v prints: through slices, arrays, maps, pointers and
// struct fields.
func holdsActionName(t types.Type) bool {
	seen := map[types.Type]bool{}
	var holds func(types.Type) bool
	holds = func(t types.Type) bool {
		t = types.Unalias(t)
		if t == nil || seen[t] {
			return false
		}
		seen[t] = true
		if isActionName(t) {
			return true
		}
		switch u := t.Underlying().(type) {
		case *types.Slice:
			return holds(u.Elem())
		case *types.Array:
			return holds(u.Elem())
		case *types.Pointer:
			return holds(u.Elem())
		case *types.Map:
			return holds(u.Key()) || holds(u.Elem())
		case *types.Struct:
			for i := range u.NumFields() {
				if holds(u.Field(i).Type()) {
					return true
				}
			}
		}
		return false
	}
	return holds(t)
}

// Check applies the rule to files, the type-checked files of one package,
// with the printf wrappers Wrappers found. It returns each argument that
// breaks it, and how many arguments carry an ActionName the allowed way.
func Check(fset *token.FileSet, files []*ast.File, info *types.Info, wrappers map[string]bool) (found []Finding, allowed int) {
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := formatting(call, info, wrappers)
			if !ok {
				return true
			}
			for _, arg := range call.Args {
				if e, ok := heldInValue(arg, info); ok {
					found = append(found, Finding{Pos: fset.Position(e.Pos()), Call: name})
					continue
				}
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
						found = append(found, Finding{Pos: fset.Position(e.Pos()), Call: name})
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

// heldInValue finds a container holding an ActionName that is the value arg
// formats: arg itself, or what it is through parentheses, a conversion
// (`any(ns)`), `&` or `*`. A container read for another value (`e.Progress`,
// `len(ns)`) formats that value, not the container.
func heldInValue(arg ast.Expr, info *types.Info) (ast.Expr, bool) {
	for e := arg; ; {
		e = ast.Unparen(e)
		if t := info.TypeOf(e); !isActionName(t) && holdsActionName(t) {
			return e, true
		}
		switch x := e.(type) {
		case *ast.CallExpr:
			if tv, ok := info.Types[x.Fun]; !ok || !tv.IsType() || len(x.Args) != 1 {
				return nil, false
			}
			e = x.Args[0]
		case *ast.UnaryExpr:
			if x.Op != token.AND {
				return nil, false
			}
			e = x.X
		case *ast.StarExpr:
			e = x.X
		default:
			return nil, false
		}
	}
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
