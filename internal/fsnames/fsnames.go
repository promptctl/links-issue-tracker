// Package fsnames finds io/fs names built with package path/filepath.
//
// An io/fs name is always slash-separated, on every OS. filepath builds names
// with the host's separator, so a name it builds works on Linux and macOS and
// fails every lookup on Windows, and per-PR CI runs only on Linux. path is the
// builder for an io/fs name; filepath.ToSlash converts a host path into one.
//
// The check reads types: a read is a call to one of io/fs's functions, or a
// call to a name-taking method on a value whose type has io/fs's Open, so a
// filesystem is recognised however and wherever it was declared.
//
// A name is traced through variables assigned exactly once.
//
// WHAT IT DOES NOT COVER: a name built inside a helper such as pathspec's
// Join, or in a variable assigned more than once, or a name or pattern handed
// to a reader outside io/fs, such as template.ParseFS.
package fsnames

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
)

// Finding is a read whose name can hold the host's separator.
type Finding struct {
	Pos  token.Position // where the name picked up the separator
	Read string         // the read, as "fs.ReadFile" or "<receiver type>.ReadFile"
	Name string         // what put it there, as "filepath.Join" or "os.PathSeparator"
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %s name built with %s", f.Pos, f.Read, f.Name)
}

// fsFuncs are io/fs's functions that take a filesystem and then a name.
var fsFuncs = []string{"ReadFile", "ReadDir", "Stat", "Lstat", "ReadLink", "Sub", "Glob", "WalkDir"}

// fsMethods are io/fs's interface methods, each of which takes one name.
var fsMethods = []string{"Open", "ReadFile", "ReadDir", "Stat", "Lstat", "ReadLink", "Sub", "Glob"}

// separatorFree are the filepath functions whose result holds no separator, or
// holds only slashes, so it is a valid io/fs name or name element on every OS.
var separatorFree = []string{"ToSlash", "Base", "Ext"}

// Check returns every io/fs read in files, the type-checked files of one
// package, whose name argument calls a filepath function that can put the
// host's separator into the name.
func Check(fset *token.FileSet, files []*ast.File, info *types.Info) []Finding {
	assigned := singleAssignments(files, info)
	var found []Finding
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			read, arg := readOf(call, info)
			if arg < 0 || arg >= len(call.Args) {
				return true
			}
			for _, b := range hostBuilders(call.Args[arg], info, assigned) {
				found = append(found, Finding{Pos: fset.Position(b.pos), Read: read, Name: b.name})
			}
			return true
		})
	}
	return found
}

// readOf names call and the index of its name argument when call is an io/fs
// read, and returns -1 otherwise.
func readOf(call *ast.CallExpr, info *types.Info) (string, int) {
	if fn := calleeOf(call, info); fn != nil && isIOFS(fn.Pkg()) &&
		fn.Signature().Recv() == nil && slices.Contains(fsFuncs, fn.Name()) {
		return "fs." + fn.Name(), 1
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return "", -1
	}
	selection, ok := info.Selections[sel]
	if !ok || !slices.Contains(fsMethods, sel.Sel.Name) || !isFilesystem(selection.Recv()) {
		return "", -1
	}
	method, ok := selection.Obj().(*types.Func)
	if !ok || !takesOneName(method.Signature()) {
		return "", -1
	}
	read := types.TypeString(selection.Recv(), (*types.Package).Name) + "." + sel.Sel.Name
	switch selection.Kind() {
	case types.MethodVal:
		return read, 0
	case types.MethodExpr:
		return read, 1 // the receiver is the first argument
	}
	return "", -1
}

// isFilesystem reports whether t has io/fs's FS method, Open(string) (fs.File,
// error) — the method every filesystem type has and nothing else need.
func isFilesystem(t types.Type) bool {
	obj, _, _ := types.LookupFieldOrMethod(t, true, nil, "Open")
	fn, ok := obj.(*types.Func)
	if !ok || !takesOneName(fn.Signature()) {
		return false
	}
	named, ok := types.Unalias(fn.Signature().Results().At(0).Type()).(*types.Named)
	return ok && isIOFS(named.Obj().Pkg()) && named.Obj().Name() == "File"
}

// takesOneName reports whether sig has io/fs's method shape: one string in,
// a result and an error out.
func takesOneName(sig *types.Signature) bool {
	if sig.Params().Len() != 1 || sig.Results().Len() != 2 {
		return false
	}
	return types.Identical(sig.Params().At(0).Type(), types.Typ[types.String]) &&
		types.Identical(sig.Results().At(1).Type(), types.Universe.Lookup("error").Type())
}

func isIOFS(pkg *types.Package) bool { return pkg != nil && pkg.Path() == "io/fs" }

// hostSeparators are the constants that hold the host's separator, by package.
var hostSeparators = map[string]string{"path/filepath": "Separator", "os": "PathSeparator"}

// builder is where a name picked up the host's separator.
type builder struct {
	pos  token.Pos
	name string // as "filepath.Join" or "os.PathSeparator"
}

// hostBuilders returns where expr's value can pick up the host's separator: a
// call into path/filepath, a separator constant, or either in the one
// assignment of a variable expr reads. A separator-free call's argument is not
// searched: whatever built it, the call's result is a valid name.
func hostBuilders(expr ast.Expr, info *types.Info, assigned map[types.Object]ast.Expr) []builder {
	var found []builder
	followed := map[types.Object]bool{} // a name is followed once, so a cycle ends
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			fn := calleeOf(n, info)
			if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "path/filepath" {
				return true
			}
			if !slices.Contains(separatorFree, fn.Name()) {
				found = append(found, builder{n.Pos(), "filepath." + fn.Name()})
			}
			return false
		case *ast.Ident:
			obj := info.Uses[n]
			if c, ok := obj.(*types.Const); ok && c.Pkg() != nil && hostSeparators[c.Pkg().Path()] == c.Name() {
				found = append(found, builder{n.Pos(), c.Pkg().Name() + "." + c.Name()})
			}
			if init, ok := assigned[obj]; ok && !followed[obj] {
				followed[obj] = true
				ast.Inspect(init, visit)
			}
		}
		return true
	}
	ast.Inspect(expr, visit)
	return found
}

// singleAssignments maps each variable or constant files assign exactly once,
// one value to one name, to the value it is assigned.
func singleAssignments(files []*ast.File, info *types.Info) map[types.Object]ast.Expr {
	values := map[types.Object]ast.Expr{}
	count := map[types.Object]int{}
	record := func(id *ast.Ident, value ast.Expr) {
		obj := info.Defs[id]
		if obj == nil {
			obj = info.Uses[id]
		}
		switch obj.(type) {
		case *types.Var, *types.Const:
			count[obj]++
			values[obj] = value
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						var value ast.Expr
						if len(n.Lhs) == len(n.Rhs) {
							value = n.Rhs[i]
						}
						record(id, value)
					}
				}
			case *ast.ValueSpec:
				for i, id := range n.Names {
					var value ast.Expr
					if len(n.Names) == len(n.Values) {
						value = n.Values[i]
					}
					record(id, value)
				}
			}
			return true
		})
	}
	for v, n := range count {
		if n != 1 || values[v] == nil {
			delete(values, v)
		}
	}
	return values
}

// calleeOf is the function or method call statically calls, or nil.
func calleeOf(call *ast.CallExpr, info *types.Info) *types.Func {
	var id *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	default:
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}
