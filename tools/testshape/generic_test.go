package main

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The reading's view of the build disagreeing with the build is a bug in
// the tool, and refuses the run. The build never produces one, so each case
// builds the disagreement: an outside package, example.com/out, whose source
// is src and whose export data declares nothing.

const outPath = "example.com/out"

func analyzerOverSource(t *testing.T, src string) *analyzer {
	t.Helper()
	l, err := shapeModule()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ld := &loader{
		fset:     token.NewFileSet(),
		byID:     map[string]*listedPackage{outPath: {ImportPath: outPath, Name: "out", Dir: dir, CompiledGoFiles: []string{"out.go"}}},
		exported: map[string]*types.Package{outPath: types.NewPackage(outPath, "out")},
		checked:  map[string]*types.Package{},
		sizes:    types.SizesFor("gc", "arm64"),
	}
	a := newAnalyzer(l, shapeClassification(t))
	a.generics = newGenericSource(ld)
	return a
}

func outInstance(name string) instance {
	return instance{funcName{outPath, name}, []types.Type{types.Typ[types.Int]}}
}

func TestASourceCopyThatDoesNotTypeCheckRefusesTheRun(t *testing.T) {
	a := analyzerOverSource(t, "package out\n\nfunc F[T any](v T) any { return missing }\n")
	a.decide(outInstance("F"))
	refusals := analysisRefusals(a)
	prefix := "testshape is inconsistent, a bug in the tool: reading example.com/out.F[int]: the source the build compiles for example.com/out does not type-check: "
	if len(refusals) != 1 || !strings.HasPrefix(refusals[0], prefix) || !strings.HasSuffix(refusals[0], "undefined: missing") {
		t.Errorf("refusals = %q, want one: %s...undefined: missing", refusals, prefix)
	}
}

func TestABindTheExportDataCannotHoldRefusesTheRun(t *testing.T) {
	a := analyzerOverSource(t, "package out\n\ntype W[T any] struct{ v T }\n\nfunc F[T any](v T) any { return W[T]{v} }\n")
	a.decide(outInstance("F"))
	want := []string{"testshape is inconsistent, a bug in the tool: reading example.com/out.F[int]: its source declares type example.com/out.W, which the build's export data does not"}
	if got := analysisRefusals(a); !slices.Equal(got, want) {
		t.Errorf("refusals = %q, want %q", got, want)
	}
}

// Two types of one name and package, each declared in its own function
// body, are two type arguments: their instances are two, within the
// program and across the suite's modules, which gather by the same key.
func TestTwoFunctionLocalTypesOfOneNameAreTwoInstances(t *testing.T) {
	l, err := shapeModule()
	if err != nil {
		t.Fatal(err)
	}
	a := newAnalyzer(l, shapeClassification(t))
	a.measure(l.Binaries["example.com/shape/genericlocals"])
	if refusals := analysisRefusals(a); len(refusals) > 0 {
		t.Fatalf("refused:\n%s", strings.Join(refusals, "\n"))
	}
	g := newSuiteGenerics()
	g.add(a, l.Module)
	g.add(a, l.Module) // a second module's analysis reading the same instances
	var got []string
	for _, u := range g.unread {
		got = append(got, u.text(namer{l.Module}))
	}
	want := []string{
		"nested/gen.Address[genericlocals.file]: it converts *T to unsafe.Pointer",
		"nested/gen.Address[genericlocals.file]: it converts *T to unsafe.Pointer",
	}
	if !slices.Equal(got, want) {
		t.Errorf("unread = %q, want %q", got, want)
	}
	if g.unread[0].key == g.unread[1].key {
		t.Errorf("both instances have key %v", g.unread[0].key)
	}
}

// A constraint method its type argument lacks cannot come from a body that
// compiled, so the reading is put in place by hand.
func TestAConstraintMethodItsTypeArgumentLacksRefusesTheRun(t *testing.T) {
	a := analyzerOverSource(t, "package out\n")
	in := outInstance("G")
	key := a.instanceKey(in)
	a.instances[key] = &genericInstance{in: in, key: key, reading: uses{
		names: []string{"T"},
		each:  []bodyUse{{constraintCall{types.Typ[types.Int], nil, "Close"}, []int{0}}},
	}}
	newWalker(a, emptyBinary()).typeArguments(in, nil)
	want := []string{"testshape is inconsistent, a bug in the tool: example.com/out.G[int]: its type argument int has no method Close, which its constraint declares"}
	if got := analysisRefusals(a); !slices.Equal(got, want) {
		t.Errorf("refusals = %q, want %q", got, want)
	}
}
