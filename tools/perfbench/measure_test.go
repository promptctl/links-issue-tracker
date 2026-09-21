package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

func fakeStore(t *testing.T) generatedStore {
	t.Helper()
	return generatedStore{size: size{name: "fake", rows: 7}, root: t.TempDir()}
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
	probeUnderTest := probe{name: "next", args: []string{"next"}, okCodes: []int{0}}
	elapsed, err := runProbe(bin, fakeStore(t), probeUnderTest, devNull(t))
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
	probeUnderTest := probe{name: "next", args: []string{"next"}, okCodes: []int{0, 6}}
	elapsed, err := runProbe(bin, fakeStore(t), probeUnderTest, devNull(t))
	if err != nil {
		t.Fatalf("runProbe(exit 6, okCodes {0,6}) error = %v, want a sample", err)
	}
	if elapsed <= 0 {
		t.Errorf("runProbe returned %s, want a positive wall clock", elapsed)
	}
}

func TestRunProbeReportsAMissingBinaryAsAHarnessFailure(t *testing.T) {
	bin := litBinary{path: filepath.Join(t.TempDir(), "absent")}
	_, err := runProbe(bin, fakeStore(t), probe{name: "ls", args: []string{"ls"}, okCodes: []int{0}}, devNull(t))
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
// reads-before-writes no matter how the invocations were interleaved. An
// earlier version of this test checked the slice, passed, and was passing while
// the write probe really was running once per round with four rounds of reads
// after it.
func TestMeasureRunsEveryReadBeforeAnyWrite(t *testing.T) {
	bin, log := fakeLit(t, 0)
	samples, err := measure(bin, fakeStore(t))
	if err != nil {
		t.Fatalf("measure() error = %v", err)
	}
	if len(samples) != len(probes) {
		t.Fatalf("measure() returned %d samples, want one per probe (%d)", len(samples), len(probes))
	}

	writeArgs := map[string]bool{}
	for _, p := range probes {
		if p.mutates {
			writeArgs[strings.Join(p.args, " ")] = true
		}
	}
	if len(writeArgs) == 0 {
		t.Fatal("no probe mutates, so this test would pass vacuously")
	}

	ran := invocations(t, log)
	if len(ran) != len(probes)*repeats {
		t.Errorf("fake binary ran %d times, want %d (%d probes x %d repeats)",
			len(ran), len(probes)*repeats, len(probes), repeats)
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
		if s.runs != repeats {
			t.Errorf("sample %q recorded %d runs, want %d", s.probe.name, s.runs, repeats)
		}
		if s.min > s.max {
			t.Errorf("sample %q has min %s above max %s", s.probe.name, s.min, s.max)
		}
	}
}

// The reported samples must also be ordered reads-first, because the report
// prints them in slice order and a table listing the write among the reads
// would misdescribe what it measured.
func TestMeasureReturnsReadSamplesFirst(t *testing.T) {
	bin, _ := fakeLit(t, 0)
	samples, err := measure(bin, fakeStore(t))
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
	if _, err := measure(bin, fakeStore(t)); err == nil {
		t.Fatal("measure() with a binary exiting 3 returned no error")
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
