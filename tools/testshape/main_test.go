package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// shapeOf classifies one test of the fixture module under testdata/src/shape.
// Each fixture test's name states what it touches, so the expectations below
// come from the fixture's design, never from this tool's output.
func shapeOf(t *testing.T, a *analyzer, test string) reach {
	t.Helper()
	f, err := a.testFunc("example.com/shape/internal/widget", test)
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.reachOf(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func set(ts ...touch) touchSet {
	var s touchSet
	for _, t := range ts {
		s = s.with(t)
	}
	return s
}

func TestEachTestIsClassifiedByWhatItsCodeReaches(t *testing.T) {
	a := newAnalyzer(os.DirFS(filepath.Join("testdata", "src", "shape")), "example.com/shape")
	for _, tc := range []struct {
		test    string
		touches touchSet
		layer   layer
	}{
		{"TestOnlyComputes", set(), unit},
		{"TestSpawnsGit", set(git), integration},
		{"TestCallsAHelperThatRunsTheCLI", set(cli), e2e},
		{"TestSleeps", set(clockWait), integration},
		{"TestTimesOutAgainstTheClock", set(clockWait), integration},
		{"TestReadsTheClock", set(clockRead), unit},
		{"TestWritesATempFile", set(fs), unit},
		{"TestOpensAStore", set(store), integration},
		{"TestRunsGitThroughAHelperNamingIt", set(git), integration},
		{"TestRunsAnotherProgram", set(process), integration},
		{"TestReexecsItself", set(cli), e2e},
		{"TestReachesGitThroughProductionCode", set(git), integration},
		{"TestReachesGitThroughAnotherPackage", set(git), integration},
		{"TestReachesGitThroughAMethod", set(git), integration},
		{"TestCallsTheSeamThatRunsGit", set(git), integration},
		{"TestReplacesTheSeamThatRunsGit", set(), unit},
		{"TestReadsAFieldNamedLikeAMethodThatRunsGit", set(), unit},
		{"TestCallsCodeWithALocalNamedLikeAHelperThatRunsTheCLI", set(), unit},
		{"TestRunsGitInASubtest", set(git), integration},
		{"TestFromOutsideReachesGitThroughProductionCode", set(git), integration},
	} {
		t.Run(tc.test, func(t *testing.T) {
			r := shapeOf(t, a, tc.test)
			if r.touches != tc.touches {
				t.Errorf("touches = [%s], want [%s]", r.touches, tc.touches)
			}
			if got := layerOf(r.touches); got != tc.layer {
				t.Errorf("layer = %s, want %s", got, tc.layer)
			}
		})
	}
}

func TestATouchTwoCallsDeepIsWitnessedByItsChain(t *testing.T) {
	a := newAnalyzer(os.DirFS(filepath.Join("testdata", "src", "shape")), "example.com/shape")
	r := shapeOf(t, a, "TestCallsAHelperThatRunsTheCLI")
	want := []string{
		"internal/widget.TestCallsAHelperThatRunsTheCLI",
		"internal/widget.runInDir",
		"internal/widget.runCLI",
		"./internal/cli.Run",
	}
	if got := r.witness[cli]; !reflect.DeepEqual(got, want) {
		t.Errorf("cli witness = %q, want %q", got, want)
	}
}

func TestATestTheStreamRanButTheSourceLacksIsNotGuessed(t *testing.T) {
	a := newAnalyzer(os.DirFS(filepath.Join("testdata", "src", "shape")), "example.com/shape")
	shapes, unread := classify(a, []timedTest{
		{Package: "example.com/shape/internal/widget", Name: "TestOnlyComputes", Verdict: "pass"},
		{Package: "example.com/shape/internal/widget", Name: "TestNobodyWrote", Verdict: "pass"},
	})
	if len(shapes) != 1 || shapes[0].Name != "TestOnlyComputes" {
		t.Errorf("classified %v, want only TestOnlyComputes", shapes)
	}
	if len(unread) != 1 || !strings.Contains(unread[0].Error(), "TestNobodyWrote") {
		t.Errorf("unread = %v, want one error naming TestNobodyWrote", unread)
	}
}

// timed classifies the stream testdata/src/record.sh recorded from a real go
// test -json run of testdata/src/timed against that package's source.
func timed(t *testing.T) []testShape {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "timed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tests, err := readStream(f, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	shapes, unread := classify(newAnalyzer(os.DirFS(filepath.Join("testdata", "src", "timed")), "example.com/timed"), tests)
	if len(unread) > 0 {
		t.Fatal(unread)
	}
	return shapes
}

func TestTheTableCountsAndTimesEachLayer(t *testing.T) {
	// timed: six top-level tests. Three wait on the clock (integration):
	// 0.20s, two parallel 0.15s subtests summed to 0.30s, and a 0.10s subtest
	// nested two deep counted once, 0.60s in all. Three are unit: one
	// computes, one writes a temp file (fs, 0.01s), one skips.
	var out strings.Builder
	printTable(&out, "example.com/timed", timed(t))
	want := "testshape: top-level tests per layer, by what each reaches (example.com/timed)\n" +
		"package  tests  unit  time  integration  time  e2e  time  unit%  cli  store  git  proc  fs  clock-wait  clock-read  skip\n" +
		".            6     3  0.0s            3  0.6s    0  0.0s    50%    0      0    0     0   1           3           0     1\n" +
		"TOTAL        6     3  0.0s            3  0.6s    0  0.0s    50%    0      0    0     0   1           3           0     1\n"
	if got := out.String(); got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
}

func TestTheListingGivesEachTestItsLayerTimeAndEvidence(t *testing.T) {
	var out strings.Builder
	printTests(&out, "example.com/timed", timed(t))
	want := "unit            0.00s  pass        .  TestOnlyComputes  []\n" +
		"integration     0.10s  pass        .  TestParentOfANestedSubtestThatSleeps100ms  [clock-wait]\n" +
		"    clock-wait: TestParentOfANestedSubtestThatSleeps100ms > time.Sleep\n" +
		"integration     0.30s  pass        .  TestParentOfTwoParallelSubtestsThatSleep150ms  [clock-wait]\n" +
		"    clock-wait: TestParentOfTwoParallelSubtestsThatSleep150ms > time.Sleep\n" +
		"unit            0.00s  skip        .  TestSkips  []\n" +
		"integration     0.20s  pass        .  TestSleeps200ms  [clock-wait]\n" +
		"    clock-wait: TestSleeps200ms > time.Sleep\n" +
		"unit            0.01s  pass        .  TestWritesATempFile  [fs]\n" +
		"    fs: TestWritesATempFile > os.WriteFile\n" +
		"\n"
	if got := out.String(); got != want {
		t.Errorf("listing:\n%s\nwant:\n%s", got, want)
	}
}
