package actionnames

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"
)

// The probe plants each shape the rule names in a package that imports
// model, and the check must flag exactly the ones it forbids.
const probe = `package probe

import (
	"fmt"
	f "fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

type event struct{ Action model.ActionName }

func name() model.ActionName { return "" }

type log struct{}

func (log) Logf(format string, args ...any) { _ = fmt.Sprintf(format, args...) }

func fail(code int, format string, args ...any) error {
	return log{}.wrap(format, args...)
}

func (log) wrap(format string, args ...any) error { return fmt.Errorf(format, args...) }

func plain(prefix string, args ...any) string { return fmt.Sprint(args...) }

func sites(e event, n model.ActionName, ns []model.ActionName, byName map[string]model.ActionName) {
	local := n
	_ = fmt.Sprintf("%s", e.Action)
	_ = fmt.Sprintf("%s %s", n.Verb(), n)
	_ = fmt.Errorf("cannot :) %s", local)
	_ = fmt.Sprint(name())
	_ = fmt.Sprintf("%v", any(e.Action))
	_ = f.Sprintf("%s",
		n)
	_ = fmt.Sprintf("%v", ns)
	_ = fmt.Sprintf("%v", byName)
	_ = fmt.Sprintf("%v", e)
	_ = fmt.Appendf(nil, "%s", n)
	log{}.Logf("%s", n)
	_ = fail(1, "%s", n)
	_ = fmt.Sprintf("%s", n.Verb())
	_ = fmt.Sprintf("%s", string(e.Action))
	_ = fmt.Sprintf("%s", "unrelated")
	_ = plain("not printf-shaped", n)
	_ = fmt.Sprintf("%d %v", len(ns), e == event{})
}
`

func TestCheckFlagsEveryForbiddenShapeAndNoAllowedOne(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", probe, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("example.com/probe", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	files := []*ast.File{file}
	wrappers := Wrappers([]Package{{Files: files, Info: info}})
	wantWrappers := []string{"(example.com/probe.log).Logf", "(example.com/probe.log).wrap", "example.com/probe.fail"}
	if got := slices.Sorted(maps.Keys(wrappers)); !slices.Equal(got, wantWrappers) {
		t.Errorf("Wrappers = %q, want %q", got, wantWrappers)
	}
	found, allowed := Check(fset, files, info, wrappers)
	var got []string
	for _, f := range found {
		got = append(got, f.String())
	}
	want := []string{
		"probe.go:28:24: argument to fmt.Sprintf",                  // a field
		"probe.go:29:37: argument to fmt.Sprintf",                  // beside an allowed Verb()
		"probe.go:30:33: argument to fmt.Errorf",                   // a := local, after a ) in the format
		"probe.go:31:17: argument to fmt.Sprint",                   // a call's result
		"probe.go:32:28: argument to fmt.Sprintf",                  // wrapped in any
		"probe.go:34:3: argument to fmt.Sprintf",                   // an aliased fmt, the call wrapped
		"probe.go:35:24: argument to fmt.Sprintf",                  // a slice of them
		"probe.go:36:24: argument to fmt.Sprintf",                  // a map holding them
		"probe.go:37:24: argument to fmt.Sprintf",                  // a struct holding one
		"probe.go:38:29: argument to fmt.Appendf",                  // fmt's Append family
		"probe.go:39:19: argument to (example.com/probe.log).Logf", // a method wrapper
		"probe.go:40:20: argument to example.com/probe.fail",       // a wrapper of a wrapper
	}
	if !slices.Equal(got, want) {
		t.Errorf("Check found\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if allowed != 3 {
		t.Errorf("allowed = %d, want 3 (n.Verb() twice, string(e.Action) once)", allowed)
	}
}
