package gotestjson

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// lines joins raw stream lines into one stream.
func lines(ls ...string) io.Reader {
	return strings.NewReader(strings.Join(ls, "\n") + "\n")
}

// read reads a stream built from raw lines.
func read(t *testing.T, ls ...string) Stream {
	t.Helper()
	s, err := Read(lines(ls...), io.Discard)
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

func TestAPackageEndWithNoElapsedSaysSo(t *testing.T) {
	s := read(t,
		`{"Action":"pass","Package":"a","Elapsed":0}`,
		`{"Action":"skip","Package":"b"}`,
	)
	want := []Package{{ImportPath: "a"}, {ImportPath: "b", NoElapsed: true}}
	if !reflect.DeepEqual(s.Packages, want) {
		t.Errorf("packages = %+v, want %+v", s.Packages, want)
	}
}

func TestAVerdictRecordsHowTheTestEndedAndWhetherItCarriedElapsed(t *testing.T) {
	s := read(t,
		`{"Time":"2026-01-01T00:00:00Z","Action":"run","Package":"p","Test":"TestFailed"}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"fail","Package":"p","Test":"TestFailed","Elapsed":1}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"run","Package":"p","Test":"TestSkipped"}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"skip","Package":"p","Test":"TestSkipped","Elapsed":0}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"run","Package":"p","Test":"TestUntimed"}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"pass","Package":"p","Test":"TestUntimed"}`,
		`{"Time":"2026-01-01T00:00:01Z","Action":"run","Package":"p","Test":"TestHangs"}`,
		`{"Time":"2026-01-01T00:00:03Z","Action":"fail","Package":"p","Elapsed":3}`,
	)
	want := []Test{
		{Name: "TestFailed", Elapsed: time.Second, Verdict: Fail},
		{Name: "TestSkipped", Verdict: Skip},
		{Name: "TestUntimed", Verdict: Pass, NoElapsed: true},
		{Name: "TestHangs", Elapsed: 2 * time.Second, Verdict: Unfinished},
	}
	if got := s.Packages[0].Tests; !reflect.DeepEqual(got, want) {
		t.Errorf("tests = %+v, want %+v", got, want)
	}
}

func TestTheTestsOfAPackageTheStreamNeverEndedAreCut(t *testing.T) {
	s := read(t,
		`{"Action":"run","Package":"b","Test":"TestB"}`,
		`{"Action":"run","Package":"a","Test":"TestA"}`,
		`{"Action":"run","Package":"a","Test":"TestA/sub"}`,
		`{"Action":"run","Package":"a","Test":"TestDone"}`,
		`{"Action":"pass","Package":"a","Test":"TestDone","Elapsed":0}`,
	)
	// TestDone reached its verdict, and still no package result lists it.
	want := []TestID{{"a", "TestA"}, {"a", "TestA/sub"}, {"a", "TestDone"}, {"b", "TestB"}}
	if !reflect.DeepEqual(s.Cut, want) || s.Packages != nil {
		t.Errorf("cut = %+v and packages = %+v, want cut %+v and no packages", s.Cut, s.Packages, want)
	}
}

func TestAPackageActionGoTestDoesNotEmitIsAnError(t *testing.T) {
	_, err := Read(lines(`{"Action":"explode","Package":"p"}`), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"explode"`) {
		t.Errorf("Read() error = %v, want one naming the action", err)
	}
}

// failingWriter refuses every write.
type failingWriter struct{}

var errRefused = errors.New("refused")

func (failingWriter) Write([]byte) (int, error) { return 0, errRefused }

func TestAReplayThatCannotWriteIsAnError(t *testing.T) {
	_, err := Read(lines(`{"Action":"output","Package":"p","Output":"ok  \tp\n"}`), failingWriter{})
	if !errors.Is(err, errRefused) {
		t.Errorf("Read() error = %v, want the replay's write error", err)
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
		{"a failed test", Package{Failed: true, Tests: []Test{{Name: "TestA", Verdict: Fail}}}, false},
		{"an unfinished test", Package{Failed: true, Tests: []Test{{Name: "TestA", Verdict: Unfinished}}}, false},
		{"a failed build", Package{Failed: true, FailedBuild: "p [p.test]"}, false},
		{"a passing package", Package{Tests: []Test{{Name: "TestA"}}}, false},
	} {
		if got := c.pkg.MainFailed(); got != c.want {
			t.Errorf("%s: MainFailed() = %v, want %v", c.name, got, c.want)
		}
	}
}
