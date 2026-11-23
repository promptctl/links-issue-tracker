package main

import (
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// fixtureDir is the module whose packages are each named for what they
// touch. Every expectation below is written from a fixture's design, stated
// in its comments, never read back off this tool's output.
var fixtureDir = filepath.Join("testdata", "src")

// fixtureModule is the fixture module's path.
const fixtureModule = "example.com/fixtures"

// fixtureMarkers are the fixture module's own markers, its CLI and its store,
// as moduleMarkers are this module's.
var fixtureMarkers = []marker{
	{"internal/cli.Run", touchCLI},
	{"internal/store.Open", touchStore},
}

func TestClassifiesEachFixtureByWhatItTouches(t *testing.T) {
	l, err := load(fixtureDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := buildGraph(l, newMarkerSet(l.module, fixtureMarkers, externalMarkers, spawners))
	for _, c := range []struct {
		pkg, test string
		touches   string // touched touches, in column order; "-" for none
		layer     layer
	}{
		{"fstwocallsdeep", "TestSaveWritesTwoCallsDown", "fs", layerIntegration},
		{"gitthroughaninterface", "TestDriveHandedAFakeTouchesNothing", "-", layerUnit},
		{"gitthroughaninterface", "TestTheConstructorCarriesGit", "git", layerIntegration},
		{"gitthroughaninterface", "TestACompositeLiteralThroughTheInterfaceTouchesGit", "git", layerIntegration},
		{"replacedclock", "TestStampOnTheRealClockTouchesTheClock", "clock", layerIntegration},
		{"replacedclock", "TestStampOnAReplacedClockTouchesNothing", "-", layerUnit},
		{"testmainclock", "TestStampUnderTestMainsClockTouchesNothing", "-", layerUnit},
		{"localnamedlikemarker", "TestLocalsSpelledLikeMarkersTouchNothing", "-", layerUnit},
		{"helpernamedlikemarker", "TestHelpersNamedLikeMarkersTouchNothing", "-", layerUnit},
		{"storeopen", "TestOpenTouchesTheStore", "store", layerIntegration},
		{"storethroughinterface", "TestAppOpenCarriesTheStore", "store", layerIntegration},
		{"storethroughinterface", "TestAnAppOnAMemoryStoreTouchesNothing", "-", layerUnit},
		{"programconstant", "TestGitNamedByAConstantIsGit", "git", layerIntegration},
		{"programconstant", "TestAPathEndingInGitIsGit", "git", layerIntegration},
		{"programconstant", "TestGitLookedUpIsGit", "git", layerIntegration},
		{"programconstant", "TestACmdBuiltAsAValueSpawnsItsPath", "git", layerIntegration},
		{"programconstant", "TestAnyOtherProgramIsAnotherProcess", "proc", layerIntegration},
		{"programconstant", "TestASpawnerInALocalIsStillGit", "git", layerIntegration},
		{"programconstant", "TestASpawnerHandedOnAsAValueIsAnotherProcess", "proc", layerIntegration},
		{"subtestnamedlikeprogram", "TestASubtestNamedGitIsUnit", "-", layerUnit},
		{"gitthroughtwohelpers", "TestGitThroughTwoHelpersIsGit", "git", layerIntegration},
		{"gitthroughtwohelpers", "TestMakeThroughTwoHelpersIsAnotherProcess", "proc", layerIntegration},
		{"gitthroughtwohelpers", "TestGitCapturedByAClosureIsGit", "git", layerIntegration},
		{"reexeccli", "TestReRunningTheBinaryRunsTheCLI", "cli", layerE2E},
		{"helperprocess", "TestReRunningTheBinaryForOneTestIsAnotherProcess", "proc", layerIntegration},
		{"helperprocess", "TestHelper", "-", layerUnit},
		{"network", "TestAListenerTouchesTheNetwork", "network", layerIntegration},
		{"importerprocess", "TestTheDefaultImporterIsAnotherProcess", "proc", layerIntegration},
		{"cliinprocess", "TestRunningTheCLIInProcessIsEndToEnd", "cli", layerE2E},
	} {
		t.Run(c.pkg+"/"+c.test, func(t *testing.T) {
			s := classifyOne(t, g, l, c.pkg, c.test)
			if got := s.touches.names(); got != c.touches {
				t.Errorf("touches = %s, want %s", got, c.touches)
			}
			if got := layerOf(s.touches); got != c.layer {
				t.Errorf("layer = %s, want %s", got, c.layer)
			}
		})
	}
}

// classifyOne classifies one fixture test as a stream that ran only it.
func classifyOne(t *testing.T, g *graph, l *loaded, pkg, test string) testShape {
	t.Helper()
	shapes, err := classify(g, l, []gotestjson.Package{{
		ImportPath: fixtureModule + "/" + pkg,
		Tests:      []gotestjson.Test{{Name: test}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return shapes[0].tests[0]
}

func TestAWitnessChainIsTheCallsFromTheTestToTheTouch(t *testing.T) {
	l, err := load(fixtureDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := buildGraph(l, newMarkerSet(l.module, fixtureMarkers, externalMarkers, spawners))
	for _, c := range []struct {
		pkg, test string
		touch     touch
		chain     string
	}{
		// The store is reached through app.Store; the chain runs through
		// the constructor that built it, and stops at the marker.
		{"storethroughinterface", "TestAppOpenCarriesTheStore", touchStore,
			"storethroughinterface.TestAppOpenCarriesTheStore → internal/app.Open → internal/store.Open"},
		// The program "git" is resolved through both helpers' parameters.
		{"gitthroughtwohelpers", "TestGitThroughTwoHelpersIsGit", touchGit,
			"gitthroughtwohelpers.TestGitThroughTwoHelpersIsGit → gitthroughtwohelpers.run → gitthroughtwohelpers.runIn → os/exec.Command"},
	} {
		s := classifyOne(t, g, l, c.pkg, c.test)
		if got := strings.Join(s.touches[c.touch], " → "); got != c.chain {
			t.Errorf("%s %s: chain = %s, want %s", c.pkg, c.test, got, c.chain)
		}
	}
}

func TestATestMainsTouchesAreItsPackagesNotItsTests(t *testing.T) {
	l, err := load(fixtureDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := buildGraph(l, newMarkerSet(l.module, fixtureMarkers, externalMarkers, spawners))
	shapes, err := classify(g, l, []gotestjson.Package{
		{ImportPath: fixtureModule + "/reexeccli", Tests: []gotestjson.Test{{Name: "TestReRunningTheBinaryRunsTheCLI"}}},
		{ImportPath: fixtureModule + "/helperprocess", Tests: []gotestjson.Test{{Name: "TestHelper"}}},
		{ImportPath: fixtureModule + "/network", Tests: []gotestjson.Test{{Name: "TestAListenerTouchesTheNetwork"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// reexeccli's TestMain calls main in its child branch; helperprocess's
	// only runs the tests; network has none.
	for i, want := range []string{"cli", "-", "no TestMain"} {
		got := "no TestMain"
		if shapes[i].testMain != nil {
			got = shapes[i].testMain.names()
		}
		if got != want {
			t.Errorf("%s TestMain touches = %s, want %s", shapes[i].importPath, got, want)
		}
	}
}

// stream joins raw go test -json lines into one stream.
func stream(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// passing is the events of one top-level test that passed after elapsed
// seconds, in pkg under the fixture module.
func passing(pkg, test, elapsed string) []string {
	return []string{
		`{"Time":"2026-01-01T00:00:00Z","Action":"run","Package":"` + fixtureModule + "/" + pkg + `","Test":"` + test + `"}`,
		`{"Time":"2026-01-01T00:00:09Z","Action":"pass","Package":"` + fixtureModule + "/" + pkg + `","Test":"` + test + `","Elapsed":` + elapsed + `}`,
		`{"Time":"2026-01-01T00:00:09Z","Action":"pass","Package":"` + fixtureModule + "/" + pkg + `","Elapsed":` + elapsed + `}`,
	}
}

// runFixture runs testshape on lines against the fixture module.
func runFixture(cfg config, lines ...string) (code int, stdout, stderr string) {
	var out, errOut strings.Builder
	code = run(cfg, stream(lines...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestAMeasuredStreamPrintsTheTableAndTheListing(t *testing.T) {
	var lines []string
	lines = append(lines, passing("network", "TestAListenerTouchesTheNetwork", "1.5")...)
	lines = append(lines, passing("subtestnamedlikeprogram", "TestASubtestNamedGitIsUnit", "0.5")...)
	lines = append(lines, passing("cliinprocess", "TestRunningTheCLIInProcessIsEndToEnd", "2")...)
	code, out, errOut := runFixture(config{dir: fixtureDir, moduleMarkers: fixtureMarkers, listTests: true}, lines...)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	// One unit test of 0.5s, one integration test of 1.5s touching the
	// network, one end-to-end test of 2s running the CLI.
	total := []string{"TOTAL", "3", "1", "0.5s", "1", "1.5s", "1", "2.0s", "33%", "1", "0", "0", "0", "0", "0", "1"}
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "TOTAL") {
			got = strings.Fields(line)
		}
	}
	if strings.Join(got, " ") != strings.Join(total, " ") {
		t.Errorf("TOTAL row = %v, want %v\n%s", got, total, out)
	}
	for _, want := range []string{
		"network TestAListenerTouchesTheNetwork  integration  1.50s  network\n    network: network.TestAListenerTouchesTheNetwork → net.Listen\n",
		"subtestnamedlikeprogram TestASubtestNamedGitIsUnit  unit  0.50s  -\n",
		"cliinprocess TestRunningTheCLIInProcessIsEndToEnd  e2e  2.00s  cli\n    cli: cliinprocess.TestRunningTheCLIInProcessIsEndToEnd → internal/cli.Run\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
}

func TestAStreamThatMeasuredNothingFailsNamingWhy(t *testing.T) {
	pkg := `"Package":"` + fixtureModule + `/network"`
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"an empty stream", nil, "the stream ran no test"},
		{"a failed build", []string{
			`{"Action":"start",` + pkg + `}`,
			`{"Action":"fail",` + pkg + `,"Elapsed":0,"FailedBuild":"` + fixtureModule + `/network [` + fixtureModule + `/network.test]"}`,
		}, fixtureModule + "/network failed to build"},
		{"a test unfinished when its package ended", []string{
			`{"Time":"2026-01-01T00:00:00Z","Action":"run",` + pkg + `,"Test":"TestAListenerTouchesTheNetwork"}`,
			`{"Time":"2026-01-01T00:00:02Z","Action":"fail",` + pkg + `,"Elapsed":2}`,
		}, "TestAListenerTouchesTheNetwork has no verdict: its package ended before it did"},
		{"a test open when the stream ended", []string{
			`{"Time":"2026-01-01T00:00:00Z","Action":"run",` + pkg + `,"Test":"TestAListenerTouchesTheNetwork"}`,
		}, "TestAListenerTouchesTheNetwork has no verdict: the stream ended before its package did"},
		{"a verdict with no elapsed time", []string{
			`{"Time":"2026-01-01T00:00:00Z","Action":"run",` + pkg + `,"Test":"TestAListenerTouchesTheNetwork"}`,
			`{"Time":"2026-01-01T00:00:01Z","Action":"pass",` + pkg + `,"Test":"TestAListenerTouchesTheNetwork"}`,
			`{"Time":"2026-01-01T00:00:01Z","Action":"pass",` + pkg + `,"Elapsed":1}`,
		}, "TestAListenerTouchesTheNetwork has a verdict with no elapsed time"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := runFixture(config{dir: fixtureDir, moduleMarkers: fixtureMarkers}, c.lines...)
			if code == 0 || out != "" || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant a non-zero exit, no report, and stderr naming %q", code, out, errOut, c.want)
			}
		})
	}
}

func TestAMarkerThatNamesNoFunctionFailsTheRun(t *testing.T) {
	markers := append([]marker{{"internal/store.Gone", touchStore}}, fixtureMarkers...)
	code, out, errOut := runFixture(config{dir: fixtureDir, moduleMarkers: markers},
		passing("network", "TestAListenerTouchesTheNetwork", "1")...)
	want := "marker " + fixtureModule + "/internal/store.Gone names no function the module declares"
	if code == 0 || out != "" || !strings.Contains(errOut, want) {
		t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant a non-zero exit, no report, and stderr naming %q", code, out, errOut, want)
	}
}

func TestATestTheProgramDoesNotDeclareFailsTheRun(t *testing.T) {
	code, out, errOut := runFixture(config{dir: fixtureDir, moduleMarkers: fixtureMarkers},
		passing("network", "TestNobodyWrote", "1")...)
	want := fixtureModule + "/network ran TestNobodyWrote, and the loaded module declares no such function"
	if code == 0 || out != "" || !strings.Contains(errOut, want) {
		t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant a non-zero exit, no report, and stderr naming %q", code, out, errOut, want)
	}
}

// TestEveryModuleMarkerNamesADeclaredFunction loads this module's packages
// that markers.go names and fails on a marker that names nothing: a renamed
// opener would otherwise count no touch, and its tests would read as unit.
func TestEveryModuleMarkerNamesADeclaredFunction(t *testing.T) {
	var patterns []string
	for _, m := range moduleMarkers {
		// "(internal/environ.Env).Git" is declared in internal/environ.
		name := strings.TrimLeft(m.name, "(*")
		name, _, _ = strings.Cut(name, ")")
		if p := "./" + name[:strings.LastIndex(name, ".")]; !slices.Contains(patterns, p) {
			patterns = append(patterns, p)
		}
	}
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedModule, Dir: filepath.Join("..", "..")}, patterns...)
	if err != nil {
		t.Fatal(err)
	}
	if err := loadErrors(pkgs); err != nil {
		t.Fatal(err)
	}
	var declared []*types.Package
	for _, p := range pkgs {
		declared = append(declared, p.Types)
	}
	if missing := undeclared(pkgs[0].Module.Path, moduleMarkers, declared); len(missing) > 0 {
		t.Errorf("markers name no declared function: %v", missing)
	}
}
