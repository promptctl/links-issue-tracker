package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// The fixture module under testdata/shape holds one package per case of the
// reach rules. Each expectation is what the rules say of the case, written
// from the rules, never from the tool's output.

const shapeDir = "testdata/shape"

// shapeModule is the fixture module, loaded once for every case, since the
// load builds export data for its imports, the costly step. Each case
// measures with its own analyzer; the program they share builds method
// wrappers on demand under its own lock, and no case runs in parallel. It
// loads under the fixture's build tag, which is how -tags passing through is
// proven.
var shapeModule = sync.OnceValues(func() (*loaded, error) {
	return loadModule(module{Dir: shapeDir, Path: "example.com/shape"}, []string{"-tags=shapetag"})
})

func shapeClassification(t *testing.T) *Classification {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(shapeDir, "classification.json"))
	if err != nil {
		t.Fatal(err)
	}
	class, err := parseClassification(data)
	if err != nil {
		t.Fatal(err)
	}
	return class
}

// measureFixture measures the test binary of one fixture package, returning
// what it reached and every refusal the analysis made.
func measureFixture(t *testing.T, pkg string) (binaryShape, []string) {
	t.Helper()
	l, err := shapeModule()
	if err != nil {
		t.Fatal(err)
	}
	b := l.Binaries["example.com/shape/"+pkg]
	if b == nil {
		t.Fatalf("the fixture module has no test binary for %s", pkg)
	}
	a := newAnalyzer(l, shapeClassification(t))
	shape := a.measure(b)
	return shape, analysisRefusals(a)
}

// reachOf is what one fixture test reaches, the analysis refusing nothing.
func reachOf(t *testing.T, pkg, test string) reached {
	t.Helper()
	return testShapeOf(t, pkg, test).Reached
}

// testShapeOf is one fixture test's shape, the analysis refusing nothing.
func testShapeOf(t *testing.T, pkg, test string) testShape {
	t.Helper()
	shape, refusals := measureFixture(t, pkg)
	if len(refusals) > 0 {
		t.Fatalf("measuring %s refused:\n%s", pkg, strings.Join(refusals, "\n"))
	}
	var names []string
	for _, ts := range shape.Tests {
		if ts.Name == test {
			return ts
		}
		names = append(names, ts.Name)
	}
	t.Fatalf("%s declares no %s; it declares %v", pkg, test, names)
	return testShape{}
}

// expectReach checks a test's layer and touches, and, for each touch given a
// witness, that witness exactly.
func expectReach(t *testing.T, r reached, layer Layer, touches string, witnesses map[Touch]string) {
	t.Helper()
	got := "none"
	if r.set() != 0 {
		got = r.set().String()
	}
	if l := layerOf(r.set()); l != layer || got != touches {
		t.Fatalf("layer %s touching %s, want %s touching %s; witnesses:\n%s", l, got, layer, touches, describeReach(r))
	}
	for touch, want := range witnesses {
		if w := r[touch].String(); w != want {
			t.Errorf("%s witness:\n got %s\nwant %s", touch, w, want)
		}
	}
}

func describeReach(r reached) string {
	var lines []string
	for touch := range r.set().Each {
		lines = append(lines, touch.String()+": "+r[touch].String())
	}
	return strings.Join(lines, "\n")
}

func TestATouchTwoCallsDeepIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "twodeep", "TestLoad"), Integration, "fs", map[Touch]string{
		FS: "twodeep.TestLoad → twodeep.Load → twodeep.read → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAnInMemoryImplementationIsUnitBesideATouchingConstructor(t *testing.T) {
	expectReach(t, reachOf(t, "inmemory", "TestMemory"), Unit, "none", nil)
	expectReach(t, reachOf(t, "inmemory", "TestDisk"), Integration, "fs", nil)
}

func TestAModeTableReachesEveryOpenerOfItsSignature(t *testing.T) {
	// The test opens the memory mode, but the table is read by a dynamic
	// call, which reaches every binary-level opener of its signature.
	expectReach(t, reachOf(t, "modetable", "TestMemoryMode"), Integration, "fs", map[Touch]string{
		FS: "modetable.TestMemoryMode → modetable.Open → modetable.openDisk [dynamic call reaches value from init of modetable, made in modetable.init] → os.MkdirAll ⇒ os.MkdirAll (fs)",
	})
}

func TestWhatTestMainReachesInOneBranchEveryTestReaches(t *testing.T) {
	// TestMain runs the CLI only when re-run as it, but its reach is the
	// setup of every test.
	expectReach(t, reachOf(t, "climain", "TestThroughThePort"), Integration, "fs", map[Touch]string{
		FS: "climain.TestMain → climain.Run → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAnOutsideDispatcherReachesWhatInitRegistered(t *testing.T) {
	expectReach(t, reachOf(t, "initdispatch", "TestServe"), Integration, "fs", map[Touch]string{
		FS: "initdispatch.TestServe → initdispatch.Serve → initdispatch.config [dispatch reaches value from init of initdispatch, made in initdispatch.init] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAFunctionValueTestMainStoresIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "testmainvalue", "TestLoad"), Integration, "fs", map[Touch]string{
		FS: "testmainvalue.TestLoad → testmainvalue.ReadConfig [dynamic call reaches value from TestMain, made in testmainvalue.TestMain] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAModuleTypeInitConvertsHasEveryMethodInTheSetup(t *testing.T) {
	expectReach(t, reachOf(t, "initmap", "TestOpen"), Integration, "fs", map[Touch]string{
		FS: "initmap.init → initmap.init#1 → (initmap.disk).Open [converts initmap.disk to an interface] → os.MkdirAll ⇒ os.MkdirAll (fs)",
	})
}

func TestAValueInitBuildsIsReachedThroughAStructField(t *testing.T) {
	expectReach(t, reachOf(t, "initfield", "TestPrepare"), Integration, "fs", map[Touch]string{
		FS: "initfield.TestPrepare → initfield.Prepare → initfield.mkdirAll [dynamic call reaches value from init of initfield, made in initfield.init] → os.MkdirAll ⇒ os.MkdirAll (fs)",
	})
}

func TestAModuleTypeInitConvertsThroughAPointerHasEveryMethodInTheSetup(t *testing.T) {
	expectReach(t, reachOf(t, "initpointer", "TestOpen"), Integration, "fs", map[Touch]string{
		FS: "initpointer.init → initpointer.init#1 → (initpointer.disk).Open [converts initpointer.disk to an interface] → os.MkdirAll ⇒ os.MkdirAll (fs)",
	})
}

func TestACompileTimeAssertionMakesNothing(t *testing.T) {
	expectReach(t, reachOf(t, "varblank", "TestOpenNone"), Unit, "none", nil)
}

func TestACallbackHandedToTRunIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "trun", "TestSubtest"), Integration, "fs", map[Touch]string{
		FS: "trun.TestSubtest → (*testing.T).Run → trun.TestSubtest$1 [call into testing.T.Run reaches value from TestSubtest, made in trun.TestSubtest] → trun.Write → os.WriteFile ⇒ os.WriteFile (fs)",
	})
}

func TestGitThroughTwoHelpersIsGit(t *testing.T) {
	expectReach(t, reachOf(t, "gitwrap", "TestStatus"), Integration, "git", map[Touch]string{
		Git: `gitwrap.TestStatus → gitwrap.Status → gitwrap.git → gitwrap.run ⇒ os/exec.Command spawns "git", which is git`,
	})
}

func TestTheTestBinaryRerunUnderACLITestMainIsEndToEnd(t *testing.T) {
	expectReach(t, reachOf(t, "selfcli", "TestRerun"), EndToEnd, "cli,fs", map[Touch]string{
		CLI: "selfcli.TestRerun ⇒ os/exec.Command spawns os.Args[0], the test binary, whose setup runs the CLI",
		FS:  "selfcli.TestMain → selfcli.Run → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestTheTestBinaryRerunUnderAPlainTestMainIsAnotherProcess(t *testing.T) {
	expectReach(t, reachOf(t, "selfplain", "TestRerun"), Integration, "proc", map[Touch]string{
		Proc: "selfplain.TestRerun ⇒ os/exec.Command spawns os.Args[0], the test binary, whose setup does not reach the CLI: another process",
	})
}

func TestAMainPackageRunsItselfAndGitThroughOneHelper(t *testing.T) {
	// TestMain runs main when asked, so main's reach is every test's.
	expectReach(t, reachOf(t, "cmdshape", "TestRunsItself"), EndToEnd, "cli,fs", map[Touch]string{
		CLI: "cmdshape.TestRunsItself → cmdshape.runProgram ⇒ os/exec.Command spawns os.Executable(), the test binary, whose setup runs the CLI",
	})
	expectReach(t, reachOf(t, "cmdshape", "TestRunsGit"), Integration, "git,fs", map[Touch]string{
		Git: `cmdshape.TestRunsGit → cmdshape.runProgram ⇒ os/exec.Command spawns "git", which is git`,
		FS:  "cmdshape.TestMain → cmdshape.main → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAnUnresolvedProgramWithNoCLIToBeIsAnotherProcess(t *testing.T) {
	// The setup does not run the CLI and nothing in reach builds one, so the
	// program, whatever it is, cannot be a CLI.
	r := reachOf(t, "unresolved", "TestRunTool")
	expectReach(t, r, Integration, "proc", nil)
	if w := r[Proc].String(); !strings.Contains(w, "os/exec.Command spawns an unresolved program (") {
		t.Errorf("proc witness = %s, want it to say the program is unresolved, and why", w)
	}
}

func TestAnUnresolvedProgramIsACLIRunInATestThatBuildsOne(t *testing.T) {
	// The test builds a CLI with the go tool, so the program it cannot name
	// may be that CLI; the build is another process.
	r := reachOf(t, "buildsgo", "TestBuildsThenRuns")
	expectReach(t, r, EndToEnd, "cli,proc", nil)
	if w := r[CLI].String(); !strings.Contains(w, "buildsgo.RunTool ⇒ os/exec.Command spawns an unresolved program (") || !strings.HasSuffix(w, `which may be a CLI the reach builds with "go"`) {
		t.Errorf("cli witness = %s, want the unresolved spawn, which may be the CLI the reach builds", w)
	}
	// The binary's other test builds nothing: its unresolved program cannot
	// be a CLI.
	expectReach(t, reachOf(t, "buildsgo", "TestRunsOnly"), Integration, "proc", nil)
}

func TestAGoBuildInTheSetupMakesEveryTestsUnresolvedProgramACLIRun(t *testing.T) {
	// TestMain builds the CLI and never runs it: every test that runs a
	// program it cannot name may be running that CLI.
	for _, test := range []string{"TestRunsTheBuiltCLI", "TestRunsItAgain"} {
		r := reachOf(t, "builtcli", test)
		expectReach(t, r, EndToEnd, "cli,proc", nil)
		if w := r[CLI].String(); !strings.Contains(w, "builtcli.Run ⇒ os/exec.Command spawns an unresolved program (") || !strings.HasSuffix(w, `which may be a CLI the reach builds with "go"`) {
			t.Errorf("%s cli witness = %s, want the unresolved spawn, which may be the CLI the setup builds", test, w)
		}
	}
}

func TestAStringMethodNestedInAPrintedStructIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "stringer", "TestDescribe"), Integration, "fs", map[Touch]string{
		FS: "stringer.TestDescribe → stringer.Describe → (stringer.path).String [converts stringer.report to an interface] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAnOutsideClientBehindAModulePortIsTheNetwork(t *testing.T) {
	expectReach(t, reachOf(t, "httpclient", "TestFetch"), Integration, "net", nil)
}

func TestHandingTToAHelperChargesOnlyWhatTheHelperCalls(t *testing.T) {
	expectReach(t, reachOf(t, "tbhelper", "TestCheck"), Unit, "none", nil)
	expectReach(t, reachOf(t, "tbhelper", "TestScratch"), Integration, "fs", map[Touch]string{
		FS: "tbhelper.TestScratch → tbhelper.Scratch ⇒ testing.TB.TempDir (fs)",
	})
}

func TestABuildTagReachesTheTestsItSelects(t *testing.T) {
	expectReach(t, reachOf(t, "tagged", "TestTagged"), Integration, "fs", nil)
	expectReach(t, reachOf(t, "tagged", "TestPure"), Unit, "none", nil)
}

func TestASkippedTestCountsInItsLayerAndIsMarkedNotRun(t *testing.T) {
	shape, refusals := measureFixture(t, "skipped")
	if len(refusals) > 0 {
		t.Fatalf("refused: %v", refusals)
	}
	pkg := gotestjson.Package{ImportPath: "example.com/shape/skipped", Tests: []gotestjson.Test{
		{Name: "TestPure", Elapsed: 200 * time.Millisecond, Ending: gotestjson.Passed},
		{Name: "TestRead", Ending: gotestjson.Skipped},
	}}
	m, refusals := join(pkg, shape)
	if len(refusals) > 0 {
		t.Fatalf("join refused: %v", refusals)
	}
	r := rowOf(m, "example.com/shape")
	if r.Count != [numLayers]int{1, 1, 0} || r.Summed != [numLayers]time.Duration{200 * time.Millisecond, 0, 0} {
		t.Errorf("row counts %v summed %v, want one unit test of 0.2s and one integration test of no time", r.Count, r.Summed)
	}
	var listing strings.Builder
	writeListing(&listing, []modulePackage{{m, module{Path: "example.com/shape"}}})
	want := "\nexample.com/shape/skipped\n" +
		"  unit        TestPure\n" +
		"  integration TestRead  [not run]\n" +
		"    fs: skipped.TestRead → skipped.Read → os.ReadFile ⇒ os.ReadFile (fs)\n"
	if listing.String() != want {
		t.Errorf("listing:\n%s\nwant:\n%s", listing.String(), want)
	}
}

func TestWhatTestMainTouchesEveryTestOfItsBinaryTouches(t *testing.T) {
	expectReach(t, reachOf(t, "setupmain", "TestAdd"), Integration, "fs", map[Touch]string{
		FS: "setupmain.TestMain → os.MkdirAll ⇒ os.MkdirAll (fs)",
	})
	shape, _ := measureFixture(t, "setupmain")
	if got := shape.Setup.Reached.set(); got != Touches(0).With(FS) {
		t.Errorf("setup touches %s, want fs", got)
	}
}

func TestAnInitializerOnlyADependencyImportsRunsInTheSetup(t *testing.T) {
	shape, refusals := measureFixture(t, "transitiveinit")
	if len(refusals) > 0 {
		t.Fatalf("measuring transitiveinit refused:\n%s", strings.Join(refusals, "\n"))
	}
	if got := shape.Setup.Reached.set(); got != Touches(0).With(FS) {
		t.Errorf("setup touches %s, want fs", got)
	}
	expectReach(t, reachOf(t, "transitiveinit", "TestHome"), Integration, "fs", map[Touch]string{
		FS: "nested/deep.init ⇒ example.com/shape/nested/deep.init (fs)",
	})
}

// What an outside generic does with a type argument is read from its
// source: a body that neither converts the type nor calls a method of it
// reaches nothing of it, and the test names the type argument withheld.
func TestATypeArgumentAGenericNeitherConvertsNorCallsIsWithheld(t *testing.T) {
	ts := testShapeOf(t, "genericclone", "TestCopy")
	expectReach(t, ts.Reached, Unit, "none", nil)
	expectWithheld(t, ts, "slices.Clone[genericclone.paths, string] neither converts its type parameter S (genericclone.paths) nor calls a method of it")
}

// Withholding is decided per type argument: an instance that converts its
// first reaches the first's methods, and withholds its second.
func TestAGenericWithholdsEachTypeArgumentItNeitherConvertsNorCalls(t *testing.T) {
	ts := testShapeOf(t, "genericpair", "TestPick")
	expectReach(t, ts.Reached, Integration, "fs", map[Touch]string{
		FS: "genericpair.TestPick → genericpair.Pick → nested/gen.First[genericpair.doc, genericpair.page] → (genericpair.doc).Close [nested/gen.First[genericpair.doc, genericpair.page] converts genericpair.doc to an interface] → os.Remove ⇒ os.Remove (fs)",
	})
	expectWithheld(t, ts, "nested/gen.First[genericpair.doc, genericpair.page] neither converts its type parameter B (genericpair.page) nor calls a method of it")
}

// Withholding is per type parameter, not per type: one type handed as two
// type arguments is converted through the first and withheld as the second.
func TestATypeParameterIsWithheldThoughItsTypeIsAnotherParametersToo(t *testing.T) {
	ts := testShapeOf(t, "genericpair", "TestPickSame")
	expectReach(t, ts.Reached, Integration, "fs", map[Touch]string{
		FS: "genericpair.TestPickSame → genericpair.PickSame → nested/gen.First[genericpair.doc, genericpair.doc] → (genericpair.doc).Close [nested/gen.First[genericpair.doc, genericpair.doc] converts genericpair.doc to an interface] → os.Remove ⇒ os.Remove (fs)",
	})
	expectWithheld(t, ts, "nested/gen.First[genericpair.doc, genericpair.doc] neither converts its type parameter B (genericpair.doc) nor calls a method of it")
}

// A constraint call reaches one method of its type argument: the other
// methods of the type it points to are not reached, and a type parameter
// whose argument is that type is withheld.
func TestAConstraintCallReachesOneMethodAndLeavesTheOtherParameterWithheld(t *testing.T) {
	ts := testShapeOf(t, "genericonly", "TestShut")
	expectReach(t, ts.Reached, Integration, "fs", map[Touch]string{
		FS: "genericonly.TestShut → genericonly.Shut → nested/gen.CloseFirst[*genericonly.doc, genericonly.doc] → (*genericonly.doc).Close [nested/gen.CloseFirst[*genericonly.doc, genericonly.doc] calls Close of *genericonly.doc through its constraint] → (genericonly.doc).Close → os.Remove ⇒ os.Remove (fs)",
	})
	expectWithheld(t, ts, "nested/gen.CloseFirst[*genericonly.doc, genericonly.doc] neither converts its type parameter D (genericonly.doc) nor calls a method of it")
}

// A promotion wrapper runs the embedded type's method: it is read as an
// instance of that method, over the embedded type's type arguments.
func TestAPromotedMethodOfAGenericTypeIsReadAsTheEmbeddedTypesMethod(t *testing.T) {
	expectReach(t, reachOf(t, "genericpromote", "TestShow"), Integration, "fs", map[Touch]string{
		FS: "genericpromote.TestShow → genericpromote.Show → (nested/gen.Outer[genericpromote.doc]).Show [converts nested/gen.Outer[genericpromote.doc] to an interface] → (genericpromote.doc).Close [nested/gen.Shelf.Show[genericpromote.doc] converts genericpromote.doc to an interface] → os.Remove ⇒ os.Remove (fs)",
	})
}

// A type made of a type argument, converted, reaches the methods of every
// module type in its structure; a type over the argument that holds none of
// it reaches nothing of it, and the argument is not withheld: the body uses
// it.
func TestAConvertedTypeMadeOfATypeArgumentReachesWhatItsStructureHolds(t *testing.T) {
	close := func(test, fn, inst, conv string) string {
		return "genericshapes." + test + " → genericshapes." + fn + " → " + inst + " → (genericshapes.doc).Close [" + inst + " converts " + conv + " to an interface] → os.Remove ⇒ os.Remove (fs)"
	}
	for _, tc := range []struct {
		test, fn, inst, conv string
	}{
		{"TestSlice", "InSlice", "nested/gen.Slice[genericshapes.doc]", "[]genericshapes.doc"},
		{"TestField", "InField", "nested/gen.Field[genericshapes.doc]", `struct{F genericshapes.doc "json:\"f\""}`},
		{"TestPointer", "InPointer", "nested/gen.Pointer[genericshapes.doc]", "*genericshapes.doc"},
		{"TestHolder", "InHolder", "nested/gen.Hold[genericshapes.doc]", "nested/gen.holder[genericshapes.doc]"},
	} {
		ts := testShapeOf(t, "genericshapes", tc.test)
		expectReach(t, ts.Reached, Integration, "fs", map[Touch]string{FS: close(tc.test, tc.fn, tc.inst, tc.conv)})
		expectWithheld(t, ts)
	}
	ts := testShapeOf(t, "genericshapes", "TestMap")
	m := "nested/gen.Map[genericshapes.doc, genericshapes.page]"
	expectReach(t, ts.Reached, Integration, "net,fs", map[Touch]string{
		FS:  close("TestMap", "InMap", m, "map[genericshapes.doc]genericshapes.page"),
		Net: "genericshapes.TestMap → genericshapes.InMap → " + m + " → (genericshapes.page).Fetch [" + m + " converts map[genericshapes.doc]genericshapes.page to an interface] → net/http.Get ⇒ net/http.Get (net)",
	})
	expectWithheld(t, ts)
	for _, test := range []string{"TestFunc", "TestTag"} {
		ts := testShapeOf(t, "genericshapes", test)
		expectReach(t, ts.Reached, Unit, "none", nil)
		expectWithheld(t, ts)
	}
}

func expectWithheld(t *testing.T, ts testShape, want ...string) {
	t.Helper()
	var got []string
	for _, wh := range ts.Withheld {
		got = append(got, wh.text(namer{module{Path: "example.com/shape"}}))
	}
	if !slices.Equal(got, want) {
		t.Errorf("withheld = %q, want %q", got, want)
	}
}

// A constraint method called on a type argument that is an interface is an
// interface call: it reaches the method of each runtime type implementing
// the interface, here an *os.File that reached it through any.
func TestAConstraintMethodCalledOnAnInterfaceTypeArgumentIsAnInterfaceCall(t *testing.T) {
	expectReach(t, reachOf(t, "genericclose", "TestRelease"), Integration, "fs", map[Touch]string{
		FS: "genericclose.TestRelease → genericclose.Release → nested/gen.Shut[genericclose.Closer] → (*os.File).Close [interface call reaches *os.File from TestRelease, made in genericclose.Release] ⇒ os.File.Close (fs)",
	})
}

// A bound method value of an outside generic type is an instance of the
// method, its type arguments its receiver type's. A call of the closure
// where it is made is a static call of its wrapper.
func TestABoundMethodValueOfAGenericTypeIsReadWithItsReceiversTypeArguments(t *testing.T) {
	expectReach(t, reachOf(t, "genericbound", "TestFill"), Integration, "fs", map[Touch]string{
		FS: "genericbound.TestFill → genericbound.Fill → (*nested/gen.Bag[genericbound.doc]).Add$bound → (genericbound.doc).Close [nested/gen.Bag.Add[genericbound.doc] converts genericbound.doc to an interface] → os.Remove ⇒ os.Remove (fs)",
	})
}

func TestAConstraintMethodAGenericCallsIsReachedOnItsTypeArgument(t *testing.T) {
	expectReach(t, reachOf(t, "genericjoin", "TestNames"), Integration, "fs", map[Touch]string{
		FS: "genericjoin.TestNames → genericjoin.Names → nested/strs.Join[genericjoin.file] → (genericjoin.file).String [nested/strs.Join[genericjoin.file] calls String of genericjoin.file through its constraint] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

// A method expression of a constraint method is a thunk the generic hands
// out, which calls the method: it is read as part of the body.
func TestAConstraintMethodAGenericTakesAsAMethodExpressionIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "genericthunk", "TestName"), Integration, "fs", map[Touch]string{
		FS: "genericthunk.TestName → genericthunk.Name → nested/gen.Namer[genericthunk.file] → (genericthunk.file).String [nested/gen.Namer[genericthunk.file] calls String of genericthunk.file through its constraint] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

// A type argument the body converts reaches what module code converting it
// reaches: every method of the type, GoString included, which %#v calls.
func TestATypeArgumentAGenericConvertsHasEveryMethodReached(t *testing.T) {
	expectReach(t, reachOf(t, "generickeep", "TestRender"), Integration, "fs", map[Touch]string{
		FS: "generickeep.TestRender → generickeep.Render → nested/gen.Keep[generickeep.doc] → (generickeep.doc).GoString [nested/gen.Keep[generickeep.doc] converts generickeep.doc to an interface] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAModuleTypeInAConvertedTypeArgumentsStructureIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "genericshow", "TestShowSlice"), Integration, "fs", map[Touch]string{
		FS: "genericshow.TestShowSlice → genericshow.ShowSlice → nested/gen.Show[[]genericshow.file] → (genericshow.file).String [nested/gen.Show[[]genericshow.file] converts []genericshow.file to an interface] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
	expectReach(t, reachOf(t, "genericshow", "TestShowStruct"), Integration, "fs", map[Touch]string{
		FS: "genericshow.TestShowStruct → genericshow.ShowStruct → nested/gen.Show[genericshow.wrap] → (genericshow.file).String [nested/gen.Show[genericshow.wrap] converts genericshow.wrap to an interface] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestATypeArgumentADispatchingGenericRegistersIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "genericvisit", "TestRegister"), Integration, "fs", map[Touch]string{
		FS: "genericvisit.TestRegister → genericvisit.Register → nested/gen.Visit[genericvisit.cleaner] → (genericvisit.cleaner).Visit [nested/gen.Visit[genericvisit.cleaner] converts genericvisit.cleaner to an interface] → os.RemoveAll ⇒ os.RemoveAll (fs)",
	})
}

func TestATypeArgumentAnInstantiatedGenericConvertsIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "generictwice", "TestHold"), Integration, "fs", map[Touch]string{
		FS: "generictwice.TestHold → generictwice.Hold → nested/gen.Twice[generictwice.doc] → (generictwice.doc).Close [nested/gen.Twice[generictwice.doc] instantiates nested/gen.Keep[generictwice.doc], which converts generictwice.doc to an interface] → os.Remove ⇒ os.Remove (fs)",
	})
}

// A body the reading cannot decide converts every type argument, the reach
// a module conversion gets, and the witness says why it was not read.
func TestATypeArgumentOfAGenericThatCannotBeReadIsConverted(t *testing.T) {
	expectReach(t, reachOf(t, "genericaddress", "TestWhere"), Integration, "fs", map[Touch]string{
		FS: "genericaddress.TestWhere → genericaddress.Where → nested/gen.Address[genericaddress.lock] → (genericaddress.lock).Release [nested/gen.Address[genericaddress.lock] cannot be read (it converts *T to unsafe.Pointer), so it counts as one that converts genericaddress.lock to an interface] → os.Remove ⇒ os.Remove (fs)",
	})
	expectReach(t, reachOf(t, "genericlocal", "TestWrap"), Integration, "fs", map[Touch]string{
		FS: "genericlocal.TestWrap → genericlocal.Wrap → nested/gen.Box[genericlocal.lock] → (genericlocal.lock).Release [nested/gen.Box[genericlocal.lock] cannot be read (it declares type box in its body), so it counts as one that converts genericlocal.lock to an interface] → os.Remove ⇒ os.Remove (fs)",
	})
}

// Each instance whose body could not be read is counted, with why: its
// conservative reading is in the output, not only in a witness. Beside it,
// the type parameters withheld are counted.
func TestTheOutputCountsTheGenericInstancesThatCouldNotBeReadAndWhy(t *testing.T) {
	l, err := shapeModule()
	if err != nil {
		t.Fatal(err)
	}
	a := newAnalyzer(l, shapeClassification(t))
	for _, pkg := range []string{"genericaddress", "genericlocal", "generickeep", "genericclone"} {
		a.measure(l.Binaries["example.com/shape/"+pkg])
	}
	if refusals := analysisRefusals(a); len(refusals) > 0 {
		t.Fatalf("refused:\n%s", strings.Join(refusals, "\n"))
	}
	var unread []moduleUnread
	for _, u := range a.unreadInstances() {
		unread = append(unread, moduleUnread{u, l.Module})
	}
	var out strings.Builder
	writeGenerics(&out, unread, len(a.withheldParameters()))
	want := "\ntype parameters of outside generic instances withheld, reaching nothing of their type arguments: 1\n" +
		"outside generic instances whose bodies could not be read, each counted as converting every type argument: 2\n" +
		"  nested/gen.Address[genericaddress.lock]: it converts *T to unsafe.Pointer\n" +
		"  nested/gen.Box[genericlocal.lock]: it declares type box in its body\n"
	if out.String() != want {
		t.Errorf("unread:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestARangeOverFuncLoopBodyIsCalledBackByTheIterator(t *testing.T) {
	expectReach(t, reachOf(t, "chunk", "TestSave"), Integration, "fs", map[Touch]string{
		FS: "chunk.TestSave → chunk.Save → chunk.Save$1 [dynamic call reaches value from TestSave, made in chunk.Save] → os.WriteFile ⇒ os.WriteFile (fs)",
	})
}

func TestAFuzzFunctionHandedToFFuzzIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "fuzzsave", "FuzzSave"), Integration, "fs", map[Touch]string{
		FS: "fuzzsave.FuzzSave → (*testing.F).Fuzz → fuzzsave.FuzzSave$1 [call into testing.F.Fuzz reaches value from FuzzSave, made in fuzzsave.FuzzSave] → os.WriteFile ⇒ os.WriteFile (fs)",
	})
}

func TestAClosureCalledThroughReflectionIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "reflectcall", "TestSave"), Integration, "fs", map[Touch]string{
		FS: "reflectcall.TestSave → reflectcall.Save → reflect.ValueOf → reflectcall.Save$1 [call into reflect.ValueOf reaches value from TestSave, made in reflectcall.Save] → os.WriteFile ⇒ os.WriteFile (fs)",
	})
}

func TestAProgramStoredIntoACmdsPathIsSpawned(t *testing.T) {
	expectReach(t, reachOf(t, "cmdfield", "TestStatus"), Integration, "git", map[Touch]string{
		Git: `cmdfield.TestStatus → cmdfield.Status ⇒ os/exec.Cmd.Path spawns "/usr/bin/git", which is git`,
	})
}

func TestAMethodAnOutsideFunctionCallsBackIsChargedByItsClassification(t *testing.T) {
	expectReach(t, reachOf(t, "callback", "TestPrint"), Integration, "net", map[Touch]string{
		Net: "callback.TestPrint → callback.Print → fmt.Sprint → (*net/url.URL).String [fmt.Stringer callback reaches *net/url.URL from TestPrint, made in callback.Print] ⇒ net/url.URL.String (net)",
	})
}

func TestACalledBackInterfaceTheProgramLacksRefuses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(shapeDir, "classification.json"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"calls:fmt.Stringer,calls:error"`, `"calls:example.com/absent.I"`, 1))
	class, err := parseClassification(data)
	if err != nil {
		t.Fatal(err)
	}
	l, err := shapeModule()
	if err != nil {
		t.Fatal(err)
	}
	a := newAnalyzer(l, class)
	a.measure(l.Binaries["example.com/shape/callback"])
	want := []string{"called-back interface example.com/absent.I is not in the program, called back by fmt.Sprint"}
	if got := analysisRefusals(a); !slices.Equal(got, want) {
		t.Errorf("refusals = %q, want %q", got, want)
	}
}

func TestAFunctionStoredInAStructHandedToOutsideCodeIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "funcfield", "TestNew"), Integration, "fs", map[Touch]string{
		FS: "funcfield.TestNew → funcfield.New → log/slog.NewTextHandler → funcfield.redact [call into log/slog.NewTextHandler reaches value from TestNew, made in funcfield.New] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAParameterOfAFunctionReachedDynamicallyMayHoldAnyProgram(t *testing.T) {
	// spawn's static call hands it git; apply's dynamic call hands it the
	// test binary, which no static call shows, so the program is unresolved.
	// Under a TestMain that runs the CLI, it may be the test binary as the
	// CLI.
	expectReach(t, reachOf(t, "spawnindirect", "TestStatusAndRerun"), EndToEnd, "git,cli,proc", map[Touch]string{
		Git: "spawnindirect.TestStatusAndRerun → spawnindirect.Status → spawnindirect.spawn ⇒ os/exec.Command spawns \"git\", which is git",
		CLI: "spawnindirect.TestStatusAndRerun → spawnindirect.Status → spawnindirect.spawn ⇒ os/exec.Command spawns an unresolved program (parameter program of spawnindirect.spawn, which a call other than a static one may pass anything: dynamic call reaches value from TestStatusAndRerun, made in spawnindirect.Rerun), which may be the test binary, whose setup runs the CLI",
	})
}

func TestAModuleNestedUnderTheModulesPathIsOutsideIt(t *testing.T) {
	expectReach(t, reachOf(t, "nesteddep", "TestConnect"), Integration, "net", map[Touch]string{
		Net: "nesteddep.TestConnect → nesteddep.Connect → nested.Dial ⇒ example.com/shape/nested.Dial (net)",
	})
}

func TestAnUnclassifiedOutsideFunctionRefusesNamingItsFirstCaller(t *testing.T) {
	_, refusals := measureFixture(t, "unclassified")
	want := []string{"unclassified outside function strings.ToUpper, first called by unclassified.Shout"}
	if !slices.Equal(refusals, want) {
		t.Errorf("refusals = %q, want %q", refusals, want)
	}
}

func TestAModuleFunctionWithNoBodyRefuses(t *testing.T) {
	_, refusals := measureFixture(t, "bodiless")
	want := []string{"module function bodiless.nanotime has no Go body to walk, reached from bodiless.Now"}
	if !slices.Equal(refusals, want) {
		t.Errorf("refusals = %q, want %q", refusals, want)
	}
}

// passingStream is a go test -json stream in which every test the fixture
// module declares passed.
func passingStream(t *testing.T) string {
	t.Helper()
	l, err := shapeModule()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, path := range sortedKeys(l.Binaries) {
		fmt.Fprintf(&b, `{"Action":"start","Package":%q}`+"\n", path)
		for _, test := range l.Binaries[path].Tests {
			fmt.Fprintf(&b, `{"Action":"run","Package":%q,"Test":%q}`+"\n", path, test.Name)
			fmt.Fprintf(&b, `{"Action":"pass","Package":%q,"Test":%q,"Elapsed":0}`+"\n", path, test.Name)
		}
		fmt.Fprintf(&b, `{"Action":"pass","Package":%q,"Elapsed":0.1}`+"\n", path)
	}
	return b.String()
}

func TestTheRunExitsOneOnWhatTheAnalysisRefuses(t *testing.T) {
	class, err := os.ReadFile(filepath.Join(shapeDir, "classification.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run(class, []string{"-root", shapeDir, "-tags", "shapetag"}, strings.NewReader(passingStream(t)), &stdout, &stderr)
	wantErr := "" +
		"testshape: refused: unclassified outside function strings.ToUpper, first called by unclassified.Shout\n" +
		"testshape: refused: module function bodiless.nanotime has no Go body to walk, reached from bodiless.Now\n"
	if code != exitRefused || stdout.String() != "" || stderr.String() != wantErr {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, no table, stderr %q", code, stdout.String(), stderr.String(), wantErr)
	}
}

func TestTheRunRefusesAPackageAbsentFromTheStream(t *testing.T) {
	stream := `{"Action":"start","Package":"example.com/shape/twodeep"}
{"Action":"run","Package":"example.com/shape/twodeep","Test":"TestLoad"}
{"Action":"pass","Package":"example.com/shape/twodeep","Test":"TestLoad","Elapsed":0}
{"Action":"pass","Package":"example.com/shape/twodeep","Elapsed":0.1}
`
	class, err := os.ReadFile(filepath.Join(shapeDir, "classification.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run(class, []string{"-root", shapeDir, "-tags", "shapetag"}, strings.NewReader(stream), &stdout, &stderr)
	want := "testshape: refused: example.com/shape/selfcli: its test binary declares tests, and the stream has no result for the package\n"
	if code != exitRefused || !strings.Contains(stderr.String(), want) {
		t.Errorf("exit %d, stderr %q; want exit 1 naming each absent package", code, stderr.String())
	}
}

func TestTheRunExitsThreeOnAMalformedStream(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run(classificationData, nil, strings.NewReader(`{"Action":"explode","Package":"p"}`+"\n"), &stdout, &stderr)
	if code != exitMalformed || !strings.Contains(stderr.String(), "malformed go test -json stream at") {
		t.Errorf("exit %d, stderr %q; want exit 3 naming the malformed line", code, stderr.String())
	}
}

func TestTheSuiteIsEveryModuleTheGoCommandWalks(t *testing.T) {
	mods, err := suiteModules(filepath.Join("testdata", "suite"))
	if err != nil {
		t.Fatal(err)
	}
	want := []module{
		{Dir: filepath.Join("testdata", "suite"), Path: "example.com/suite"},
		{Dir: filepath.Join("testdata", "suite", "nested"), Path: "example.com/suite/nested"},
	}
	if !slices.Equal(mods, want) {
		t.Errorf("modules = %v, want %v (none under testdata, nor a directory starting . or _)", mods, want)
	}
}

func TestAClassificationThatNamesNothingRefuses(t *testing.T) {
	class, err := parseClassification([]byte(`{
		"cli": ["example.com/shape/selfcli.Missing", "os.Exit"],
		"modules": {"std": {"init": "none"}, "example.com/shape/nested": {"init": "none"}, "example.com/gone": {"init": "none"}},
		"packages": {
			"os": {"default": "fs", "functions": {"NoSuchFunction": "none", "File.NoSuchMethod": "none", "Getenv": "spawn:3"}},
			"example.com/nowhere": {"default": "none"},
			"example.com/nothing/...": {"default": "none"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	l, err := shapeModule()
	if err != nil {
		t.Fatal(err)
	}
	err = class.Validate(suiteOf([]*loaded{l}))
	want := "the classification names what does not exist:\n" +
		"  cli example.com/shape/selfcli.Missing: example.com/shape/selfcli declares no function Missing; delete the entry\n" +
		"  cli os.Exit: os is not a package of the suite's modules, whose code alone a walk reads; delete the entry\n" +
		"  example.com/nothing/...: the suite loads no package it names; delete the entry\n" +
		"  example.com/nowhere: the suite loads no package it names; delete the entry\n" +
		"  module example.com/gone: no measured build links a package of it; delete the entry\n" +
		"  os.File.NoSuchMethod: no package it names declares File.NoSuchMethod; delete the entry\n" +
		"  os.Getenv: spawn argument is not a parameter of Getenv\n" +
		"  os.NoSuchFunction: no package it names declares NoSuchFunction; delete the entry"
	if err == nil || err.Error() != want {
		t.Errorf("Validate = %v\nwant %s", err, want)
	}
}
