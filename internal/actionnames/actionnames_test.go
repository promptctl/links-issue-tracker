package actionnames

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
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

func sites(e event, n model.ActionName) {
	local := n
	_ = fmt.Sprintf("%s", e.Action)
	_ = fmt.Sprintf("%s %s", n.Verb(), n)
	_ = fmt.Errorf("cannot :) %s", local)
	_ = fmt.Sprint(name())
	_ = fmt.Sprintf("%v", any(e.Action))
	_ = f.Sprintf("%s",
		n)
	_ = fmt.Sprintf("%s", n.Verb())
	_ = fmt.Sprintf("%s", string(e.Action))
	_ = fmt.Sprintf("%s", "unrelated")
}
`

func TestCheckFlagsEveryForbiddenShapeAndNoAllowedOne(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", probe, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("example.com/probe", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	found, allowed := Check(fset, []*ast.File{file}, info)
	var got []string
	for _, f := range found {
		got = append(got, f.String())
	}
	want := []string{
		"probe.go:16:24: argument to fmt.Sprintf", // a field
		"probe.go:17:37: argument to fmt.Sprintf", // beside an allowed Verb()
		"probe.go:18:33: argument to fmt.Errorf",  // a := local, after a ) in the format
		"probe.go:19:17: argument to fmt.Sprint",  // a call's result
		"probe.go:20:28: argument to fmt.Sprintf", // wrapped in any
		"probe.go:22:3: argument to fmt.Sprintf",  // an aliased fmt, the call wrapped
	}
	if !slices.Equal(got, want) {
		t.Errorf("Check found\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if allowed != 3 {
		t.Errorf("allowed = %d, want 3 (n.Verb() twice, string(e.Action) once)", allowed)
	}
}
