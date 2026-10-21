package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stream builds a go test -json stream from raw lines.
func stream(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// recorded opens a stream that testdata/src/record.sh recorded from a real
// go test -json run of the package of the same name under testdata/src. Each
// test's name states its designed timing, so the expectations below come from
// the package's design, not from this tool's output.
func recorded(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// report consumes streams and renders every package's violation against a
// budget every package blows, as main writes them to stderr.
func report(t *testing.T, streams ...io.Reader) string {
	t.Helper()
	results, err := consume(io.MultiReader(streams...), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, v := range enforce(results, nil, time.Second) {
		writeViolation(&out, "", v)
	}
	return out.String()
}

func TestOverBudgetPackageListsItsTenSlowestTopLevelTests(t *testing.T) {
	// slow: eleven top-level tests, so the eleventh slowest (TestSerial10ms)
	// is left off, and a 400ms subtest that would outrank TestSerial40ms if
	// subtests were listed.
	got := report(t, recorded(t, "slow"))
	want := "test runtime budget exceeded: example.com/slowpkg took 2.2s, budget is 1s (over by 1.2s, +122%) — see tools/testbudget/budgets.go before raising the number\n" +
		"  slowest top-level tests, 10 of 11:\n" +
		"      0.60s  parallel  TestParallel600ms\n" +
		"      0.52s  serial    TestSerial520ms\n" +
		"      0.44s  parallel  TestParallel440ms\n" +
		"      0.40s  serial    TestSerialParentOfA400msSubtest\n" +
		"      0.36s  serial    TestSerial360ms\n" +
		"      0.28s  parallel  TestParallel280ms\n" +
		"      0.20s  serial    TestSerial200ms\n" +
		"      0.12s  parallel  TestParallel120ms\n" +
		"      0.08s  serial    TestSerial80ms\n" +
		"      0.04s  serial    TestSerial40ms\n"
	if got != want {
		t.Errorf("violation report:\n%s\nwant:\n%s", got, want)
	}
}

func TestTimedOutPackageListsItsHungTestsAsUnfinished(t *testing.T) {
	// timeout-serial, under -timeout 2s: TestSerialHangs hangs in a subtest
	// and never gets a verdict — go test emits only the package fail. The
	// parallel test is still paused when the timeout fires, so its whole
	// duration is the 300ms before t.Parallel. Hung elapsed = package fail
	// Time 21:11:10.477267 - run Time 21:11:08.874787 = 1.60s.
	got := report(t, recorded(t, "timeout-serial"))
	want := "  slowest top-level tests, 3 of 3:\n" +
		"      1.60s  serial    TestSerialHangs  [unfinished]\n" +
		"      0.30s  parallel  TestParallel300msBeforePauseNeverResumed  [unfinished]\n" +
		"      0.10s  serial    TestSerial100ms\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("violation report:\n%s\nwant it to end:\n%s", got, want)
	}
}

func TestUnfinishedParallelTestCountsTimeBeforeItsPauseAndSinceItsResume(t *testing.T) {
	// timeout-parallel: run 10.652058 → pause 10.853367 (0.20s), then
	// cont 10.953990 → package fail 12.656610 (1.70s): 1.90s, not the
	// 0.20s or 1.70s of either half, nor the 2.00s from run to fail.
	got := report(t, recorded(t, "timeout-parallel"))
	want := "  slowest top-level tests, 2 of 2:\n" +
		"      1.90s  parallel  TestParallel200msBeforePauseThenHangs  [unfinished]\n" +
		"      0.10s  serial    TestSerial100ms\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("violation report:\n%s\nwant it to end:\n%s", got, want)
	}
}

func TestHungTestOutputPrintsWhenItsPackageEnds(t *testing.T) {
	// The -timeout panic is output of the hung test, which never gets a
	// verdict. It must print as its package ends, not after every package
	// that finishes later in a concurrent go test ./... stream.
	var out strings.Builder
	if _, err := consume(io.MultiReader(recorded(t, "timeout-serial"), recorded(t, "slow")), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	panicAt := strings.Index(got, "panic: test timed out after 2s")
	header := "--- unfinished: example.com/hungpkg TestSerialHangs/blocks (no verdict before its package ended)"
	headerAt := strings.Index(got, header)
	laterPackageAt := strings.Index(got, "ok  \texample.com/slowpkg")
	if panicAt < 0 || headerAt < 0 || laterPackageAt < 0 {
		t.Fatalf("output missing the panic, %q, or the later package's ok line:\n%s", header, got)
	}
	if !(headerAt < panicAt && panicAt < laterPackageAt) {
		t.Errorf("hung test's output not printed under its header before the later package's result:\n%s", got)
	}
}

func TestPackageWithinBudgetHasNoViolation(t *testing.T) {
	results, err := consume(recorded(t, "slow"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if viols := enforce(results, nil, 3*time.Second); len(viols) != 0 {
		t.Errorf("2.2s package against a 3s budget: got violations %+v", viols)
	}
}

func TestConsumeStampsTerminalPackageResults(t *testing.T) {
	var out strings.Builder
	results, err := consume(stream(
		`{"Action":"start","Package":"example.com/a"}`,
		`{"Action":"pass","Package":"example.com/a","Elapsed":1.5}`,
		`{"Action":"fail","Package":"example.com/b","Elapsed":0.2}`,
		`{"Action":"skip","Package":"example.com/c"}`,
	), &out)
	if err != nil {
		t.Fatal(err)
	}
	want := []packageResult{
		{ImportPath: "example.com/a", Elapsed: 1500 * time.Millisecond},
		{ImportPath: "example.com/b", Elapsed: 200 * time.Millisecond, Failed: true},
		{ImportPath: "example.com/c"}, // no Elapsed in the event means zero
	}
	if !reflect.DeepEqual(results, want) {
		t.Errorf("results = %+v, want %+v", results, want)
	}
}

func TestConsumeReplaysOutputOnlyForFailingTests(t *testing.T) {
	var out strings.Builder
	_, err := consume(stream(
		`{"Action":"output","Package":"p","Test":"TestGood","Output":"good detail\n"}`,
		`{"Action":"pass","Package":"p","Test":"TestGood","Elapsed":0.1}`,
		`{"Action":"output","Package":"p","Test":"TestBad","Output":"--- FAIL: TestBad\n"}`,
		`{"Action":"output","Package":"p","Test":"TestBad","Output":"    boom\n"}`,
		`{"Action":"fail","Package":"p","Test":"TestBad","Elapsed":0.1}`,
		`{"Action":"output","Package":"p","Output":"FAIL\tp\t0.2s\n"}`,
		`{"Action":"fail","Package":"p","Elapsed":0.2}`,
	), &out)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"--- FAIL: TestBad", "    boom", "FAIL\tp\t0.2s"} {
		if !strings.Contains(got, want) {
			t.Errorf("replayed output missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "good detail") {
		t.Errorf("passing test's output should be discarded; got:\n%s", got)
	}
}

func TestConsumeEchoesNonJSONAndBuildOutput(t *testing.T) {
	var out strings.Builder
	_, err := consume(stream(
		`panic: something terrible [recovered]`,
		`{"Action":"build-output","Package":"p","Output":"p: compile error\n"}`,
	), &out)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"panic: something terrible", "compile error"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

func TestConsumeFlushesStrandedOutputWhenStreamEndsMidTest(t *testing.T) {
	// A binary that dies mid-test (os.Exit, a hard signal) never emits its
	// running tests' fail events; their buffered output must surface at end
	// of stream instead of being silently dropped.
	var out strings.Builder
	_, err := consume(stream(
		`{"Action":"output","Package":"p","Test":"TestDies","Output":"    about to crash\n"}`,
		`{"Action":"output","Package":"a","Test":"TestAlsoDies","Output":"    sibling detail\n"}`,
	), &out)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"unfinished", "about to crash", "sibling detail"} {
		if !strings.Contains(got, want) {
			t.Errorf("stranded output missing %q; got:\n%s", want, got)
		}
	}
	// Deterministic order: package "a" before package "p".
	if strings.Index(got, "sibling detail") > strings.Index(got, "about to crash") {
		t.Errorf("stranded output not in deterministic package order; got:\n%s", got)
	}
}

func TestPrintTableRendersSlowestFirstWithBudgetsAndFailureMarks(t *testing.T) {
	var out strings.Builder
	printTable(&out, []packageResult{
		{ImportPath: "example.com/fast", Elapsed: 1200 * time.Millisecond},
		{ImportPath: "example.com/slow", Elapsed: 90 * time.Second},
		{ImportPath: "example.com/red", Elapsed: 3 * time.Second, Failed: true},
	}, map[string]time.Duration{"example.com/slow": 120 * time.Second}, 30*time.Second)
	got := out.String()
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 4 { // header + three rows
		t.Fatalf("got %d lines, want 4:\n%s", len(lines), got)
	}
	wantRows := []string{
		"    90.0s /  120s  example.com/slow",
		"     3.0s /   30s  example.com/red  [FAILED — not a calibration point]",
		"     1.2s /   30s  example.com/fast",
	}
	for i, want := range wantRows {
		if lines[i+1] != want {
			t.Errorf("row %d = %q, want %q", i, lines[i+1], want)
		}
	}
}

func TestEnforceFlagsOnlyPackagesOverTheirBudget(t *testing.T) {
	b := map[string]time.Duration{"listed/slow": 10 * time.Second}
	viols := enforce([]packageResult{
		{ImportPath: "listed/slow", Elapsed: 12 * time.Second},
		{ImportPath: "listed/slow2", Elapsed: 9 * time.Second}, // unlisted: default 5s
		{ImportPath: "fast", Elapsed: time.Second},
	}, b, 5*time.Second)
	if len(viols) != 2 {
		t.Fatalf("got %d violations, want 2: %+v", len(viols), viols)
	}
	if viols[0].ImportPath != "listed/slow" || viols[0].Budget != 10*time.Second {
		t.Errorf("listed package violation wrong: %+v", viols[0])
	}
	if viols[1].ImportPath != "listed/slow2" || viols[1].Budget != 5*time.Second {
		t.Errorf("unlisted package must be held to the default budget: %+v", viols[1])
	}
}

func TestViolationLineNamesPackageOverageAndBudgetHome(t *testing.T) {
	line := violationLine(violation{
		ImportPath: "example.com/store",
		Elapsed:    330 * time.Second,
		Budget:     300 * time.Second,
	})
	for _, want := range []string{
		"example.com/store",
		"330.0s",
		"300s",
		"over by 30.0s",
		"+10%",
		"tools/testbudget/budgets.go",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("violation line missing %q: %s", want, line)
		}
	}
}

func TestEveryBudgetedPackageExists(t *testing.T) {
	// A budget for a package that moved or was deleted is a map with no
	// territory: it would silently enforce nothing. The import paths in
	// budgets.go must resolve in this module.
	// [LAW:one-source-of-truth]
	for path := range budgets {
		rel := strings.TrimPrefix(path, "github.com/promptctl/links-issue-tracker/")
		if rel == path {
			t.Errorf("budget key %q is outside this module", path)
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(rel))); err != nil {
			t.Errorf("budget key %q has no package directory: %v", path, err)
		}
	}
}
