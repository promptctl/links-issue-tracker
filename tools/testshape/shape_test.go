package main

import (
	"slices"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

func TestTheLayerIsDecidedByTheTouches(t *testing.T) {
	for _, tc := range []struct {
		touches Touches
		want    Layer
	}{
		{0, Unit},
		{Touches(0).With(FS), Integration},
		{Touches(0).With(Clock), Integration},
		{Touches(0).With(Store).With(Git), Integration},
		{Touches(0).With(Proc), Integration},
		{Touches(0).With(CLI), EndToEnd},
		{Touches(0).With(CLI).With(FS).With(Net), EndToEnd},
	} {
		if got := layerOf(tc.touches); got != tc.want {
			t.Errorf("layerOf(%s) = %s, want %s", tc.touches, got, tc.want)
		}
	}
}

func TestJoinPutsDurationsOnlyOnTestsThatRan(t *testing.T) {
	fs := reached{FS: witness{chain: []string{"p.TestDisk"}, what: "os.ReadFile (fs)"}}
	shape := binaryShape{
		Tests: map[string]reached{"TestDisk": fs, "TestPure": {}, "TestSkipped": fs, "TestUnselected": {}},
		Order: []string{"TestDisk", "TestPure", "TestSkipped", "TestUnselected"},
	}
	pkg := gotestjson.Package{ImportPath: "p", Tests: []gotestjson.Test{
		{Name: "TestDisk", Elapsed: time.Second, Verdict: gotestjson.Fail},
		{Name: "TestPure", Elapsed: time.Millisecond, Verdict: gotestjson.Pass},
		{Name: "TestSkipped", Elapsed: 3 * time.Millisecond, Verdict: gotestjson.Skip},
	}}
	m, refusals := join(pkg, shape)
	if len(refusals) > 0 {
		t.Fatalf("refusals = %v", refusals)
	}
	type got struct {
		name    string
		layer   Layer
		ran     bool
		elapsed time.Duration
	}
	var gots []got
	for _, mt := range m.Tests {
		gots = append(gots, got{mt.Name, mt.Layer(), mt.Ran, mt.Elapsed})
	}
	want := []got{
		{"TestDisk", Integration, true, time.Second},
		{"TestPure", Unit, true, time.Millisecond},
		{"TestSkipped", Integration, false, 0},
		{"TestUnselected", Unit, false, 0},
	}
	if !slices.Equal(gots, want) {
		t.Errorf("joined %v, want %v", gots, want)
	}
}

func TestJoinRefusesWhatTheBinaryDoesNotDeclareAndWhatRanTwice(t *testing.T) {
	shape := binaryShape{Tests: map[string]reached{"TestA": {}}, Order: []string{"TestA"}}
	pkg := gotestjson.Package{ImportPath: "p", Tests: []gotestjson.Test{
		{Name: "TestA", Verdict: gotestjson.Pass},
		{Name: "TestA", Verdict: gotestjson.Pass},
		{Name: "TestGhost", Verdict: gotestjson.Pass},
	}}
	_, refusals := join(pkg, shape)
	want := []string{
		"p: the stream ran TestA twice",
		"p: the stream ran TestGhost, which its test binary does not declare",
	}
	if !slices.Equal(refusals, want) {
		t.Errorf("refusals = %q, want %q", refusals, want)
	}
}

func TestAStreamThatMeasuredNothingRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  gotestjson.Run
		want []string
	}{
		{"empty", gotestjson.Run{}, []string{"the stream is empty"}},
		{"a package twice", gotestjson.Run{Packages: []gotestjson.Package{
			{ImportPath: "p", Verdict: gotestjson.Pass}, {ImportPath: "p", Verdict: gotestjson.Pass},
		}}, []string{"p: the stream ran the package twice"}},
		{"a failed build", gotestjson.Run{Packages: []gotestjson.Package{
			{ImportPath: "p", Verdict: gotestjson.Fail, FailedBuild: "p [p.test]"},
		}}, []string{"p: its test binary failed to build (p [p.test])"}},
		{"a binary that died outside any test", gotestjson.Run{Packages: []gotestjson.Package{
			{ImportPath: "p", Verdict: gotestjson.Fail, Tests: []gotestjson.Test{{Name: "TestA", Verdict: gotestjson.Pass}}},
		}}, []string{"p: its test binary died outside any test"}},
		{"a test still running", gotestjson.Run{Packages: []gotestjson.Package{
			{ImportPath: "p", Verdict: gotestjson.Fail, Tests: []gotestjson.Test{{Name: "TestHang", Verdict: gotestjson.Unfinished}}},
		}}, []string{"p: the package ended with TestHang still running"}},
		{"a stream cut inside a package", gotestjson.Run{Cut: []gotestjson.Cut{
			{ImportPath: "p", Open: []string{"TestA"}}, {ImportPath: "q"},
		}}, []string{
			"p: the stream ended inside the package, with TestA still running",
			"q: the stream ended inside the package, with no test running",
		}},
		{"a failing test is a measure", gotestjson.Run{Packages: []gotestjson.Package{
			{ImportPath: "p", Verdict: gotestjson.Fail, Tests: []gotestjson.Test{{Name: "TestA", Verdict: gotestjson.Fail}}},
		}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamRefusals(tc.run); !slices.Equal(got, tc.want) {
				t.Errorf("refusals = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEveryModuleOfTheSuiteMustBeInTheStream(t *testing.T) {
	mods := []module{{Dir: "/r", Path: "example.com/r"}, {Dir: "/r/driver", Path: "example.com/r/driver"}}
	run := gotestjson.Run{Packages: []gotestjson.Package{
		{ImportPath: "example.com/r/cmd"},
		{ImportPath: "example.com/elsewhere"},
	}}
	want := []string{
		"example.com/elsewhere: the package belongs to no module of the suite",
		"module example.com/r/driver (/r/driver): its tests are absent from the stream",
	}
	if got := moduleRefusals(mods, run); !slices.Equal(got, want) {
		t.Errorf("refusals = %q, want %q", got, want)
	}
}

func TestANestedModuleOwnsItsPackages(t *testing.T) {
	mods := []module{{Path: "example.com/r"}, {Path: "example.com/r/driver"}}
	for path, want := range map[string]int{
		"example.com/r":                0,
		"example.com/r/x":              0,
		"example.com/r/driver":         1,
		"example.com/r/driver/sub":     1,
		"example.com/r/driver_test":    1,
		"example.com/r/x.test":         0,
		"example.com/rival":            -1,
		"example.com/r/driverless/pkg": 0,
	} {
		if got := moduleIndex(mods, path); got != want {
			t.Errorf("moduleIndex(%s) = %d, want %d", path, got, want)
		}
	}
}
