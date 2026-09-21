package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// sample is the stamped fact that one probe ran to completion the declared
// number of times against one store: nothing constructs a sample from an
// invocation whose exit code failed to prove the command did its job, so no
// consumer downstream has to ask whether a fast number is a real one.
// [LAW:parse-dont-validate]
type sample struct {
	probe probe
	min   time.Duration
	max   time.Duration
	runs  int
}

// measure times every probe against one store and returns one sample per probe.
//
// PHASES, THEN ROUNDS, THEN PROBES. Probes are grouped into phases by whether
// they write, and a phase finishes all of its rounds before the next phase
// starts. The grouping is what makes "reads before writes" true of the
// execution rather than only of the output: sorting the probe list and then
// replaying it once per round would order them inside a round and interleave
// them across rounds, so the write would run five times and every round after
// the first would read a store one row larger than the last. That was this
// function's first shape, and the bug it produced was invisible from the table
// -- worst on the empty control, whose whole job is isolating fixed cost, and
// which would have been non-empty for four of its five rounds.
//
// ROUND-ROBIN WITHIN A PHASE. Each round runs every probe in the phase once,
// and only then repeats. Running all five `backlog` invocations back to back
// would put every one of its samples inside the same few seconds, so a Go build
// or a peer agent session starting up in that window contaminates that probe's
// whole distribution -- including its min, which is the figure reported.
// Spreading a probe's repeats across rounds means a load spike lands once in
// each probe's distribution rather than wholly inside one, and the min still
// has a clean round to find. The first round is additionally the cold one (page
// cache, dynamic linking), which min-of-rounds discards without needing a
// warm-up mode to configure.
//
// What the phase split buys is that every read figure, and the store's byte
// count sampled before any probe ran, describe a store of exactly the size its
// column names. The write phase cannot have that property and does not claim
// it: each of its repeats adds a row, so `new` is timed against N, N+1, ...
// N+4 rows. That is inherent to timing a write more than once, and it is the
// reason writes go last rather than first.
func measure(bin litBinary, store generatedStore) ([]sample, error) {
	// Opened once for every invocation: handed an *os.File, exec passes the
	// descriptor straight to the child, so a chatty command's stdout costs the
	// child one write to the null device. An io.Discard writer instead would
	// make this process copy every byte of `lit backlog`'s output through a
	// pipe, charging the reader's timing for the harness's own plumbing.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer devnull.Close()

	var samples []sample
	for _, phase := range phasesOf(probes) {
		measured, err := measurePhase(bin, store, phase, devnull)
		if err != nil {
			return nil, err
		}
		samples = append(samples, measured...)
	}
	return samples, nil
}

// phasesOf groups probes into the order they may be run in: everything that
// only reads, then everything that writes.
//
// It returns groups rather than a sorted list because the difference is the
// whole fix. A sorted list still has to be replayed once per round by whoever
// repeats it, and that replay is what interleaves a write between two reads.
// A group is a unit the round loop lives inside, so the ordering cannot be
// undone downstream. An empty group is omitted, so a probe set with no writes
// produces one phase rather than a second, empty pass.
// [LAW:dataflow-not-control-flow]
func phasesOf(ps []probe) [][]probe {
	var reads, writes []probe
	for _, p := range ps {
		if p.mutates {
			writes = append(writes, p)
			continue
		}
		reads = append(reads, p)
	}
	phases := make([][]probe, 0, 2)
	for _, group := range [][]probe{reads, writes} {
		if len(group) == 0 {
			continue
		}
		phases = append(phases, group)
	}
	return phases
}

// measurePhase runs one phase to completion: every probe in it, repeats times,
// round-robin, reporting the min and max of each.
func measurePhase(bin litBinary, store generatedStore, phase []probe, devnull *os.File) ([]sample, error) {
	mins := make([]time.Duration, len(phase))
	maxs := make([]time.Duration, len(phase))
	for round := range repeats {
		for i, p := range phase {
			elapsed, err := runProbe(bin, store, p, devnull)
			if err != nil {
				return nil, fmt.Errorf("store %s (%d rows), probe %q, round %d: %w",
					store.size.name, store.size.rows, p.name, round+1, err)
			}
			if round == 0 || elapsed < mins[i] {
				mins[i] = elapsed
			}
			if elapsed > maxs[i] {
				maxs[i] = elapsed
			}
		}
	}
	samples := make([]sample, len(phase))
	for i, p := range phase {
		samples[i] = sample{probe: p, min: mins[i], max: maxs[i], runs: repeats}
	}
	return samples, nil
}

// runProbe times one invocation and returns its wall clock, or an error naming
// what the command did instead of its job.
//
// The exit code is checked against the probe's declared set rather than against
// zero, and an undeclared code is fatal rather than skipped. Both halves matter:
// `lit next` exits 6 on an empty store because there is genuinely no ready work,
// so treating nonzero as failure would make the empty control unmeasurable —
// while treating any exit as success would let a refused invocation, which
// returns an order of magnitude faster than a real answer, set the minimum this
// tool reports. [LAW:no-silent-failure]
func runProbe(bin litBinary, store generatedStore, p probe, devnull *os.File) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin.path, p.args...)
	cmd.Dir = store.root
	cmd.Stdout = devnull
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if ctx.Err() != nil {
		return 0, fmt.Errorf("exceeded the %s run budget (still running when it was killed): %s",
			runBudget, strings.TrimSpace(stderr.String()))
	}
	code, err := exitCode(err)
	if err != nil {
		return 0, err
	}
	if !slices.Contains(p.okCodes, code) {
		return 0, fmt.Errorf("`lit %s` exited %d, which is not among its expected codes %v — "+
			"the command did not do its work, so its %s is not a measurement of it:\n%s",
			strings.Join(p.args, " "), code, p.okCodes, elapsed.Round(time.Millisecond),
			strings.TrimSpace(stderr.String()))
	}
	return elapsed, nil
}

// exitCode reduces what exec reports to the child's status, keeping "the child
// exited nonzero" (a fact about lit, which the caller judges against the
// probe's declared codes) separate from "the child could not be run at all" (a
// fact about this harness, which is never a measurement).
func exitCode(err error) (int, error) {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), nil
	default:
		return 0, fmt.Errorf("could not run the binary: %w", err)
	}
}
