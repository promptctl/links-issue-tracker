package fsnames

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
)

// TestShippedCodeBuildsNoFSNameWithFilepath is the gate: every shipped package,
// as docclaims walks the import graph from cmd/, is checked on every PR.
func TestShippedCodeBuildsNoFSNameWithFilepath(t *testing.T) {
	t.Parallel()
	fsys := os.DirFS(repoRoot(t))
	dirs, err := docclaims.ShippedPackages(fsys)
	if err != nil {
		t.Fatalf("ShippedPackages: %v", err)
	}
	var found []string
	files := 0
	for _, dir := range dirs {
		fset := token.NewFileSet()
		var parsed []*ast.File
		err := docclaims.EachProductFile(fsys, dir, func(name string, src []byte) error {
			file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
			parsed = append(parsed, file)
			return err
		})
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		files += len(parsed)
		for _, f := range Check(fset, parsed) {
			found = append(found, f.String())
		}
	}
	if files == 0 {
		t.Fatal("scanned no shipped files; the gate would pass vacuously")
	}
	if len(found) > 0 {
		t.Errorf("an io/fs name is slash-separated on every OS, so a name built with filepath fails every lookup on Windows; build it with path (or convert a host path with filepath.ToSlash):\n  %s", strings.Join(found, "\n  "))
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want []string // Finding.Read + " " + Finding.Name, in order
	}{
		{
			name: "embedded template lookup built with filepath.Join",
			src: `package p
import ("embed"; "path/filepath")
type store struct{ defaultsFS embed.FS }
func (s store) read(name string) ([]byte, error) {
	return s.defaultsFS.ReadFile(filepath.Join("defaults", name))
}`,
			want: []string{"defaultsFS.ReadFile Join"},
		},
		{
			name: "io/fs functions take the name second",
			src: `package p
import ("io/fs"; fp "path/filepath")
func f(fsys fs.FS, d string) {
	fs.ReadFile(fsys, fp.Join(d, "x"))
	fs.WalkDir(fsys, fp.Clean(d), nil)
}`,
			want: []string{"fs.ReadFile Join", "fs.WalkDir Clean"},
		},
		{
			name: "filesystems from os.DirFS, fs.Sub and pointer declarations",
			src: `package p
import ("embed"; "io/fs"; "os"; "path/filepath")
var assets *embed.FS
func f(d string) {
	root := os.DirFS(d)
	sub, _ := fs.Sub(root, "x")
	sub.Stat(filepath.Base(d))
	root.Open(filepath.Join(d, "a"))
	assets.ReadDir(filepath.Dir(d))
}`,
			want: []string{"sub.Stat Base", "root.Open Join", "assets.ReadDir Dir"},
		},
		{
			name: "correct builders and host-path reads pass",
			src: `package p
import ("embed"; "os"; "path"; "path/filepath")
var assets embed.FS
func f(d string) {
	assets.ReadFile(path.Join("defaults", d))
	assets.ReadFile(filepath.ToSlash(filepath.Join("defaults", d)))
	os.ReadFile(filepath.Join(d, "x"))
	os.Open(filepath.Join(d, "x"))
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got []string
			for _, f := range Check(fset, []*ast.File{file}) {
				got = append(got, f.Read+" "+f.Name)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("Check() = %q, want %q", got, tc.want)
			}
		})
	}
}

func repoRoot(t *testing.T) string {
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
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
