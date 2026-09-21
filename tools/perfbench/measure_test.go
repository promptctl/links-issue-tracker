package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeLit writes a shell script that exits with the given code, standing in for
// the real binary so the exit-code contract can be exercised without building
// lit or generating a store.
func fakeLit(t *testing.T, exitCode int) litBinary {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lit")
	script := fmt.Sprintf("#!/bin/sh\necho stdout noise\necho stderr detail >&2\nexit %d\n", exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return litBinary{path: path}
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
	bin := fakeLit(t, 6)
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
	bin := fakeLit(t, 6)
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

// Read probes must all be measured before anything writes, so that every read
// figure and the store's byte count describe a store of the size the column
// claims.
func TestMeasureOrdersEveryReadBeforeAnyWrite(t *testing.T) {
	samples, err := measure(fakeLit(t, 0), fakeStore(t))
	if err != nil {
		t.Fatalf("measure() error = %v", err)
	}
	if len(samples) != len(probes) {
		t.Fatalf("measure() returned %d samples, want one per probe (%d)", len(samples), len(probes))
	}
	firstWrite := slices.IndexFunc(samples, func(s sample) bool { return s.probe.mutates })
	lastRead := -1
	for i, s := range samples {
		if !s.probe.mutates {
			lastRead = i
		}
	}
	if firstWrite >= 0 && firstWrite < lastRead {
		t.Errorf("write probe %q is measured at index %d, before read probe at index %d; "+
			"the reads after it describe a store one row larger than its column claims",
			samples[firstWrite].probe.name, firstWrite, lastRead)
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

// A probe list containing a command the binary refuses must fail the whole run
// rather than yield a table missing a row, because a table read as complete
// while a column silently lost an entry is worse than no table.
func TestMeasureFailsTheRunWhenAnyProbeFails(t *testing.T) {
	if _, err := measure(fakeLit(t, 3), fakeStore(t)); err == nil {
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
