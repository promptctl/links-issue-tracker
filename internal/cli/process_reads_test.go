package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// processReads are the process-wide values no code Run can reach may read or
// write, by import path: Run is handed its own copies in the Invocation
// cmd/lit's main builds, and a second user would make two runs in one process
// see each other's. filepath.Abs is here because it reads os.Getwd for a
// relative path, os.ExpandEnv because it reads os.Getenv, os.TempDir and the
// user directories because they read the variables that name them, and fmt's
// and log's printers because they write os.Stdout and os.Stderr, inside the
// standard library where a scan of this module cannot see it.
// [LAW:effects-at-boundaries] [LAW:no-shared-mutable-globals]
var processReads = map[string][]string{
	"os":            {"Getwd", "Args", "Stdin", "Stdout", "Stderr", "Getenv", "LookupEnv", "Environ", "ExpandEnv", "UserHomeDir", "UserConfigDir", "UserCacheDir", "TempDir"},
	"path/filepath": {"Abs"},
	"syscall":       {"Getenv", "Environ"},
	"fmt":           {"Print", "Printf", "Println"},
	"log":           {"Print", "Printf", "Println", "Fatal", "Fatalf", "Fatalln", "Panic", "Panicf", "Panicln", "Writer", "Default"},
}

// childBuilders are the ways to build a child process. A child built with one
// inherits the process's environment unless its Env is set, so they are used
// only in environBuilderDir, by environ.Env's Command, CommandContext and Git,
// which set it to the run's: no other code may name one, build an exec.Cmd
// itself, or write a child's Env. [LAW:single-enforcer]
var childBuilders = map[string][]string{
	"os/exec": {"Command", "CommandContext"},
	"github.com/dolthub/dolt/go/libraries/utils/gitcmd": {"Command"},
}

const environBuilderDir = "internal/environ"

// TestNothingRunReachesReadsTheProcess scans every non-test file of every
// package of this module that cli.Run can reach — internal/cli and what it
// depends on, which excludes cmd/lit's main, the one place that reads the
// process.
func TestNothingRunReachesReadsTheProcess(t *testing.T) {
	t.Parallel()
	root := mustRepoRoot(t)
	fset := token.NewFileSet()
	var found []string
	files, builderFiles := 0, 0
	for _, dir := range moduleDepDirs(t, root, "./internal/cli") {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s) error = %v", dir, err)
		}
		builds := dir == filepath.Join(root, environBuilderDir)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			files++
			found = append(found, processReadsIn(fset, file)...)
			if builds {
				builderFiles++
				continue
			}
			found = append(found, childBuildsIn(fset, file)...)
		}
	}
	if files == 0 || builderFiles == 0 {
		t.Fatalf("scanned %d file(s), %d of them in %s; the gate would pass vacuously", files, builderFiles, environBuilderDir)
	}
	if len(found) > 0 {
		sort.Strings(found)
		t.Errorf("code cli.Run can reach reads or writes the process or builds a child outside %s; take the value from the Invocation, and build the child with its Env:\n  %s", environBuilderDir, strings.Join(found, "\n  "))
	}
}

// processReadsIn is every read of processReads in file, as "path:line: name".
func processReadsIn(fset *token.FileSet, file *ast.File) []string {
	return selectorsIn(fset, file, processReads)
}

// childBuildsIn is every use of a childBuilder in file, every exec.Cmd it
// builds without one (a literal, new(exec.Cmd) or a declared exec.Cmd value),
// and every write of a field named Env: an assignment, or an Env key in a
// literal of a type from a childBuilders package.
func childBuildsIn(fset *token.FileSet, file *ast.File) []string {
	found := selectorsIn(fset, file, childBuilders)
	local := importedAs(fset, file, childBuilders, func(token.Pos, string) {})
	at := func(pos token.Pos, what string) {
		p := fset.Position(pos)
		found = append(found, fmt.Sprintf("%s:%d: %s", p.Filename, p.Line, what))
	}
	execCmd := func(expr ast.Expr) bool {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && local[pkg.Name] == "os/exec" && sel.Sel.Name == "Cmd"
	}
	fromBuilderPackage := func(expr ast.Expr) bool {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return false
		}
		_, imported := local[pkg.Name]
		return imported
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if execCmd(n.Type) {
				at(n.Pos(), "os/exec.Cmd literal")
			}
			if fromBuilderPackage(n.Type) {
				for _, elt := range n.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Env" {
							at(kv.Pos(), "Env key")
						}
					}
				}
			}
		case *ast.CallExpr:
			if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "new" && len(n.Args) == 1 && execCmd(n.Args[0]) {
				at(n.Pos(), "new(os/exec.Cmd)")
			}
		case *ast.ValueSpec:
			if n.Type != nil && execCmd(n.Type) {
				at(n.Pos(), "os/exec.Cmd value")
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
					at(sel.Pos(), "Env assignment")
				}
			}
		}
		return true
	})
	return found
}

// selectorsIn is every use of a watched name in file, as "path:line: name".
// An import is matched by its path, not its local name, so an alias cannot
// hide a use, and a dot import of a watched package is itself a finding,
// since its uses would carry no qualifier to match.
func selectorsIn(fset *token.FileSet, file *ast.File, watched map[string][]string) []string {
	var found []string
	at := func(pos token.Pos, what string) {
		p := fset.Position(pos)
		found = append(found, fmt.Sprintf("%s:%d: %s", p.Filename, p.Line, what))
	}
	local := importedAs(fset, file, watched, at)
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if path, imported := local[pkg.Name]; imported && slices.Contains(watched[path], sel.Sel.Name) {
			at(sel.Pos(), path+"."+sel.Sel.Name)
		}
		return true
	})
	return found
}

// importedAs maps the local name of each watched package file imports to its
// path, reporting a dot import of one to at.
func importedAs(fset *token.FileSet, file *ast.File, watched map[string][]string, at func(token.Pos, string)) map[string]string {
	local := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if _, ok := watched[path]; !ok {
			continue
		}
		name := pathpkg.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			at(imp.Pos(), "dot import of "+path)
			continue
		}
		local[name] = path
	}
	return local
}

// The scans have to see every spelling of a read and of a child build, or the
// gate above passes on one it was blind to.
func TestProcessScansFindEverySpelling(t *testing.T) {
	t.Parallel()
	const gitcmdImport = `import "github.com/dolthub/dolt/go/libraries/utils/gitcmd"; `
	cases := []struct {
		name string
		scan func(*token.FileSet, *ast.File) []string
		src  string
		want []string
	}{
		{"plain call", processReadsIn, `import "os"; func f() { os.Getwd() }`, []string{"os.Getwd"}},
		{"value use", processReadsIn, `import "os"; var r = os.Stdin`, []string{"os.Stdin"}},
		{"arguments", processReadsIn, `import "os"; var a = os.Args[1:]`, []string{"os.Args"}},
		{"aliased import", processReadsIn, `import sys "os"; var r = sys.Stdin`, []string{"os.Stdin"}},
		{"filepath.Abs", processReadsIn, `import "path/filepath"; func f() { filepath.Abs("x") }`, []string{"path/filepath.Abs"}},
		{"dot import", processReadsIn, `import . "os"; var r = Stdin`, []string{"dot import of os"}},
		{"environment reads", processReadsIn, `import "os"; func f() { os.Getenv("A"); os.LookupEnv("A"); _ = os.Environ(); os.ExpandEnv("$A") }`, []string{"os.Getenv", "os.LookupEnv", "os.Environ", "os.ExpandEnv"}},
		{"syscall environment reads", processReadsIn, `import "syscall"; func f() { syscall.Getenv("A"); _ = syscall.Environ() }`, []string{"syscall.Getenv", "syscall.Environ"}},
		{"directories the environment names", processReadsIn, `import "os"; func f() { os.UserHomeDir(); os.UserConfigDir(); os.UserCacheDir(); os.TempDir() }`, []string{"os.UserHomeDir", "os.UserConfigDir", "os.UserCacheDir", "os.TempDir"}},
		{"standard streams", processReadsIn, `import "os"; var w = os.Stdout; func f() { fmt.Fprintln(os.Stderr) }`, []string{"os.Stdout", "os.Stderr"}},
		{"printers that write the standard streams", processReadsIn, `import ("fmt"; "log"); func f() { fmt.Println(); fmt.Printf(""); log.Printf(""); _ = log.Default() }`, []string{"fmt.Println", "fmt.Printf", "log.Printf", "log.Default"}},
		{"printers handed a writer", processReadsIn, `import "fmt"; func f(w W) { fmt.Fprintln(w); _ = fmt.Sprintf("") }`, nil},
		{"other os names", processReadsIn, `import "os"; func f() { os.Getpid() }`, nil},
		{"same name, other package", processReadsIn, `import os "example.com/notos"; var r = os.Stdin`, nil},
		{"exec constructors", childBuildsIn, `import "os/exec"; func f() { exec.Command("x"); exec.CommandContext(nil, "x") }`, []string{"os/exec.Command", "os/exec.CommandContext"}},
		{"aliased exec", childBuildsIn, `import run "os/exec"; func f() { run.Command("x") }`, []string{"os/exec.Command"}},
		{"gitcmd constructor", childBuildsIn, gitcmdImport + `func f() { gitcmd.Command(nil, gitcmd.Spec{Dir: "/d"}) }`, []string{"github.com/dolthub/dolt/go/libraries/utils/gitcmd.Command"}},
		{"exec.Cmd built by hand", childBuildsIn, `import "os/exec"; var a = exec.Cmd{Path: "x"}; var b = &exec.Cmd{}; var c = new(exec.Cmd); var d exec.Cmd`, []string{"os/exec.Cmd literal", "os/exec.Cmd literal", "new(os/exec.Cmd)", "os/exec.Cmd value"}},
		{"Env written", childBuildsIn, gitcmdImport + `func f(c *T) { c.Env = nil; _ = gitcmd.Spec{Env: nil} }`, []string{"Env assignment", "Env key"}},
		{"a child built through the run's Env", childBuildsIn, `import "os/exec"; func f(env E) *exec.Cmd { return env.Command("x") }`, nil},
		{"Env key of a type from elsewhere", childBuildsIn, `var i = Invocation{Env: e}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "probe.go", "package probe; "+tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got []string
			for _, finding := range tc.scan(fset, file) {
				got = append(got, finding[strings.LastIndex(finding, ": ")+2:])
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("scan = %q, want %q", got, tc.want)
			}
		})
	}
}
