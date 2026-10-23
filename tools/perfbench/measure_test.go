package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeLit writes a shell script that exits with the given code, standing in for
// the real binary so the exit-code contract can be exercised without building
// lit or generating a store. It appends its own argv to a log, which is the
// only way a test can see the ORDER invocations actually happened in — the
// samples measure returns are built from the probe list and would look
// identical however the runs were interleaved.
func fakeLit(t *testing.T, exitCode int) (litBinary, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lit")
	log := filepath.Join(dir, "invocations.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\necho stdout noise\necho stderr detail >&2\nexit %d\n", log, exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return litBinary{path: path}, log
}

// invocations reads back what the fake binary was actually asked to run, in
// order.
func invocations(t *testing.T, log string) []string {
	t.Helper()
	blob, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("reading invocation log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(blob)), "\n")
}

// fakeStore is a store of seven rows that exist only as ids, so every probe,
// `show` included, has an invocation against it.
func fakeStore(t *testing.T) generatedStore {
	t.Helper()
	return generatedStore{size: size{name: "fake", rows: 7}, root: t.TempDir(),
		ids: []string{"bench-a", "bench-b", "bench-c", "bench-d", "bench-e", "bench-f", "bench-g"}}
}

// resolve is p's one invocation against store, failing the test if the store
// admits none.
func resolve(t *testing.T, p probe, store generatedStore) invocation {
	t.Helper()
	invs := invocationsFor([]probe{p}, store)
	if len(invs) != 1 {
		t.Fatalf("probe %q has %d invocations against store %s, want 1", p.name, len(invs), store.size.name)
	}
	return invs[0]
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// The whole tool rests on this: a command that failed returns faster than a
// command that worked, so an unchecked exit code turns every breakage into the
// best measurement in the table. An undeclared exit code must therefore produce
// no sample at all.
//
// The codes here are written out rather than read from the probes list, so this
// test cannot agree with a wrong okCodes value by construction.
func TestRunProbeRefusesAnUndeclaredExitCode(t *testing.T) {
	bin, _ := fakeLit(t, 6)
	store := fakeStore(t)
	probeUnderTest := resolve(t, probe{name: "next", argv: fixed("next"), okCodes: exits(0)}, store)
	elapsed, err := runProbe(bin, store, probeUnderTest, devNull(t), gateLimits.kill())
	if err == nil {
		t.Fatalf("runProbe accepted exit 6 for a probe declaring only {0}, returning %s; "+
			"a failed invocation must not become a measurement", elapsed)
	}
	if elapsed != 0 {
		t.Errorf("runProbe returned a duration (%s) alongside its error; a refused "+
			"invocation has no timing to report", elapsed)
	}
	for _, want := range []string{"exited 6", "[0]", "stderr detail"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — the reader cannot tell what the command did instead of its job", err, want)
		}
	}
}

// The complement, and the reason the check is a declared set rather than a
// comparison against zero: `lit next` exits 6 on an empty store because there
// is genuinely no ready work, and the empty store is the control the epic reads
// first. Treating nonzero as failure would make it unmeasurable.
func TestRunProbeAcceptsADeclaredNonZeroExitCode(t *testing.T) {
	bin, _ := fakeLit(t, 6)
	store := fakeStore(t)
	probeUnderTest := resolve(t, probe{name: "next", argv: fixed("next"), okCodes: exits(0, 6)}, store)
	elapsed, err := runProbe(bin, store, probeUnderTest, devNull(t), gateLimits.kill())
	if err != nil {
		t.Fatalf("runProbe(exit 6, okCodes {0,6}) error = %v, want a sample", err)
	}
	if elapsed <= 0 {
		t.Errorf("runProbe returned %s, want a positive wall clock", elapsed)
	}
}

// The accepted set is judged against the store the probe ran on, not against a
// fixed list. `lit next` exiting 6 proves it ran on the empty store and proves
// it did NOT on a populated one, where every generated row is open: a routing
// regression that passes over every row exits 6 in ~30ms, and a size-blind set
// would record it as that command's fastest time.
//
// This uses the real `next` probe deliberately, with the expected verdicts
// written out: it pins the shipped rule, not a fixture that mirrors it.
func TestRunProbeJudgesTheExitAgainstTheStoreSize(t *testing.T) {
	bin, _ := fakeLit(t, 6)
	var next probe
	for _, p := range probes {
		if p.name == "next" {
			next = p
		}
	}
	if next.name == "" {
		t.Fatal("no probe named next")
	}
	empty := generatedStore{size: size{name: "empty", rows: 0}, root: t.TempDir()}
	if _, err := runProbe(bin, empty, resolve(t, next, empty), devNull(t), gateLimits.kill()); err != nil {
		t.Errorf("exit 6 on the empty store is the honest answer, got error: %v", err)
	}
	populated := generatedStore{size: size{name: "5x-target", rows: 590}, root: t.TempDir()}
	if _, err := runProbe(bin, populated, resolve(t, next, populated), devNull(t), gateLimits.kill()); err == nil {
		t.Error("exit 6 on a 590-row store, where every row is open, was accepted as a measurement")
	}
}

// The probe runs under the store's hermetic environment, not the harness's.
// A developer's LIT_DISABLE_AUTO_SYNC or global lit config would otherwise
// select a different code path on their machine than on everyone else's, with
// nothing in the table to say so.
func TestRunProbeRunsLitUnderTheStoresEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	t.Setenv("LIT_DISABLE_AUTO_SYNC", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "lit")
	seen := filepath.Join(dir, "seen")
	script := fmt.Sprintf("#!/bin/sh\necho \"${LIT_DISABLE_AUTO_SYNC:-unset} ${XDG_CONFIG_HOME:-unset}\" > %q\nexit 0\n", seen)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	configHome := filepath.Join(dir, "config-home")
	store := generatedStore{size: size{name: "fake", rows: 7}, root: dir, env: hermeticEnv(os.Environ(), configHome, filepath.Join(dir, "state-home"))}
	if _, err := runProbe(litBinary{path: path}, store, resolve(t, probe{name: "ls", argv: fixed("ls"), okCodes: exits(0)}, store), devNull(t), gateLimits.kill()); err != nil {
		t.Fatalf("runProbe error = %v", err)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if want := "unset " + configHome + "\n"; string(got) != want {
		t.Errorf("the probe saw %q, want %q: lit ran under the harness's environment rather than the store's", got, want)
	}
}

func TestRunProbeReportsAMissingBinaryAsAHarnessFailure(t *testing.T) {
	bin := litBinary{path: filepath.Join(t.TempDir(), "absent")}
	store := fakeStore(t)
	_, err := runProbe(bin, store, resolve(t, probe{name: "ls", argv: fixed("ls"), okCodes: exits(0)}, store), devNull(t), gateLimits.kill())
	if err == nil {
		t.Fatal("runProbe with a nonexistent binary returned no error")
	}
	if !strings.Contains(err.Error(), "could not run the binary") {
		t.Errorf("error %q does not distinguish a harness failure from lit exiting nonzero", err)
	}
}

// Every read must be measured before anything writes, so that every read
// figure and the store's byte count describe a store of the size the column
// claims.
//
// This asserts on the ORDER OF EXECUTION, read back from the fake binary's own
// log, and not on the order of the returned samples. The distinction is the
// whole test: samples are built from the probe list, so they report
// reads-before-writes no matter how the invocations were interleaved.
func TestMeasureRunsEveryReadBeforeAnyWrite(t *testing.T) {
	bin, log := fakeLit(t, 0)
	store := fakeStore(t)
	samples, err := measure(bin, store, gateLimits, io.Discard)
	if err != nil {
		t.Fatalf("measure() error = %v", err)
	}
	if len(samples) != len(probes) {
		t.Fatalf("measure() returned %d samples, want one per probe (%d)", len(samples), len(probes))
	}

	writeArgs := map[string]bool{}
	for _, inv := range invocationsFor(probes, store) {
		if inv.probe.mutates {
			writeArgs[strings.Join(inv.args, " ")] = true
		}
	}
	if len(writeArgs) == 0 {
		t.Fatal("no probe mutates, so this test would pass vacuously")
	}

	ran := invocations(t, log)
	// One unrecorded warm-up per probe, then the recorded repeats.
	if len(ran) != len(probes)*(repeats+1) {
		t.Errorf("fake binary ran %d times, want %d (%d probes x (1 warm-up + %d repeats))",
			len(ran), len(probes)*(repeats+1), len(probes), repeats)
	}
	firstWrite := -1
	for i, line := range ran {
		if writeArgs[line] {
			firstWrite = i
			break
		}
	}
	if firstWrite < 0 {
		t.Fatalf("no write invocation in the log: %v", ran)
	}
	for i, line := range ran[firstWrite:] {
		if !writeArgs[line] {
			t.Errorf("invocation %d (%q) is a read that ran after the first write at %d; "+
				"every read after a write measures a store one or more rows larger than "+
				"its column claims\nfull order: %v", firstWrite+i, line, firstWrite, ran)
			break
		}
	}
	for _, s := range samples {
		c, ok := s.outcome.(completed)
		if !ok {
			t.Errorf("sample %q is %#v, want completed: no run was slow", s.probe.name, s.outcome)
			continue
		}
		if c.min > c.max {
			t.Errorf("sample %q has min %s above max %s", s.probe.name, c.min, c.max)
		}
	}
}

// The warm-up is not a sample. A probe whose every invocation is slow but
// whose warm-up is the slowest must report the max of its RECORDED rounds, or
// the max the report tells a reader to take as a contention signal carries the
// cold start it exists to exclude. The fake sleeps on its first invocation
// only, so the cold cost is on the warm-up and nowhere else.
func TestMeasureDoesNotRecordTheWarmUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lit")
	marker := filepath.Join(dir, "warmed")
	script := fmt.Sprintf("#!/bin/sh\nif [ ! -e %q ]; then touch %q; sleep 0.3; fi\nexit 0\n", marker, marker)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store := fakeStore(t)
	only := []invocation{resolve(t, probe{name: "ls", argv: fixed("ls"), okCodes: exits(0)}, store)}
	samples, err := measurePhase(litBinary{path: path}, store, only, devNull(t), gateLimits, io.Discard)
	if err != nil {
		t.Fatalf("measurePhase error = %v", err)
	}
	if got := samples[0].outcome.(completed).max; got >= 300*time.Millisecond {
		t.Errorf("recorded max is %s; the 300ms cold invocation was recorded rather than discarded as the warm-up", got)
	}
}

// The reported samples must also be ordered reads-first, because the report
// prints them in slice order and a table listing the write among the reads
// would misdescribe what it measured.
func TestMeasureReturnsReadSamplesFirst(t *testing.T) {
	bin, _ := fakeLit(t, 0)
	samples, err := measure(bin, fakeStore(t), gateLimits, io.Discard)
	if err != nil {
		t.Fatalf("measure() error = %v", err)
	}
	seenWrite := false
	for _, s := range samples {
		if s.probe.mutates {
			seenWrite = true
			continue
		}
		if seenWrite {
			t.Errorf("read sample %q is reported after a write sample", s.probe.name)
		}
	}
}

// A probe list containing a command the binary refuses must fail the whole run
// rather than yield a table missing a row, because a table read as complete
// while a column silently lost an entry is worse than no table.
func TestMeasureFailsTheRunWhenAnyProbeFails(t *testing.T) {
	bin, _ := fakeLit(t, 3)
	if _, err := measure(bin, fakeStore(t), gateLimits, io.Discard); err == nil {
		t.Fatal("measure() with a binary exiting 3 returned no error")
	}
}

// A probe that outlives the kill limit is a verdict, not a crash: it is
// recorded as killed, the kill reaches progress when it happens, and the phase
// goes on to measure every other probe. Before this, one hung command ended
// the run and named only itself, so a regression slowing two commands showed
// one of them. The fake hangs on `backlog` only; `exec` replaces the shell so
// the kill reaches the sleeping process rather than orphaning it.
func TestAKilledProbeIsRecordedAndTheRunContinues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lit")
	script := "#!/bin/sh\nif [ \"$1\" = backlog ]; then echo 'waiting on lock' >&2; exec sleep 30; fi\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store := fakeStore(t)
	phase := []invocation{
		resolve(t, probe{name: "backlog", argv: fixed("backlog"), okCodes: exits(0)}, store),
		resolve(t, probe{name: "ls", argv: fixed("ls"), okCodes: exits(0)}, store),
	}
	var progress strings.Builder
	lim := limits{budget: 5 * time.Millisecond}
	samples, err := measurePhase(litBinary{path: path}, store, phase, devNull(t), lim, &progress)
	if err != nil {
		t.Fatalf("measurePhase error = %v; a kill must not end the run", err)
	}
	if got, want := samples[0].outcome, (killed{round: 0, after: lim.kill()}); got != want {
		t.Errorf("backlog outcome = %#v, want %#v: killed in its warm-up", got, want)
	}
	if _, ok := samples[1].outcome.(completed); !ok {
		t.Errorf("ls outcome = %#v, want completed: the kill of another probe stopped it being measured", samples[1].outcome)
	}
	if got := strings.Count(progress.String(), "killed: "); got != 1 {
		t.Errorf("progress names %d kills, want 1 (the first kill of backlog, as it happened):\n%s", got, progress.String())
	}
	for _, want := range []string{`"backlog"`, "warm-up", "150ms", "waiting on lock"} {
		if !strings.Contains(progress.String(), want) {
			t.Errorf("kill line does not mention %q:\n%s", want, progress.String())
		}
	}
}

// A store's verdicts reach progress before anything after its probes runs, so
// a later failure (here, the compaction the history measurement starts with)
// cannot erase them. The fake lit does just enough to be generated: init makes
// the store directory, import prints the id mapping, and `sync` fails.
// `next` is slow enough to break a budget lowered for the test.
func TestAStoresVerdictsAreLoggedBeforeItsLaterStepsRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lit")
	script := `#!/bin/sh
case "$1" in
  init) mkdir -p .git/links/dolt ;;
  import) printf 'imported 1 issues\n  r0 -> bench-x\n' ;;
  next) sleep 0.2 ;;
  sync) echo "compaction broke" >&2; exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	env := hermeticEnv(os.Environ(), filepath.Join(dir, "config"), filepath.Join(dir, "state"))
	lim := limits{budget: 100 * time.Millisecond}
	_, _, _, err := measureSize(litBinary{path: path}, dir, size{name: "one", rows: 1}, env, lim, runQuiet, &progress)
	if err == nil || !strings.Contains(err.Error(), "compaction broke") {
		t.Fatalf("measureSize error = %v, want the failed compaction", err)
	}
	if !strings.Contains(progress.String(), "OVER BUDGET: store one (1 rows): next took") {
		t.Errorf("the store's verdict on next was not logged before the compaction failed:\n%s", progress.String())
	}
}

func TestExitCodeSeparatesChildStatusFromHarnessFailure(t *testing.T) {
	if code, err := exitCode(nil); code != 0 || err != nil {
		t.Errorf("exitCode(nil) = (%d, %v), want (0, nil)", code, err)
	}
	if _, err := exitCode(os.ErrNotExist); err == nil {
		t.Error("exitCode(non-exit error) returned no error; a binary that could not run is not a measurement")
	}
}
