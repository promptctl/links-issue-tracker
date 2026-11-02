package fsnames

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestModuleBuildsNoFSNameWithFilepath is the gate: every package of this
// module, and of the vendored driver module it links, is checked on every PR.
// Dependencies load from the build's export data, not from source.
//
// It scans the files that build for the host OS, test files included, so a
// _windows.go file is scanned by the nightly Windows lane: type-checking for a
// Windows target elsewhere needs Dolt's cgo dependencies cross-compiled.
func TestModuleBuildsNoFSNameWithFilepath(t *testing.T) {
	if raceEnabled {
		// The export data is built without -race, so under it this would
		// rebuild the whole dependency graph to repeat a check the plain
		// run already makes.
		t.Skip("the gate's verdict does not depend on -race")
	}
	t.Parallel()
	cfg := &packages.Config{
		Dir:   "../..",
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, "./...", "github.com/dolthub/driver/...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("packages failed to load; the gate cannot read their types")
	}
	var found []string
	files := 0
	for _, pkg := range pkgs {
		files += len(pkg.Syntax)
		for _, f := range Check(pkg.Fset, pkg.Syntax, pkg.TypesInfo) {
			found = append(found, f.String())
		}
	}
	if files == 0 {
		t.Fatal("scanned no files; the gate would pass vacuously")
	}
	// A package loads once alone and once with its tests, so a file can be
	// checked twice.
	slices.Sort(found)
	found = slices.Compact(found)
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
			want: []string{"embed.FS.ReadFile filepath.Join"},
		},
		{
			name: "io/fs functions take the name second",
			src: `package p
import ("io/fs"; fp "path/filepath")
func f(fsys fs.FS, d string) {
	fs.ReadFile(fsys, fp.Join(d, "x"))
	fs.WalkDir(fsys, fp.Clean(d), nil)
}`,
			want: []string{"fs.ReadFile filepath.Join", "fs.WalkDir filepath.Clean"},
		},
		{
			name: "filesystems however they were declared",
			src: `package p
import ("embed"; "io/fs"; "os"; "path/filepath")
var assets *embed.FS
var raw embed.FS
var sub2 = mustSub(raw, "d")
func mustSub(f fs.FS, d string) fs.FS { s, _ := fs.Sub(f, d); return s }
type holder struct{ embed.FS }
func f(d string, h holder) {
	root := os.DirFS(d)
	sub, _ := fs.Sub(root, "x")
	fs.Stat(sub, filepath.Dir(d))
	root.Open(filepath.Join(d, "a"))
	assets.ReadDir(filepath.Dir(d))
	sub2.Open(filepath.Join("x", d))
	h.ReadFile(filepath.Join("x", d))
	mustSub(raw, "d").Open(filepath.Join("x", d))
}`,
			want: []string{"fs.Stat filepath.Dir", "fs.FS.Open filepath.Join", "*embed.FS.ReadDir filepath.Dir", "fs.FS.Open filepath.Join", "p.holder.ReadFile filepath.Join", "fs.FS.Open filepath.Join"},
		},
		{
			name: "a filepath call wrapped in another call",
			src: `package p
import ("embed"; "path"; "path/filepath"; "strings")
var assets embed.FS
func f(d string) {
	assets.ReadFile(path.Clean(filepath.Join("a", d)))
	assets.ReadFile(strings.TrimPrefix(filepath.Join(d, "x"), "./"))
}`,
			want: []string{"embed.FS.ReadFile filepath.Join", "embed.FS.ReadFile filepath.Join"},
		},
		{
			name: "dot-imported io/fs, method expressions and Go 1.25 reads",
			src: `package p
import ("embed"; . "io/fs"; "path/filepath")
var assets embed.FS
func f(fsys FS, d string) {
	ReadFile(fsys, filepath.Join(d, "x"))
	ReadLink(fsys, filepath.Join(d, "x"))
	embed.FS.ReadFile(assets, filepath.Join("d", d))
}`,
			want: []string{"fs.ReadFile filepath.Join", "fs.ReadLink filepath.Join", "embed.FS.ReadFile filepath.Join"},
		},
		{
			name: "a method that takes a host path is not an io/fs read",
			src: `package p
import ("io/fs"; "path/filepath")
type disk struct{}
func (disk) Open(name string) (fs.File, error) { return nil, nil }
func (disk) Stat(host string, follow bool) (fs.FileInfo, error) { return nil, nil }
type ids struct{}
func (ids) Open(id int) (fs.File, error) { return nil, nil }
func (ids) ReadFile(name string) ([]byte, error) { return nil, nil }
func f(d string) {
	disk{}.Stat(filepath.Join(d, "x"), true)
	ids{}.ReadFile(filepath.Join(d, "x"))
}`,
		},
		{
			name: "a name built in a variable or with a separator constant",
			src: `package p
import ("embed"; "os"; "path/filepath"; "strings")
var assets embed.FS
func f(d string, parts []string) {
	p := filepath.Join("defaults", d)
	assets.ReadFile(p)
	assets.ReadFile(d + string(filepath.Separator) + "x")
	assets.ReadFile(strings.Join(parts, string(os.PathSeparator)))
	q := "a"
	q = filepath.Join(q, d)
	assets.ReadFile(q)
}`,
			want: []string{"embed.FS.ReadFile filepath.Join", "embed.FS.ReadFile filepath.Separator", "embed.FS.ReadFile os.PathSeparator"},
		},
		{
			name: "a local named fs is a value, not the package",
			src: `package p
import ("embed"; "io/fs"; "path/filepath")
var _ fs.FS
func load(fs embed.FS, n string) { fs.ReadFile(filepath.Join("d", n)) }`,
			want: []string{"embed.FS.ReadFile filepath.Join"},
		},
		{
			name: "correct builders, separator-free parts and host-path reads pass",
			src: `package p
import ("embed"; "os"; "path"; "path/filepath")
var assets embed.FS
func f(d string, r *os.Root) {
	assets.ReadFile(path.Join("defaults", d))
	assets.ReadFile(filepath.ToSlash(filepath.Join("defaults", d)))
	assets.ReadFile(path.Join("x", filepath.Base(d)))
	assets.ReadFile("x" + filepath.Ext(d))
	os.ReadFile(filepath.Join(d, "x"))
	os.Open(filepath.Join(d, "x"))
	r.Open(filepath.Join(d, "x"))
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
			conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
			if _, err := conf.Check("p", fset, []*ast.File{file}, info); err != nil {
				t.Fatalf("type-check: %v", err)
			}
			var got []string
			for _, f := range Check(fset, []*ast.File{file}, info) {
				got = append(got, f.Read+" "+f.Name)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("Check() = %q, want %q", got, tc.want)
			}
		})
	}
}
