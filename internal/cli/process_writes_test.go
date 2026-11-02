package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// processWrites are the process-wide values a test in this package may not
// change: every test hands lit its directory, environment and streams through
// the Invocation (dirInvocation, setRunEnv), so the tests can run in parallel,
// and one test changing the process's would change them under every other.
// TestMain is the one place that sets them, once, before any test runs.
// [LAW:no-shared-mutable-globals] [LAW:single-enforcer]
var processWrites = map[string][]string{
	"os": {"Chdir", "Setenv", "Unsetenv", "Clearenv"},
}

// processStreams are the os values a test may not assign.
var processStreams = map[string][]string{
	"os": {"Stdin", "Stdout", "Stderr"},
}

// testingProcessWrites are the testing.T methods that change the process's
// directory or environment for the rest of the test.
var testingProcessWrites = []string{"Chdir", "Setenv"}

func TestNoTestChangesTheProcess(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var found []string
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		found = append(found, processWritesIn(fset, file)...)
	}
	if len(paths) == 0 {
		t.Fatal("found no test files to scan")
	}
	if len(found) > 0 {
		t.Fatalf("tests change the process's directory, environment or streams — hand them to the run instead (dirInvocation, setRunEnv):\n%s", strings.Join(found, "\n"))
	}
}

// processWritesIn is every change file makes to the process's directory,
// environment or streams outside TestMain, as "path:line: what".
func processWritesIn(fset *token.FileSet, file *ast.File) []string {
	var testMain *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "TestMain" {
			testMain = fn
		}
	}
	inTestMain := func(pos token.Pos) bool {
		return testMain != nil && testMain.Pos() <= pos && pos < testMain.End()
	}
	var found []string
	at := func(pos token.Pos, what string) {
		if inTestMain(pos) {
			return
		}
		p := fset.Position(pos)
		found = append(found, fmt.Sprintf("%s:%d: %s", p.Filename, p.Line, what))
	}
	// Both lists name only os, so one lookup finds its local name for both.
	local := importedAs(fset, file, processWrites, at)
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if pkg, ok := sel.X.(*ast.Ident); ok {
					if path, imported := local[pkg.Name]; imported && slices.Contains(processStreams[path], sel.Sel.Name) {
						at(sel.Pos(), path+"."+sel.Sel.Name+" assignment")
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if path, isPackage := local[recv.Name]; isPackage {
				if slices.Contains(processWrites[path], sel.Sel.Name) {
					at(sel.Pos(), path+"."+sel.Sel.Name)
				}
			} else if slices.Contains(testingProcessWrites, sel.Sel.Name) {
				at(sel.Pos(), recv.Name+"."+sel.Sel.Name)
			}
		}
		return true
	})
	return found
}

// The scan has to see every spelling of a change to the process, or the gate
// above passes on one it was blind to.
func TestProcessWritesScanFindsEverySpelling(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"os.Chdir", `import "os"; func f() { os.Chdir("d") }`, []string{"os.Chdir"}},
		{"os.Setenv", `import "os"; func f() { os.Setenv("A", "b"); os.Unsetenv("A"); os.Clearenv() }`, []string{"os.Setenv", "os.Unsetenv", "os.Clearenv"}},
		{"aliased os", `import sys "os"; func f() { sys.Setenv("A", "b") }`, []string{"os.Setenv"}},
		{"t.Setenv", `func f(t T) { t.Setenv("A", "b") }`, []string{"t.Setenv"}},
		{"t.Chdir", `func f(tb T) { tb.Chdir("d") }`, []string{"tb.Chdir"}},
		{"stream assignment", `import "os"; func f() { os.Stderr = nil; os.Stdin, os.Stdout = nil, nil }`, []string{"os.Stderr assignment", "os.Stdin assignment", "os.Stdout assignment"}},
		{"stream read", `import "os"; func f() { _ = os.Stderr }`, nil},
		{"inside TestMain", `import "os"; func TestMain(m M) { os.Setenv("A", "b"); os.Stdout = nil }`, nil},
		{"a method named TestMain", `import "os"; func (s S) TestMain() { os.Setenv("A", "b") }`, []string{"os.Setenv"}},
		{"dot import", `import . "os"; func f() { Setenv("A", "b") }`, []string{"dot import of os"}},
		{"setRunEnv", `func f(t T) { setRunEnv(t, "A", "b") }`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "probe.go", "package probe\n"+strings.ReplaceAll(tc.src, "; func", "\nfunc"), parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got []string
			for _, finding := range processWritesIn(fset, file) {
				got = append(got, finding[strings.LastIndex(finding, ": ")+2:])
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("scan = %q, want %q", got, want)
			}
		})
	}
}
