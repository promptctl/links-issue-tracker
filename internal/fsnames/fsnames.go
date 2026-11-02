// Package fsnames finds io/fs names built with package path/filepath.
//
// An io/fs name is always slash-separated, on every OS. filepath builds names
// with the host's separator, so a name it builds works on Linux and macOS and
// fails every lookup on Windows, and per-PR CI runs only on Linux. path is the
// builder for an io/fs name; filepath.ToSlash converts a host path into one.
//
// The check reads syntax, not types: type-checking every shipped package from
// source would load Dolt's whole cgo tree on every PR. A value counts as a
// filesystem when it is declared with a filesystem type (embed.FS, or io/fs's
// FS and its extension interfaces) or assigned from os.DirFS or fs.Sub, and
// names are matched package-wide by identifier, as the check cannot tell two
// declarations of one name apart.
//
// WHAT IT DOES NOT COVER: a filesystem reached through a name declared some
// other way (a := from any other call, an interface method's result), and a
// name built with filepath and stored in a variable before the read.
package fsnames

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"strconv"
	"strings"
)

// Finding is a read whose name argument is a call into path/filepath.
type Finding struct {
	Pos  token.Position
	Read string // the read, as "fs.ReadFile" or "<receiver>.ReadFile"
	Name string // the filepath function that built the name
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s name built with filepath.%s", f.Pos.Filename, f.Pos.Line, f.Read, f.Name)
}

// fsTypes are the filesystem types a declaration can name, by import path.
var fsTypes = map[string][]string{
	"embed": {"FS"},
	"io/fs": {"FS", "ReadFileFS", "ReadDirFS", "StatFS", "SubFS", "GlobFS"},
}

// fsBuilders are the calls whose result is a filesystem, by import path.
var fsBuilders = map[string][]string{
	"os":    {"DirFS"},
	"io/fs": {"Sub"},
}

// fsFuncs are io/fs's functions that take a filesystem and then a name.
var fsFuncs = []string{"ReadFile", "ReadDir", "Stat", "Sub", "Glob", "WalkDir"}

// fsMethods are the filesystem methods whose first argument is a name.
var fsMethods = []string{"Open", "ReadFile", "ReadDir", "Stat", "Sub", "Glob"}

// Check returns every io/fs read in files, the files of one package, whose name
// argument is a call into path/filepath other than ToSlash.
func Check(fset *token.FileSet, files []*ast.File) []Finding {
	filesystems := map[string]bool{}
	for _, file := range files {
		collectFilesystems(file, imports(file), filesystems)
	}
	var found []Finding
	for _, file := range files {
		local := imports(file)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			read, arg := "", -1
			if pkg, ok := sel.X.(*ast.Ident); ok && local[pkg.Name] == "io/fs" {
				if slices.Contains(fsFuncs, sel.Sel.Name) {
					read, arg = "fs."+sel.Sel.Name, 1
				}
			} else if name := lastName(sel.X); filesystems[name] && slices.Contains(fsMethods, sel.Sel.Name) {
				read, arg = name+"."+sel.Sel.Name, 0
			}
			if arg < 0 || arg >= len(call.Args) {
				return true
			}
			if built, ok := filepathCall(call.Args[arg], local); ok {
				found = append(found, Finding{Pos: fset.Position(call.Args[arg].Pos()), Read: read, Name: built})
			}
			return true
		})
	}
	return found
}

// collectFilesystems adds to into every name file declares as a filesystem.
func collectFilesystems(file *ast.File, local map[string]string, into map[string]bool) {
	isFS := func(expr ast.Expr) bool {
		if star, ok := expr.(*ast.StarExpr); ok {
			expr = star.X
		}
		return qualified(expr, local, fsTypes)
	}
	built := func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		return ok && qualified(call.Fun, local, fsBuilders)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Field:
			if isFS(n.Type) {
				for _, name := range n.Names {
					into[name.Name] = true
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if (n.Type != nil && isFS(n.Type)) || (i < len(n.Values) && built(n.Values[i])) {
					into[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			// fs.Sub returns (fs.FS, error), so its filesystem is the first name.
			if len(n.Rhs) == 1 && built(n.Rhs[0]) {
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					into[id.Name] = true
				}
			}
		}
		return true
	})
}

// filepathCall reports the filepath function expr calls, unless it is ToSlash.
func filepathCall(expr ast.Expr, local map[string]string) (string, bool) {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || local[pkg.Name] != "path/filepath" || sel.Sel.Name == "ToSlash" {
		return "", false
	}
	return sel.Sel.Name, true
}

// qualified reports whether expr is pkg.Name for a name watched lists under
// pkg's import path.
func qualified(expr ast.Expr, local map[string]string, watched map[string][]string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && slices.Contains(watched[local[pkg.Name]], sel.Sel.Name)
}

// lastName is the identifier a receiver ends in: x for x, f for s.f.
func lastName(expr ast.Expr) string {
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// imports maps each local package name in file to its import path.
func imports(file *ast.File) map[string]string {
	local := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		local[name] = path
	}
	return local
}

