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
// WHAT IT DOES NOT COVER: a name built with filepath outside the read's own
// argument — stored in a variable first, or built inside a helper such as
// pathspec's Join.
package fsnames

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
)

// Finding is a read whose name argument is built with path/filepath.
type Finding struct {
	Pos  token.Position
	Read string // the read, as "fs.ReadFile" or "<receiver type>.ReadFile"
	Name string // the filepath function that built the name
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s name built with filepath.%s", f.Pos.Filename, f.Pos.Line, f.Read, f.Name)
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
			for _, built := range filepathCalls(call.Args[arg], info) {
				found = append(found, Finding{Pos: fset.Position(built.Pos()), Read: read, Name: calleeOf(built, info).Name()})
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

// filepathCalls returns the calls into path/filepath within expr that can put
// the host's separator into its value. A separator-free call's argument is not
// searched: whatever built it, the call's result is a valid name.
func filepathCalls(expr ast.Expr, info *types.Info) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(expr, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calleeOf(call, info)
		if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "path/filepath" {
			return true
		}
		if slices.Contains(separatorFree, fn.Name()) {
			return false
		}
		calls = append(calls, call)
		return false
	})
	return calls
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
