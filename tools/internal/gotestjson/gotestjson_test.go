package gotestjson

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// read reads a stream built from raw lines.
func read(t *testing.T, lines ...string) Stream {
	t.Helper()
	s, err := Read(strings.NewReader(strings.Join(lines, "\n")+"\n"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestABuildFailureIsTheBuildGoTestNamed(t *testing.T) {
	s := read(t,
		`{"ImportPath":"p [p.test]","Action":"build-fail"}`,
		`{"Action":"start","Package":"p"}`,
		`{"Action":"fail","Package":"p","Elapsed":0,"FailedBuild":"p [p.test]"}`,
	)
	want := []Package{{ImportPath: "p", Failed: true, FailedBuild: "p [p.test]"}}
	if !reflect.DeepEqual(s.Packages, want) {
		t.Errorf("packages = %+v, want %+v", s.Packages, want)
	}
}

func TestAVerdictRecordsWhetherItFailedAndWhetherItCarriedElapsed(t *testing.T) {
	s := read(t,
		`{"Time":"2026-01-01T00:00:00Z","Action":"run","Package":"p","Test":"TestTimed"}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"fail","Package":"p","Test":"TestTimed","Elapsed":1}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"run","Package":"p","Test":"TestUntimed"}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"pass","Package":"p","Test":"TestUntimed"}`,
		`{"Action":"fail","Package":"p","Elapsed":1}`,
	)
	want := []Test{
		{Name: "TestTimed", Elapsed: time.Second, Failed: true},
		{Name: "TestUntimed", NoElapsed: true},
	}
	if got := s.Packages[0].Tests; !reflect.DeepEqual(got, want) {
		t.Errorf("tests = %+v, want %+v", got, want)
	}
}

func TestTestsRunningWhenTheStreamEndsAreCut(t *testing.T) {
	s := read(t,
		`{"Action":"run","Package":"b","Test":"TestB"}`,
		`{"Action":"run","Package":"a","Test":"TestA"}`,
		`{"Action":"run","Package":"a","Test":"TestA/sub"}`,
	)
	want := []TestID{{"a", "TestA"}, {"a", "TestA/sub"}, {"b", "TestB"}}
	if !reflect.DeepEqual(s.Cut, want) || s.Packages != nil {
		t.Errorf("cut = %+v and packages = %+v, want cut %+v and no packages", s.Cut, s.Packages, want)
	}
}

func TestAMainFailureIsAFailedBinaryWithNoTestToBlame(t *testing.T) {
	for _, c := range []struct {
		name string
		pkg  Package
		want bool
	}{
		{"a failed package with only passing tests", Package{Failed: true, Tests: []Test{{Name: "TestA"}}}, true},
		{"a failed package with no tests", Package{Failed: true}, true},
		{"a failed test", Package{Failed: true, Tests: []Test{{Name: "TestA", Failed: true}}}, false},
		{"an unfinished test", Package{Failed: true, Tests: []Test{{Name: "TestA", Unfinished: true}}}, false},
		{"a failed build", Package{Failed: true, FailedBuild: "p [p.test]"}, false},
		{"a passing package", Package{Tests: []Test{{Name: "TestA"}}}, false},
	} {
		if got := c.pkg.MainFailed(); got != c.want {
			t.Errorf("%s: MainFailed() = %v, want %v", c.name, got, c.want)
		}
	}
}
