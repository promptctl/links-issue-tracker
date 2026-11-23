package main

import (
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

// shapeModule is the fixture module, loaded once for every case: the load
// builds export data for its imports, the costly step, and no case changes
// what was loaded. [LAW:no-shared-mutable-globals] each case measures with
// its own analyzer; the loaded program is only read. It loads under the
// fixture's build tag, which is how -tags passing through is proven.
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
	a := newAnalyzer(l.Prog, l.Module, shapeClassification(t))
	shape := a.measure(b)
	return shape, analysisRefusals(a)
}

// reachOf is what one fixture test reaches, the analysis refusing nothing.
func reachOf(t *testing.T, pkg, test string) reached {
	t.Helper()
	shape, refusals := measureFixture(t, pkg)
	if len(refusals) > 0 {
		t.Fatalf("measuring %s refused:\n%s", pkg, strings.Join(refusals, "\n"))
	}
	r, ok := shape.Tests[test]
	if !ok {
		t.Fatalf("%s declares no %s; it declares %v", pkg, test, shape.Order)
	}
	return r
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

func TestAnInterfaceTheCLIImplementsReachesItFromTestMain(t *testing.T) {
	expectReach(t, reachOf(t, "climain", "TestThroughThePort"), Integration, "fs", map[Touch]string{
		FS: "climain.TestThroughThePort → (climain.Command).Execute [interface call reaches climain.Command from TestMain, made in climain.TestMain] → climain.Run → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAnOutsideDispatcherReachesWhatInitRegistered(t *testing.T) {
	expectReach(t, reachOf(t, "initdispatch", "TestServe"), Integration, "fs", map[Touch]string{
		FS: "initdispatch.TestServe → initdispatch.Serve → (*net/http.ServeMux).ServeHTTP → initdispatch.serveConfig [dispatch reaches value from init of initdispatch, made in initdispatch.init#1] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAFunctionValueTestMainStoresIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "testmainvalue", "TestLoad"), Integration, "fs", map[Touch]string{
		FS: "testmainvalue.TestLoad → testmainvalue.ReadConfig [dynamic call reaches value from TestMain, made in testmainvalue.TestMain] → os.ReadFile ⇒ os.ReadFile (fs)",
	})
}

func TestAValueInitBuildsIsReachedThroughAMap(t *testing.T) {
	expectReach(t, reachOf(t, "initmap", "TestOpen"), Integration, "fs", map[Touch]string{
		FS: "initmap.TestOpen → initmap.Open → (initmap.disk).Open [interface call reaches initmap.disk from init of initmap, made in initmap.init#1] → os.MkdirAll ⇒ os.MkdirAll (fs)",
	})
}

func TestAValueInitBuildsIsReachedThroughAStructField(t *testing.T) {
	expectReach(t, reachOf(t, "initfield", "TestPrepare"), Integration, "fs", map[Touch]string{
		FS: "initfield.TestPrepare → initfield.Prepare → initfield.mkdirAll [dynamic call reaches value from init of initfield, made in initfield.init] → os.MkdirAll ⇒ os.MkdirAll (fs)",
	})
}

func TestAValueInitBuildsIsReachedThroughAPointer(t *testing.T) {
	expectReach(t, reachOf(t, "initpointer", "TestOpen"), Integration, "fs", map[Touch]string{
		FS: "initpointer.TestOpen → initpointer.Open → (initpointer.disk).Open [interface call reaches initpointer.disk from init of initpointer, made in initpointer.init#1] → os.MkdirAll ⇒ os.MkdirAll (fs)",
	})
}

func TestACompileTimeAssertionMakesNothing(t *testing.T) {
	expectReach(t, reachOf(t, "varblank", "TestOpenNone"), Unit, "none", nil)
}

func TestACallbackHandedToTRunIsReached(t *testing.T) {
	expectReach(t, reachOf(t, "trun", "TestSubtest"), Integration, "fs", map[Touch]string{
		FS: "trun.TestSubtest → trun.TestSubtest$1 [takes it as a value] → trun.Write → os.WriteFile ⇒ os.WriteFile (fs)",
	})
}

func TestGitThroughTwoHelpersIsGit(t *testing.T) {
	expectReach(t, reachOf(t, "gitwrap", "TestStatus"), Integration, "git", map[Touch]string{
		Git: `gitwrap.TestStatus → gitwrap.Status → gitwrap.git → gitwrap.run ⇒ os/exec.Command spawns "git", which is git`,
	})
}

func TestTheTestBinaryRerunUnderACLITestMainIsEndToEnd(t *testing.T) {
	expectReach(t, reachOf(t, "selfcli", "TestRerun"), EndToEnd, "cli", map[Touch]string{
		CLI: "selfcli.TestRerun ⇒ os/exec.Command spawns os.Args[0], the test binary, whose TestMain runs the CLI",
	})
}

func TestTheTestBinaryRerunUnderAPlainTestMainIsAnotherProcess(t *testing.T) {
	expectReach(t, reachOf(t, "selfplain", "TestRerun"), Integration, "proc", map[Touch]string{
		Proc: "selfplain.TestRerun ⇒ os/exec.Command spawns os.Args[0], the test binary, whose TestMain does not reach the CLI: another process",
	})
}

func TestAMainPackageRunsItselfAndGitThroughOneHelper(t *testing.T) {
	expectReach(t, reachOf(t, "cmdshape", "TestRunsItself"), EndToEnd, "cli", map[Touch]string{
		CLI: "cmdshape.TestRunsItself → cmdshape.runProgram ⇒ os/exec.Command spawns os.Executable(), the test binary, whose TestMain runs the CLI",
	})
	expectReach(t, reachOf(t, "cmdshape", "TestRunsGit"), Integration, "git,fs", map[Touch]string{
		Git: `cmdshape.TestRunsGit → cmdshape.runProgram ⇒ os/exec.Command spawns "git", which is git`,
		FS:  "cmdshape.TestRunsGit → os/exec.LookPath ⇒ os/exec.LookPath (fs)",
	})
}

func TestAnUnresolvedProgramIsAnotherProcess(t *testing.T) {
	r := reachOf(t, "unresolved", "TestRunTool")
	expectReach(t, r, Integration, "proc", nil)
	if w := r[Proc].String(); !strings.Contains(w, "os/exec.Command spawns an unresolved program (") {
		t.Errorf("proc witness = %s, want it to say the program is unresolved, and why", w)
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

func TestABuildTagReachesTheTestsItSelects(t *testing.T) {
	expectReach(t, reachOf(t, "tagged", "TestTagged"), Integration, "fs", nil)
	expectReach(t, reachOf(t, "tagged", "TestPure"), Unit, "none", nil)
}

func TestASkippedTestCountsInItsLayerAndIsMarkedNotRun(t *testing.T) {
	shape, refusals := measureFixture(t, "skipped")
	if len(refusals) > 0 {
		t.Fatalf("refused: %v", refusals)
	}
	pkg := gotestjson.Package{ImportPath: "example.com/shape/skipped", Verdict: gotestjson.Pass, Tests: []gotestjson.Test{
		{Name: "TestPure", Elapsed: 200 * time.Millisecond, Verdict: gotestjson.Pass},
		{Name: "TestRead", Verdict: gotestjson.Skip},
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
	writeListing(&listing, []measuredPackage{m})
	want := "\nexample.com/shape/skipped\n" +
		"  unit        TestPure\n" +
		"  integration TestRead  [not run]\n" +
		"    fs: skipped.TestRead → skipped.Read → os.ReadFile ⇒ os.ReadFile (fs)\n"
	if listing.String() != want {
		t.Errorf("listing:\n%s\nwant:\n%s", listing.String(), want)
	}
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

func TestTheRunExitsOneOnAnUnclassifiedOutsideFunction(t *testing.T) {
	class, err := os.ReadFile(filepath.Join(shapeDir, "classification.json"))
	if err != nil {
		t.Fatal(err)
	}
	stream := `{"Action":"start","Package":"example.com/shape/unclassified"}
{"Action":"run","Package":"example.com/shape/unclassified","Test":"TestShout"}
{"Action":"pass","Package":"example.com/shape/unclassified","Test":"TestShout","Elapsed":0}
{"Action":"pass","Package":"example.com/shape/unclassified","Elapsed":0.1}
`
	var stdout, stderr strings.Builder
	code := run(class, []string{"-root", shapeDir}, strings.NewReader(stream), &stdout, &stderr)
	wantErr := "testshape: refused: unclassified outside function strings.ToUpper, first called by unclassified.Shout\n"
	if code != 1 || stdout.String() != "" || stderr.String() != wantErr {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, no table, stderr %q", code, stdout.String(), stderr.String(), wantErr)
	}
}

func TestTheRunExitsTwoOnAMalformedStream(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run(classificationData, nil, strings.NewReader(`{"Action":"explode","Package":"p"}`+"\n"), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "malformed go test -json stream at") {
		t.Errorf("exit %d, stderr %q; want exit 2 naming the malformed line", code, stderr.String())
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
		"cli": ["example.com/shape/selfcli.Missing"],
		"packages": {
			"os": {"default": "fs", "functions": {"NoSuchFunction": "none", "File.NoSuchMethod": "none", "Getenv": "spawn:3"}},
			"example.com/nowhere": {"default": "none"},
			"example.com/nothing/...": {"default": "none"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	err = class.Validate([]module{{Dir: shapeDir, Path: "example.com/shape"}}, nil)
	want := "the classification names what does not exist:\n" +
		"  cli example.com/shape/selfcli.Missing: example.com/shape/selfcli declares no Missing\n" +
		"  os.File.NoSuchMethod: File has no method NoSuchMethod\n" +
		"  os.Getenv: spawn argument is not a parameter of Getenv\n" +
		"  os.NoSuchFunction: os declares no NoSuchFunction\n" +
		"  package example.com/nowhere does not load from any module of the suite\n" +
		"  pattern example.com/nothing/... covers no module of the build"
	if err == nil || err.Error() != want {
		t.Errorf("Validate = %v\nwant %s", err, want)
	}
}
