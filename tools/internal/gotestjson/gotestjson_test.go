package gotestjson

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// stream builds a go test -json stream from raw lines.
func stream(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

func read(t *testing.T, lines ...string) (Run, string) {
	t.Helper()
	var out strings.Builder
	run, err := Read(stream(lines...), &out)
	if err != nil {
		t.Fatal(err)
	}
	return run, out.String()
}

func TestReadStampsTerminalPackageResults(t *testing.T) {
	run, _ := read(t,
		`{"Action":"start","Package":"example.com/a"}`,
		`{"Action":"pass","Package":"example.com/a","Elapsed":1.5}`,
		`{"Action":"fail","Package":"example.com/b","Elapsed":0.2,"FailedBuild":"example.com/b [example.com/b.test]"}`,
		`{"Action":"skip","Package":"example.com/c","Elapsed":0}`,
	)
	want := Run{Packages: []Package{
		{ImportPath: "example.com/a", Elapsed: 1500 * time.Millisecond, Verdict: Pass},
		{ImportPath: "example.com/b", Elapsed: 200 * time.Millisecond, Verdict: Fail, FailedBuild: "example.com/b [example.com/b.test]"},
		{ImportPath: "example.com/c", Verdict: Skip},
	}}
	if !reflect.DeepEqual(run, want) {
		t.Errorf("run = %+v, want %+v", run, want)
	}
}

// A real stream's lines for one package whose test sets an attribute and
// records an artifact directory, in the shape test2json emits for t.Attr and
// t.ArtifactDir.
func TestReadAcceptsAttrAndArtifactsEvents(t *testing.T) {
	run, _ := read(t,
		`{"Time":"2026-11-01T10:00:00Z","Action":"start","Package":"p"}`,
		`{"Time":"2026-11-01T10:00:00Z","Action":"run","Package":"p","Test":"TestA"}`,
		`{"Time":"2026-11-01T10:00:00.1Z","Action":"attr","Package":"p","Test":"TestA","Key":"owner","Value":"store"}`,
		`{"Time":"2026-11-01T10:00:00.2Z","Action":"artifacts","Package":"p","Test":"TestA","Path":"/tmp/a"}`,
		`{"Time":"2026-11-01T10:00:00.3Z","Action":"bench","Package":"p","Test":"TestA","Output":"x\n"}`,
		`{"Time":"2026-11-01T10:00:00.5Z","Action":"pass","Package":"p","Test":"TestA","Elapsed":0.5}`,
		`{"Time":"2026-11-01T10:00:00.6Z","Action":"pass","Package":"p","Elapsed":0.6}`,
	)
	want := []Test{{Name: "TestA", Elapsed: 500 * time.Millisecond, Verdict: Pass}}
	if len(run.Packages) != 1 || !reflect.DeepEqual(run.Packages[0].Tests, want) {
		t.Errorf("packages = %+v, want one with tests %+v", run.Packages, want)
	}
}

func TestReadRefusesMalformedStreams(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
	}{
		{"unknown action", `{"Action":"explode","Package":"p"}`, `action "explode"`},
		{"package verdict without Elapsed", `{"Action":"pass","Package":"p"}`, "package p's pass has no Elapsed"},
		{"test verdict without Elapsed", `{"Action":"fail","Package":"p","Test":"TestX"}`, "test p TestX has no Elapsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Read(stream(tc.line), &strings.Builder{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Read(%s) error = %v, want one containing %q", tc.line, err, tc.want)
			}
		})
	}
}

func TestReadMarksATestWithNoVerdictAtItsPackagesEndUnfinished(t *testing.T) {
	run, out := read(t,
		`{"Time":"2026-11-01T10:00:00Z","Action":"run","Package":"p","Test":"TestHangs"}`,
		`{"Time":"2026-11-01T10:00:00Z","Action":"output","Package":"p","Test":"TestHangs","Output":"panic: test timed out\n"}`,
		`{"Time":"2026-11-01T10:00:02Z","Action":"fail","Package":"p","Elapsed":2}`,
	)
	want := []Test{{Name: "TestHangs", Elapsed: 2 * time.Second, Verdict: Unfinished}}
	if !reflect.DeepEqual(run.Packages[0].Tests, want) {
		t.Errorf("tests = %+v, want %+v", run.Packages[0].Tests, want)
	}
	if !strings.Contains(out, "--- unfinished: p TestHangs (no verdict before its package ended)\npanic: test timed out") {
		t.Errorf("replay lacks the hung test's output under its header:\n%s", out)
	}
}

func TestReadReportsPackagesTheStreamEndedInside(t *testing.T) {
	run, out := read(t,
		`{"Action":"start","Package":"q"}`,
		`{"Action":"output","Package":"p","Test":"TestDies","Output":"    about to crash\n"}`,
		`{"Action":"output","Package":"a","Test":"TestAlsoDies","Output":"    sibling detail\n"}`,
	)
	want := []Cut{
		{ImportPath: "a", Open: []string{"TestAlsoDies"}},
		{ImportPath: "p", Open: []string{"TestDies"}},
		{ImportPath: "q"},
	}
	if !reflect.DeepEqual(run.Cut, want) {
		t.Errorf("cut = %+v, want %+v", run.Cut, want)
	}
	if a, p := strings.Index(out, "sibling detail"), strings.Index(out, "about to crash"); a < 0 || p < 0 || a > p {
		t.Errorf("stranded output missing or not in package order:\n%s", out)
	}
}

func TestReadKeepsEachRunOfATestTheStreamRanTwice(t *testing.T) {
	run, _ := read(t,
		`{"Action":"run","Package":"p","Test":"TestA"}`,
		`{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0}`,
		`{"Action":"run","Package":"p","Test":"TestA"}`,
		`{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0}`,
		`{"Action":"pass","Package":"p","Elapsed":0}`,
	)
	if got := len(run.Packages[0].Tests); got != 2 {
		t.Errorf("got %d runs of TestA, want 2", got)
	}
}

func TestReadReplaysOutputOnlyForFailingTests(t *testing.T) {
	_, got := read(t,
		`{"Action":"output","Package":"p","Test":"TestGood","Output":"good detail\n"}`,
		`{"Action":"pass","Package":"p","Test":"TestGood","Elapsed":0.1}`,
		`{"Action":"output","Package":"p","Test":"TestBad","Output":"--- FAIL: TestBad\n"}`,
		`{"Action":"output","Package":"p","Test":"TestBad","Output":"    boom\n"}`,
		`{"Action":"fail","Package":"p","Test":"TestBad","Elapsed":0.1}`,
		`{"Action":"output","Package":"p","Output":"FAIL\tp\t0.2s\n"}`,
		`{"Action":"fail","Package":"p","Elapsed":0.2}`,
	)
	for _, want := range []string{"--- FAIL: TestBad", "    boom", "FAIL\tp\t0.2s"} {
		if !strings.Contains(got, want) {
			t.Errorf("replayed output missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "good detail") {
		t.Errorf("passing test's output should be discarded; got:\n%s", got)
	}
}

func TestReadEchoesNonJSONAndBuildOutput(t *testing.T) {
	_, got := read(t,
		`panic: something terrible [recovered]`,
		`{"ImportPath":"p [p.test]","Action":"build-output","Output":"p: compile error\n"}`,
		`{"ImportPath":"p [p.test]","Action":"build-fail"}`,
	)
	for _, want := range []string{"panic: something terrible", "compile error"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

func TestReadTimesAParentOfParallelSubtestsToItsLastSubtestsEnd(t *testing.T) {
	// go test's Elapsed for the parent is 0: it sums before releasing its
	// parallel subtests. The subtest resumes at 0.1s and runs 0.3s, so the
	// parent ends at 0.4s.
	run, _ := read(t,
		`{"Time":"2026-11-01T10:00:00Z","Action":"run","Package":"p","Test":"TestParent"}`,
		`{"Time":"2026-11-01T10:00:00Z","Action":"run","Package":"p","Test":"TestParent/a"}`,
		`{"Time":"2026-11-01T10:00:00Z","Action":"pause","Package":"p","Test":"TestParent/a"}`,
		`{"Time":"2026-11-01T10:00:00.1Z","Action":"cont","Package":"p","Test":"TestParent/a"}`,
		`{"Time":"2026-11-01T10:00:01Z","Action":"pass","Package":"p","Test":"TestParent/a","Elapsed":0.3}`,
		`{"Time":"2026-11-01T10:00:01Z","Action":"pass","Package":"p","Test":"TestParent","Elapsed":0}`,
		`{"Time":"2026-11-01T10:00:01Z","Action":"pass","Package":"p","Elapsed":1}`,
	)
	want := []Test{{Name: "TestParent", Elapsed: 400 * time.Millisecond, Verdict: Pass}}
	if !reflect.DeepEqual(run.Packages[0].Tests, want) {
		t.Errorf("tests = %+v, want %+v", run.Packages[0].Tests, want)
	}
}
