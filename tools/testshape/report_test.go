package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// These tests run the pure parts in process: no load, no fixture module.

func TestEveryTouchAndLayerHasItsOwnName(t *testing.T) {
	seen := make(map[string]bool)
	for i := range numTouches {
		if n := touch(i).String(); n == "" || seen[n] {
			t.Errorf("touch %d has name %q, which is empty or another's", i, n)
		}
		seen[touch(i).String()] = true
	}
	for i := range numLayers {
		if n := layer(i).String(); n == "" || seen[n] {
			t.Errorf("layer %d has name %q, which is empty or another's", i, n)
		}
		seen[layer(i).String()] = true
	}
}

// name is a vertex of a hand-built witness.
type name string

func (n name) String() string { return string(n) }

// touched is touches holding a one-step witness for each of ts.
func touched(ts ...touch) touches {
	var out touches
	for _, t := range ts {
		out[t] = &witness{path: []fmt.Stringer{name("example.com/m/p.TestX")}, tail: []string{"example.com/m/internal/q." + t.String()}}
	}
	return out
}

func TestTheLayerIsTheCLIThenAnyTouchThenNone(t *testing.T) {
	for _, c := range []struct {
		touches touches
		want    layer
	}{
		{touched(), layerUnit},
		{touched(touchClock), layerIntegration},
		{touched(touchFS, touchGit, touchStore), layerIntegration},
		{touched(touchCLI), layerE2E},
		{touched(touchNetwork, touchCLI), layerE2E},
	} {
		if got := layerOf(c.touches); got != c.want {
			t.Errorf("layerOf(%s) = %s, want %s", c.touches.names(), got, c.want)
		}
	}
}

func TestTheTableCountsLayeredTestsAndSkippedOnesApart(t *testing.T) {
	shapes := []packageShape{
		{
			importPath: "example.com/m/p",
			init:       touched(touchFS),
			tests: []testShape{
				{Test: gotestjson.Test{Name: "TestUnit", Elapsed: time.Second}},
				{Test: gotestjson.Test{Name: "TestClock", Elapsed: 2 * time.Second}, touches: touched(touchClock)},
				{Test: gotestjson.Test{Name: "TestSkip", Elapsed: time.Second, Verdict: gotestjson.Skip}},
			},
		},
		{
			importPath: "example.com/m",
			testMain:   &touches{},
			tests:      []testShape{{Test: gotestjson.Test{Name: "TestCLI", Elapsed: 4 * time.Second}, touches: touched(touchCLI, touchFS)}},
		},
	}
	var out strings.Builder
	printTable(&out, "example.com/m", shapes)
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		rows = append(rows, strings.Fields(line))
	}
	want := [][]string{
		{"p", "2", "1", "1", "1.0s", "1", "2.0s", "0", "0.0s", "50%", "0", "0", "1", "0", "0", "0", "0", "fs", "no", "TestMain"},
		// The module's root package keeps its path.
		{"example.com/m", "1", "0", "0", "0.0s", "0", "0.0s", "1", "4.0s", "0%", "0", "1", "0", "0", "0", "0", "1", "-", "-"},
		{"TOTAL", "3", "1", "1", "1.0s", "1", "2.0s", "1", "4.0s", "33%", "0", "1", "1", "0", "0", "0", "1"},
	}
	if len(rows) != len(want)+1 {
		t.Fatalf("got %d lines, want a header and %d rows:\n%s", len(rows), len(want), out.String())
	}
	for i, w := range want {
		if strings.Join(rows[i+1], " ") != strings.Join(w, " ") {
			t.Errorf("row %d = %v, want %v", i, rows[i+1], w)
		}
	}
}

func TestTheListingGivesEachTestItsLayerAndOneWitnessPerTouch(t *testing.T) {
	shapes := []packageShape{{
		importPath: "example.com/m/p",
		tests: []testShape{
			{Test: gotestjson.Test{Name: "TestX", Elapsed: 1500 * time.Millisecond, Verdict: gotestjson.Fail}, touches: touched(touchFS, touchGit)},
			{Test: gotestjson.Test{Name: "TestSkip", Verdict: gotestjson.Skip}},
		},
	}}
	var out strings.Builder
	printTests(&out, "example.com/m", shapes)
	want := "p TestX  integration  1.50s  fs,git  [failed]\n" +
		"    fs: p.TestX → internal/q.fs\n" +
		"    git: p.TestX → internal/q.git\n" +
		"p TestSkip  skipped  0.00s\n"
	if out.String() != want {
		t.Errorf("listing =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestAStreamThatMeasuredNothingFailsNamingWhy(t *testing.T) {
	pkg := `"Package":"p"`
	run := `{"Time":"2026-01-01T00:00:00Z","Action":"run",` + pkg + `,"Test":"TestA"}`
	pass := `{"Time":"2026-01-01T00:00:01Z","Action":"pass",` + pkg + `,"Test":"TestA","Elapsed":1}`
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"an empty stream", nil, "the stream ran no test"},
		{"a failed build", []string{
			`{"Action":"start",` + pkg + `}`,
			`{"Action":"fail",` + pkg + `,"Elapsed":0,"FailedBuild":"p [p.test]"}`,
		}, "p failed to build"},
		{"a binary that failed outside any test", []string{
			run, pass, `{"Time":"2026-01-01T00:00:01Z","Action":"fail",` + pkg + `,"Elapsed":1}`,
		}, "p's test binary failed outside any test"},
		{"a test unfinished when its package ended", []string{
			run, `{"Time":"2026-01-01T00:00:02Z","Action":"fail",` + pkg + `,"Elapsed":2}`,
		}, "p TestA has no verdict: its package ended before it did"},
		{"a test open when the stream ended", []string{run}, "p TestA was cut: the stream ended before its package did"},
		{"a test finished before the stream ended", []string{run, pass}, "p TestA was cut: the stream ended before its package did"},
		{"a verdict with no elapsed time", []string{
			run, `{"Time":"2026-01-01T00:00:01Z","Action":"pass",` + pkg + `,"Test":"TestA"}`,
			`{"Time":"2026-01-01T00:00:01Z","Action":"pass",` + pkg + `,"Elapsed":1}`,
		}, "p TestA has a verdict with no elapsed time"},
		{"a test run twice", []string{
			run, pass, run, pass, `{"Time":"2026-01-01T00:00:01Z","Action":"pass",` + pkg + `,"Elapsed":1}`,
		}, "p ran TestA more than once"},
		{"a package action go test does not emit", []string{`{"Action":"explode",` + pkg + `}`}, `"explode" is no package action go test emits`},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A refused stream fails before the module is loaded: dir names
			// no module, so a run that got as far as loading would fail on it
			// instead.
			code, out, errOut := runFixture(config{dir: "no-module-here"}, c.lines...)
			if code == 0 || out != "" || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant a non-zero exit, no report, and stderr naming %q", code, out, errOut, c.want)
			}
		})
	}
}
