package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stream builds a go test -json stream from raw lines.
func stream(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
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
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(results), len(want), results)
	}
	for i := range want {
		if results[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, results[i], want[i])
		}
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
