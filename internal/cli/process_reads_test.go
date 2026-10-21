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

// processReads are the process-wide values no code Run can reach may read,
// by import path: Run is handed its own copies in the Invocation cmd/lit's
// main builds, and a second reader would make two runs in one process see each
// other's. filepath.Abs is here because it reads os.Getwd for a relative path,
// inside the standard library where a scan of this module cannot see it.
// [LAW:effects-at-boundaries] [LAW:no-shared-mutable-globals]
var processReads = map[string][]string{
	"os":            {"Getwd", "Args", "Stdin"},
	"path/filepath": {"Abs"},
}

// TestNothingRunReachesReadsTheProcess scans every non-test file of every
// package of this module that cli.Run can reach — internal/cli and what it
// depends on, which excludes cmd/lit's main, the one place that reads the
// process.
func TestNothingRunReachesReadsTheProcess(t *testing.T) {
	t.Parallel()
	root := mustRepoRoot(t)
	fset := token.NewFileSet()
	var found []string
	files := 0
	for _, dir := range moduleDepDirs(t, root, "./internal/cli") {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s) error = %v", dir, err)
		}
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
		}
	}
	if files == 0 {
		t.Fatal("scanned no files; the gate would pass vacuously")
	}
	if len(found) > 0 {
		sort.Strings(found)
		t.Errorf("code cli.Run can reach reads the process; take the value from the Invocation instead:\n  %s", strings.Join(found, "\n  "))
	}
}

// processReadsIn is every read of processReads in file, as "path:line: name".
// An import is matched by its path, not its local name, so an alias cannot hide
// a read, and a dot import of a watched package is itself a finding, since its
// reads would carry no qualifier to match.
func processReadsIn(fset *token.FileSet, file *ast.File) []string {
	var found []string
	at := func(pos token.Pos, what string) {
		p := fset.Position(pos)
		found = append(found, fmt.Sprintf("%s:%d: %s", p.Filename, p.Line, what))
	}
	local := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if _, watched := processReads[path]; !watched {
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
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if path, imported := local[pkg.Name]; imported && slices.Contains(processReads[path], sel.Sel.Name) {
			at(sel.Pos(), path+"."+sel.Sel.Name)
		}
		return true
	})
	return found
}

// The scan has to see every spelling of a read, or the gate above passes on a
// read it was blind to.
func TestProcessReadsInFindsEverySpelling(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"plain call", `import "os"; func f() { os.Getwd() }`, []string{"os.Getwd"}},
		{"value use", `import "os"; var r = os.Stdin`, []string{"os.Stdin"}},
		{"arguments", `import "os"; var a = os.Args[1:]`, []string{"os.Args"}},
		{"aliased import", `import sys "os"; var r = sys.Stdin`, []string{"os.Stdin"}},
		{"filepath.Abs", `import "path/filepath"; func f() { filepath.Abs("x") }`, []string{"path/filepath.Abs"}},
		{"dot import", `import . "os"; var r = Stdin`, []string{"dot import of os"}},
		{"other os names", `import "os"; var w = os.Stdout; func f() { os.Getpid() }`, nil},
		{"same name, other package", `import os "example.com/notos"; var r = os.Stdin`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "probe.go", "package probe; "+tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got []string
			for _, finding := range processReadsIn(fset, file) {
				got = append(got, finding[strings.LastIndex(finding, ": ")+2:])
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("processReadsIn = %q, want %q", got, tc.want)
			}
		})
	}
}
