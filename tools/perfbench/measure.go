package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// sample is the stamped fact that one probe ran against one store, and what
// came of it. Nothing constructs a sample from an invocation whose exit code
// failed to prove the command did its job, so no consumer downstream has to ask
// whether a fast number is a real one. [LAW:parse-dont-validate]
type sample struct {
	probe   probe
	outcome outcome
}

// outcome is how a probe's runs against one store ended: every one finished
// (completed), or one ran past the kill limit (killed). Two cases, so no field
// means one thing when a flag is set and another when it is not.
// [LAW:types-are-the-program]
type outcome interface{ isOutcome() }

// completed is a probe whose every run finished with a proving exit code.
type completed struct {
	min time.Duration
	max time.Duration
}

// killed is a probe one of whose runs was still going at the kill limit. round
// is the first such run: 0 is the warm-up, 1..repeats the recorded rounds. A
// kill is final for the probe, whatever its other rounds did, because a
// command that stalls one run in six past thirty times its budget is the
// lock-wait this epic exists to find, not noise to take the min over.
type killed struct {
	round int
	after time.Duration
}

func (completed) isOutcome() {}
func (killed) isOutcome()    {}

// measure times every probe against one store and returns one sample per probe.
//
// PHASES, THEN ROUNDS, THEN PROBES. Probes are grouped into phases by whether
// they write, and a phase finishes all of its rounds before the next phase
// starts. The grouping is what makes "reads before writes" true of the
// execution rather than only of the output: sorting the probe list and then
// replaying it once per round would order them inside a round and interleave
// them across rounds, so the write would run five times and every round after
// the first would read a store one row larger than the last. That skew is
// invisible from the table, and worst on the empty control, whose whole job is
// isolating fixed cost and which would be non-empty for four of its five
// rounds.
//
// ROUND-ROBIN WITHIN A PHASE. Each round runs every probe in the phase once,
// and only then repeats. Running all five `backlog` invocations back to back
// would put every one of its samples inside the same few seconds, so a Go build
// or a peer agent session starting up in that window contaminates that probe's
// whole distribution -- including its min, which is the figure reported.
// Spreading a probe's repeats across rounds means a load spike lands once in
// each probe's distribution rather than wholly inside one, and the min still
// has a clean round to find.
//
// ONE WARM-UP PASS PER PHASE, NEVER RECORDED. The first invocation of a probe
// is the cold one — page cache, dynamic linking — and on a store that has never
// been written it is more than that: lit's first write runs its inline
// compaction probe and remote check, which the imported stores paid during
// `lit import` and the empty store pays in the write probe's first round.
// Measured 2026-09-27, that made the empty column's `new` max 1.5x its min on a
// quiet machine. min-of-rounds already discards the cold round; the warm-up
// exists for the MAX, which the report tells the reader to take as a contention
// signal and which would otherwise carry a structural cost on exactly one cell.
// It is unconditional rather than a mode: every phase, every store.
// [LAW:dataflow-not-control-flow]
//
// What the phase split buys is that every read figure, and the store's byte
// count sampled before any probe ran, describe a store of exactly the size its
// column names. The write phase cannot have that property and does not claim
// it: each of its repeats adds a row, so `new` is timed against N, N+1, ...
// N+4 rows. That is inherent to timing a write more than once, and it is the
// reason writes go last rather than first.
//
// A kill is written to progress the moment it happens, because whatever stops
// the run later, a hung compaction or a CI job timeout, must not take a
// verdict already reached down with it.
func measure(bin litBinary, store generatedStore, lim limits, progress io.Writer) ([]sample, error) {
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
	for _, phase := range phasesOf(invocationsFor(probes, store), invocation.probeOf) {
		measured, err := measurePhase(bin, store, phase, devnull, lim, progress)
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
// It returns groups rather than a sorted list. A sorted list still has to be
// replayed once per round by whoever repeats it, and that replay is what
// interleaves a write between two reads. A group is a unit the round loop
// lives inside, so the ordering cannot be undone downstream. An empty group is
// omitted, so a probe set with no writes produces one phase rather than a
// second, empty pass.
// [LAW:dataflow-not-control-flow]
//
// It takes anything that carries a probe because the report orders its rows by
// the same rule the runner orders its runs by, and the rule is stated once.
// [LAW:one-source-of-truth]
func phasesOf[T any](xs []T, probeOf func(T) probe) [][]T {
	var reads, writes []T
	for _, x := range xs {
		if probeOf(x).mutates {
			writes = append(writes, x)
			continue
		}
		reads = append(reads, x)
	}
	phases := make([][]T, 0, 2)
	for _, group := range [][]T{reads, writes} {
		if len(group) == 0 {
			continue
		}
		phases = append(phases, group)
	}
	return phases
}

func (inv invocation) probeOf() probe { return inv.probe }

// measurePhase runs one phase to completion: every invocation in it once
// unrecorded, then repeats times round-robin, reporting each one's outcome.
//
// A killed run does not stop the phase. Every invocation runs its warm-up and
// every round whatever happened before, so the set of runs is the same for
// every probe and the first kill is simply what the outcome records; a probe
// killed in its warm-up costs its repeats at the kill limit, which is what
// keeps that limit a fraction of a CI job. [LAW:dataflow-not-control-flow]
func measurePhase(bin litBinary, store generatedStore, phase []invocation, devnull *os.File, lim limits, progress io.Writer) ([]sample, error) {
	mins := make([]time.Duration, len(phase))
	maxs := make([]time.Duration, len(phase))
	kills := make([]*killed, len(phase))
	for round := range repeats + 1 {
		for i, inv := range phase {
			elapsed, err := runProbe(bin, store, inv, devnull, lim.kill())
			if errors.Is(err, errKilled) {
				if kills[i] == nil {
					kills[i] = &killed{round: round, after: lim.kill()}
					// err carries what the command wrote before the kill, the
					// one clue to why it hung.
					fmt.Fprintf(progress, "killed: store %s, probe %q, %s: %v; measuring on\n",
						store, inv.probe.name, roundName(round), err)
				}
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("store %s, probe %q, %s: %w",
					store, inv.probe.name, roundName(round), err)
			}
			// Round 0 is the warm-up and is never recorded: its cold cost
			// (page cache, dynamic linking, a never-written store's first
			// write) would land on the max the report reads as contention.
			if round == 0 {
				continue
			}
			if round == 1 || elapsed < mins[i] {
				mins[i] = elapsed
			}
			maxs[i] = max(maxs[i], elapsed)
		}
	}
	samples := make([]sample, len(phase))
	for i, inv := range phase {
		samples[i] = sample{probe: inv.probe, outcome: completed{min: mins[i], max: maxs[i]}}
		if kills[i] != nil {
			samples[i].outcome = *kills[i]
		}
	}
	return samples, nil
}

// roundName is how an error names the run it came from.
func roundName(round int) string {
	if round == 0 {
		return "warm-up"
	}
	return fmt.Sprintf("round %d", round)
}

// errKilled is the one way a probe run ends without a measurement and without
// failing the run: it was still going at the kill limit. Every other error
// from runProbe means the run cannot be trusted at all.
var errKilled = errors.New("still running at the kill limit")

// runProbe times one invocation and returns its wall clock, or an error naming
// what the command did instead of its job.
//
// The exit code is checked against the probe's declared set for this store's
// size rather than against zero, and an undeclared code is fatal rather than
// skipped. Both halves matter: `lit next` exits 6 on an empty store because
// there is genuinely no ready work, so treating nonzero as failure would make
// the empty control unmeasurable — while treating any exit as success would let
// a refused invocation, which returns an order of magnitude faster than a real
// answer, set the minimum this tool reports. [LAW:no-silent-failure]
func runProbe(bin litBinary, store generatedStore, inv invocation, devnull *os.File, kill time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kill)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin.path, inv.args...)
	cmd.Dir = store.root
	cmd.Env = store.env
	cmd.Stdout = devnull
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = killGrace
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	// Killed means the process ended on the kill's signal, not that the
	// deadline has passed by now: a run that exits at 29.99s is a measurement.
	if ctx.Err() != nil && !cmd.ProcessState.Exited() {
		return 0, fmt.Errorf("%w (%s): %s", errKilled, kill, strings.TrimSpace(stderr.String()))
	}
	code, err := exitCode(err)
	if err != nil {
		return 0, err
	}
	if !slices.Contains(inv.okCodes, code) {
		return 0, fmt.Errorf("`lit %s` exited %d, which is not among its expected codes %v at %d rows — "+
			"the command did not do its work, so its %s is not a measurement of it:\n%s",
			strings.Join(inv.args, " "), code, inv.okCodes, store.rows(), elapsed.Round(time.Millisecond),
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
