package workflows

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Event names are the stable contract user definitions bind to: snake_case
// semantic names, unique, and each reads back from a when: entry as itself.
func TestCatalogNamesAreStableContractShaped(t *testing.T) {
	namePattern := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	seen := map[Event]bool{}
	for _, event := range Catalog() {
		if !namePattern.MatchString(string(event)) {
			t.Errorf("event %q is not snake_case", event)
		}
		if seen[event] {
			t.Errorf("event %q appears twice in the catalog", event)
		}
		seen[event] = true
		// A when: entry naming the event reads back as the event, never as a
		// crossing of some state.
		if moment, err := ParseMoment(string(event)); err != nil || moment != event {
			t.Errorf("ParseMoment(%q) = %#v, %v; want the event", event, moment, err)
		}
	}
	if len(seen) == 0 {
		t.Fatal("Catalog() is empty")
	}
}

// Every event the package declares is in the catalog, so a when: entry can
// name it. Catalog is a list kept beside the constants, and an event a command
// fires but the list leaves out is one ParseMoment refuses, so no definition
// could bind it. Go cannot enumerate a type's constants, so the test reads
// them from the package's source.
func TestCatalogListsEveryDeclaredEvent(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	fset := token.NewFileSet()
	var declared []Event
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for _, value := range spec.Values {
				if text, ok := eventLiteral(spec.Type, value); ok {
					declared = append(declared, Event(text))
				}
			}
			return false
		})
	}
	if len(declared) == 0 {
		t.Fatal("found no Event declarations; the test no longer reads where events are declared")
	}
	for _, event := range declared {
		if !slices.Contains(Catalog(), event) {
			t.Errorf("event %q is declared but not in Catalog(), so no when: entry can name it", event)
		}
	}
}

// eventLiteral is the text of an Event a declaration spells: a value of a spec
// typed Event, or a conversion Event("...").
func eventLiteral(typ ast.Expr, value ast.Expr) (string, bool) {
	if call, ok := value.(*ast.CallExpr); ok && isEventIdent(call.Fun) && len(call.Args) == 1 {
		value = call.Args[0]
	} else if !isEventIdent(typ) {
		return "", false
	}
	lit, ok := value.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(lit.Value)
	return text, err == nil
}

func isEventIdent(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "Event"
}
