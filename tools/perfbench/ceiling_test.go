package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store/designstore"
)

// `--sizes design` measures the store the ceiling is derived to hold, at the
// row count the ceiling reads, so a caller never writes that count down a
// second time; and naming it twice, once by count, is the duplicate it is.
func TestDesignSizeIsTheCeilingsRowCount(t *testing.T) {
	got, err := parseSizes("118,design")
	if err != nil {
		t.Fatalf("parseSizes(118,design) error = %v", err)
	}
	if len(got) != 2 || got[1].rows != designstore.Rows || got[1].name != "design" {
		t.Fatalf("parseSizes(118,design) = %v, want 118 and design at %d rows", got, designstore.Rows)
	}
	if _, err := parseSizes("design," + strconv.Itoa(designstore.Rows)); err == nil {
		t.Error("parseSizes accepted the design size twice, once by name and once by count")
	}
}

// designResult is a design-size store measured at the given figures.
func designResult(compacted, perCommit int64) result {
	return result{
		size:      designSize,
		compacted: compacted,
		history:   historyCost{edits: historyEdits, before: compacted, after: compacted + perCommit*historyEdits},
	}
}

// A design-size store is judged on each input against its own allowance: at
// the allowance it passes, past it the breach names the input and the figure.
// Any other size is not judged at all, however heavy, because the allowances
// are facts about the design size alone.
func TestInputBreachesJudgesEachInputAtTheDesignSizeOnly(t *testing.T) {
	if got := inputBreaches(designResult(designstore.CompactedBytesAllowance, designstore.CommitBytesAllowance)); len(got) != 0 {
		t.Errorf("a store at both allowances breached: %v", got)
	}

	rows := designResult(designstore.CompactedBytesAllowance+1e6, designstore.CommitBytesAllowance)
	got := inputBreaches(rows)
	if len(got) != 1 || got[0].input != "compacted bytes" {
		t.Fatalf("over on compacted bytes alone: breaches = %v, want compacted bytes", got)
	}
	want := "store design (" + strconv.Itoa(designstore.Rows) + " rows): compacted bytes " + formatBytes(designstore.CompactedBytesAllowance+1e6) +
		", over its " + formatBytes(designstore.CompactedBytesAllowance) + " allowance (internal/store/designstore)"
	if got[0].String() != want {
		t.Errorf("breach reads %q, want %q", got[0], want)
	}

	commits := inputBreaches(designResult(designstore.CompactedBytesAllowance, designstore.CommitBytesAllowance+1))
	if len(commits) != 1 || commits[0].input != "bytes/commit" || commits[0].measured != designstore.CommitBytesAllowance+1 {
		t.Errorf("over on bytes/commit alone: breaches = %v, want bytes/commit at %d", commits, designstore.CommitBytesAllowance+1)
	}

	other := designResult(10*designstore.CompactedBytesAllowance, 10*designstore.CommitBytesAllowance)
	other.size = size{name: "n=28000", rows: 10 * designstore.Rows}
	if got := inputBreaches(other); len(got) != 0 {
		t.Errorf("a store at %d rows was judged against the design size's allowances: %v", other.size.rows, got)
	}
}

// The CEILING INPUTS section says which of three things happened: a breach,
// a design-size store within its allowances, or no design-size store at all.
// The last is said in words, because a run at other sizes judged nothing and
// a blank section would read as a pass.
func TestRenderCeilingInputsStatesTheVerdict(t *testing.T) {
	small := result{size: size{name: "today", rows: 118}}
	within := designResult(designstore.CompactedBytesAllowance, designstore.CommitBytesAllowance)
	over := designResult(designstore.CompactedBytesAllowance+1e6, designstore.CommitBytesAllowance)
	for _, c := range []struct {
		name    string
		results []result
		want    string
	}{
		{"no design-size store", []result{small}, "not judged: no store at the design size was measured"},
		{"within", []result{small, within}, "none over allowance"},
		{"over", []result{over}, "OVER  store design"},
	} {
		got := renderCeilingInputs(c.results, inputBreaches(c.results[len(c.results)-1]))
		if !strings.Contains(got, c.want) || !strings.Contains(got, "CEILING INPUTS: at the design size") {
			t.Errorf("%s: section omits %q:\n%s", c.name, c.want, got)
		}
	}
}

// An input over its allowance fails the gate on its own, with exit 2's
// gateError, listing the input: the nightly lane reads only the exit status.
func TestAnInputOverItsAllowanceFailsTheGate(t *testing.T) {
	gate := gateError{}.join(gateError{inputs: inputBreaches(designResult(designstore.CompactedBytesAllowance+1e6, designstore.CommitBytesAllowance))})
	if !gate.failed() {
		t.Fatal("a gate holding only an input breach did not fail")
	}
	if !strings.Contains(gate.Error(), "1 ceiling input(s) over allowance") || !strings.Contains(gate.Error(), "compacted bytes") {
		t.Errorf("gate error does not list the input:\n%s", gate.Error())
	}
}

// End to end through measureSize: a design-size store whose compacted bytes
// are past the allowance is logged as it is judged, before the storm runs,
// and comes back as the store's verdict. The fake lit mints an id per row and
// leaves a store file of the given apparent size; the file is sparse, so the
// test writes no real bytes.
func TestADesignStoreOverItsAllowanceIsLoggedAndReturned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lit")
	heavy := designstore.CompactedBytesAllowance + 5e6
	script := `#!/bin/sh
case "$1" in
  init) mkdir -p .git/links/dolt ;;
  import) i=0; while [ $i -lt ` + strconv.Itoa(designstore.Rows) + ` ]; do echo "  r$i -> bench-$i"; i=$((i+1)); done ;;
  sync) truncate -s ` + strconv.FormatInt(heavy, 10) + ` .git/links/dolt/store ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	env := hermeticEnv(os.Environ(), filepath.Join(dir, "config"), filepath.Join(dir, "state"))
	_, verdict, err := measureSize(litBinary{path: path}, dir, designSize, env, limits{budget: time.Minute}, runQuiet, &progress)
	if err != nil {
		t.Fatalf("measureSize error = %v; an input over its allowance is a verdict, not a broken run", err)
	}
	if len(verdict.inputs) != 1 || verdict.inputs[0].input != "compacted bytes" || verdict.inputs[0].measured != heavy {
		t.Fatalf("input verdict = %v, want compacted bytes at %d", verdict.inputs, heavy)
	}
	logged := progress.String()
	over := strings.Index(logged, "OVER ALLOWANCE: store design")
	if over < 0 || over > strings.Index(logged, "storming") {
		t.Errorf("the input verdict was not logged before the storm:\n%s", logged)
	}
}
