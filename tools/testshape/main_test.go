package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// fixtureDir is the module whose packages are each named for what they
// touch. Every expectation below is written from a fixture's design, stated
// in its comments, never read back off this tool's output.
var fixtureDir = filepath.Join("testdata", "src")

// fixtureModule is the fixture module's path.
const fixtureModule = "example.com/fixtures"

// loadFixtures loads the fixture module and builds its graph, by the real
// marker data: the fixture module declares every module marker, and its
// stubs every third-party one.
func loadFixtures(t *testing.T) (*graph, *loaded) {
	t.Helper()
	ms, err := resolveMarkers(fixtureDir, moduleMarkers)
	if err != nil {
		t.Fatal(err)
	}
	l, err := load(fixtureDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return buildGraph(l, ms), l
}

// classifyFixtures classifies one stream that ran each named test once, in
// a package under the fixture module.
func classifyFixtures(t *testing.T, g *graph, l *loaded, tests ...[2]string) []packageShape {
	t.Helper()
	var pkgs []gotestjson.Package
	for _, pt := range tests {
		pkgs = append(pkgs, gotestjson.Package{ImportPath: fixtureModule + "/" + pt[0], Tests: []gotestjson.Test{{Name: pt[1], Verdict: gotestjson.Pass}}})
	}
	shapes, err := classify(g, l, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	return shapes
}

func TestClassifiesEachFixtureByWhatItTouches(t *testing.T) {
	g, l := loadFixtures(t)
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
		{"replacedclock", "TestStampOnAReplacedClockStillTouchesTheClock", "clock", layerIntegration},
		{"testmainclock", "TestStampUnderTestMainsClockStillTouchesTheClock", "clock", layerIntegration},
		{"localnamedlikemarker", "TestLocalsSpelledLikeMarkersTouchNothing", "-", layerUnit},
		{"helpernamedlikemarker", "TestHelpersNamedLikeMarkersTouchNothing", "-", layerUnit},
		{"storeopen", "TestOpenTouchesTheStore", "store", layerIntegration},
		{"storethroughinterface", "TestTheConstructorCarriesTheStore", "store", layerIntegration},
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
		{"callbuiltinitializer", "TestAClockBuiltByACallReadsTheClock", "clock", layerIntegration},
		{"callbuiltinitializer", "TestAHookBuiltByACallRunsGit", "git", layerIntegration},
		{"pointerstoreintovar", "TestAClockSwappedThroughAPointerStillReadsTheClock", "clock", layerIntegration},
		{"initreassignment", "TestAHookAnInitFunctionStoredTouchesTheFilesystem", "fs", layerIntegration},
		{"anothertestsfake", "TestStoringAFakeThatReadsAFileTouchesTheFilesystem", "fs", layerIntegration},
		{"anothertestsfake", "TestAnotherTestsFakeIsNotCharged", "-", layerUnit},
		{"initializerreplacinganother", "TestAVariableReplacedByAnotherStillReachesItsInitializer", "fs", layerIntegration},
		{"testmainchildstore", "TestAHookTestMainReplacesStillTouchesTheFilesystem", "fs", layerIntegration},
		{"stringvarprogram", "TestGitNamedByAPackageVariableIsGit", "git", layerIntegration},
		{"programfromacall", "TestAProgramNamedByACallIsUnresolved", "proc", layerIntegration},
		{"escapedalloc", "TestAProgramFilledThroughAPointerIsUnresolved", "proc", layerIntegration},
		{"interfacevarininit", "TestAClockAnInitFunctionBuiltReadsTheClock", "clock", layerIntegration},
		{"initreadsafile", "TestUsingWhatInitReadReachesTheRead", "fs", layerIntegration},
		{"openedatinit", "TestAMethodOfAVariableOpenedAtInitTouchesTheFilesystem", "fs", layerIntegration},
		{"funcmap", "TestAMapOfFuncsReachesEveryEntry", "git", layerIntegration},
		{"funcinstruct", "TestAFuncInAStructFieldIsReached", "fs", layerIntegration},
		{"installedhook", "TestAHookAHelperInstalledRunsGit", "git", layerIntegration},
		{"externalinterface", "TestAnHTTPClientBehindAnInterfaceTouchesTheNetwork", "network", layerIntegration},
		{"externalvarhandedon", "TestUsingAVariableAnInitHandsOnTouchesNothing", "-", layerUnit},
		{"environgit", "TestEnvGitIsGit", "git", layerIntegration},
		{"doltload", "TestLoadingADoltDatabaseTouchesTheStore", "store", layerIntegration},
	} {
		t.Run(c.pkg+"/"+c.test, func(t *testing.T) {
			s := classifyFixtures(t, g, l, [2]string{c.pkg, c.test})[0].tests[0]
			if got := s.touches.names(); got != c.touches {
				t.Errorf("touches = %s, want %s", got, c.touches)
			}
			if got := layerOf(s.touches); got != c.layer {
				t.Errorf("layer = %s, want %s", got, c.layer)
			}
		})
	}
}

func TestAWitnessChainIsTheCallsFromTheTestToTheTouch(t *testing.T) {
	g, l := loadFixtures(t)
	for _, c := range []struct {
		pkg, test string
		touch     touch
		chain     string
	}{
		// The store is reached through app.Store; the chain runs through
		// the constructor that built it, and stops at the marker.
		{"storethroughinterface", "TestTheConstructorCarriesTheStore", touchStore,
			"storethroughinterface.TestTheConstructorCarriesTheStore → internal/app.New → internal/store.Open"},
		// The program "git" is resolved through both helpers' parameters,
		// and named on the spawning call.
		{"gitthroughtwohelpers", "TestGitThroughTwoHelpersIsGit", touchGit,
			"gitthroughtwohelpers.TestGitThroughTwoHelpersIsGit → gitthroughtwohelpers.run → gitthroughtwohelpers.runIn → os/exec.Command (git)"},
		{"programconstant", "TestAnyOtherProgramIsAnotherProcess", touchProc,
			"programconstant.TestAnyOtherProgramIsAnotherProcess → os/exec.Command (make)"},
		// The variable's initializer resolves the program its constructor
		// carries into the closure it builds.
		{"callbuiltinitializer", "TestAHookBuiltByACallRunsGit", touchGit,
			"callbuiltinitializer.TestAHookBuiltByACallRunsGit → callbuiltinitializer.Run → callbuiltinitializer.hook → callbuiltinitializer.gitHook → callbuiltinitializer.gitHook$1 → os/exec.Command (git)"},
		// The hook's slice is the init function that calls install, which
		// resolves install's parameter.
		{"installedhook", "TestAHookAHelperInstalledRunsGit", touchGit,
			"installedhook.TestAHookAHelperInstalledRunsGit → installedhook.Run → installedhook.hook → installedhook.init#1 → installedhook.install → installedhook.install$1 → os/exec.Command (git)"},
		// db's slice calls openStore.
		{"openedatinit", "TestAMethodOfAVariableOpenedAtInitTouchesTheFilesystem", touchFS,
			"openedatinit.TestAMethodOfAVariableOpenedAtInitTouchesTheFilesystem → openedatinit.db → openedatinit.openStore → os.Open"},
		// No constant names the program where it enters the spawn.
		{"escapedalloc", "TestAProgramFilledThroughAPointerIsUnresolved", touchProc,
			"escapedalloc.TestAProgramFilledThroughAPointerIsUnresolved → os/exec.Command (program unresolved)"},
		{"programconstant", "TestASpawnerHandedOnAsAValueIsAnotherProcess", touchProc,
			"programconstant.TestASpawnerHandedOnAsAValueIsAnotherProcess → os/exec.Command (program unresolved)"},
	} {
		s := classifyFixtures(t, g, l, [2]string{c.pkg, c.test})[0].tests[0]
		w := s.touches[c.touch]
		if w == nil {
			t.Errorf("%s %s: no %s touch, want chain %s", c.pkg, c.test, c.touch, c.chain)
			continue
		}
		if got := w.render(fixtureModule); got != c.chain {
			t.Errorf("%s %s: chain = %s, want %s", c.pkg, c.test, got, c.chain)
		}
	}
}

func TestAPackagesInitializerAndTestMainTouchesAreItsRowsNotItsTests(t *testing.T) {
	g, l := loadFixtures(t)
	shapes := classifyFixtures(t, g, l,
		[2]string{"reexeccli", "TestReRunningTheBinaryRunsTheCLI"},
		[2]string{"helperprocess", "TestHelper"},
		[2]string{"network", "TestAListenerTouchesTheNetwork"},
		[2]string{"testmainchildstore", "TestAHookTestMainReplacesStillTouchesTheFilesystem"},
		[2]string{"initreadsafile", "TestUsingWhatInitReadReachesTheRead"},
	)
	// reexeccli's TestMain calls main; helperprocess's only runs the tests;
	// network has none; testmainchildstore's runs hook, which opens a file.
	// initreadsafile's initializer reads a file, and testmainchildstore's
	// takes the literal that opens one as hook's value, which makes it
	// reachable; the others' do nothing.
	for i, want := range []struct{ init, testMain string }{
		{"-", "cli"},
		{"-", "-"},
		{"-", "no TestMain"},
		{"fs", "fs"},
		{"fs", "no TestMain"},
	} {
		got := "no TestMain"
		if shapes[i].testMain != nil {
			got = shapes[i].testMain.names()
		}
		if got != want.testMain || shapes[i].init.names() != want.init {
			t.Errorf("%s: init %s and TestMain %s, want init %s and TestMain %s", shapes[i].importPath, shapes[i].init.names(), got, want.init, want.testMain)
		}
	}
}

// stream joins raw go test -json lines into one stream.
func stream(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// ended is the events of one top-level test of pkg, under the fixture
// module, that ended with verdict after elapsed seconds, and of its package.
func ended(pkg, test, verdict, elapsed string) []string {
	p := `"Package":"` + fixtureModule + "/" + pkg + `"`
	return []string{
		`{"Time":"2026-01-01T00:00:00Z","Action":"run",` + p + `,"Test":"` + test + `"}`,
		`{"Time":"2026-01-01T00:00:09Z","Action":"` + verdict + `",` + p + `,"Test":"` + test + `","Elapsed":` + elapsed + `}`,
		`{"Time":"2026-01-01T00:00:09Z","Action":"pass",` + p + `,"Elapsed":` + elapsed + `}`,
	}
}

func passing(pkg, test, elapsed string) []string { return ended(pkg, test, "pass", elapsed) }

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
	lines = append(lines, ended("skipped", "TestSkipsBeforeItTouchesTheFilesystem", "skip", "0")...)
	code, out, errOut := runFixture(config{dir: fixtureDir, moduleMarkers: moduleMarkers, listTests: true}, lines...)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	// One unit test of 0.5s, one integration test of 1.5s touching the
	// network, one end-to-end test of 2s running the CLI, and one skipped
	// test, which sits at no layer.
	total := []string{"TOTAL", "3", "1", "1", "0.5s", "1", "1.5s", "1", "2.0s", "33%", "1", "0", "0", "0", "0", "0", "1"}
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
		"skipped TestSkipsBeforeItTouchesTheFilesystem  skipped  0.00s\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
}

// refused asserts a run that exits non-zero, prints no report, and names
// want on stderr.
func refused(t *testing.T, cfg config, want string, lines ...string) {
	t.Helper()
	code, out, errOut := runFixture(cfg, lines...)
	if code == 0 || out != "" || !strings.Contains(errOut, want) {
		t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant a non-zero exit, no report, and stderr naming %q", code, out, errOut, want)
	}
}

func TestAMarkerThatNamesNoFunctionFailsTheRun(t *testing.T) {
	markers := append([]markerGroup{{"internal/store", touchStore, []string{"Gone"}}}, moduleMarkers...)
	refused(t, config{dir: fixtureDir, moduleMarkers: markers},
		"marker "+fixtureModule+"/internal/store.Gone names nothing package "+fixtureModule+"/internal/store declares",
		passing("network", "TestAListenerTouchesTheNetwork", "1")...)
}

func TestATestTheProgramDoesNotDeclareFailsTheRun(t *testing.T) {
	refused(t, config{dir: fixtureDir, moduleMarkers: moduleMarkers},
		fixtureModule+"/network ran TestNobodyWrote, and the loaded module declares no such function",
		passing("network", "TestNobodyWrote", "1")...)
}

func TestATestDeclaredInBothTestPackagesFailsTheRun(t *testing.T) {
	refused(t, config{dir: fixtureDir, moduleMarkers: moduleMarkers},
		fixtureModule+"/declaredtwice declares TestDeclaredTwice in both its package and its external test package",
		passing("declaredtwice", "TestDeclaredTwice", "1")...)
}

// TestEveryMarkerNamesADeclaredFunction holds this module to the run's own
// check: a renamed opener, or a dependency that dropped a function, would
// otherwise count no touch, and its tests would read as unit.
func TestEveryMarkerNamesADeclaredFunction(t *testing.T) {
	if _, err := resolveMarkers(filepath.Join("..", ".."), moduleMarkers); err != nil {
		t.Error(err)
	}
}
