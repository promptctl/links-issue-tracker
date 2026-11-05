package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestSyncFailureClassesIsTheConstBlock ties syncFailureClasses to the
// constants it claims to enumerate: every syncFailureClass constant declared
// in sync_failure.go is in the slice and nothing else is. The tests that
// partition the closed set range over the slice, so without this tie a class
// added to the constants alone would pass them all; with it, the addition
// fails here first. The source is read because Go cannot enumerate the
// constants of a type at run time.
func TestSyncFailureClassesIsTheConstBlock(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "sync_failure.go", nil, 0)
	if err != nil {
		t.Fatalf("parse sync_failure.go: %v", err)
	}
	declared := map[syncFailureClass]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typeName, ok := value.Type.(*ast.Ident)
			if !ok || typeName.Name != "syncFailureClass" {
				continue
			}
			for i, name := range value.Names {
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is a syncFailureClass constant without a string literal value", name.Name)
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", name.Name, lit.Value, err)
				}
				declared[syncFailureClass(text)] = name.Name
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no syncFailureClass constants found in sync_failure.go; the parse looked in the wrong place")
	}
	listed := map[syncFailureClass]bool{}
	for _, class := range syncFailureClasses {
		if listed[class] {
			t.Fatalf("syncFailureClasses lists %q twice", class)
		}
		listed[class] = true
		if _, ok := declared[class]; !ok {
			t.Fatalf("syncFailureClasses lists %q, which no syncFailureClass constant declares", class)
		}
	}
	for class, name := range declared {
		if !listed[class] {
			t.Fatalf("%s (%q) is declared but missing from syncFailureClasses; add it there so the partition tests see it", name, class)
		}
	}
}
