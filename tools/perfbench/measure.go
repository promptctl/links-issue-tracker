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
// ROUND-ROBIN, NOT PROBE-AT-A-TIME. Each round runs every probe once, and only
// then repeats. Running all five `backlog` invocations back to back would put
// every one of its samples inside the same few seconds, so a Go build or a peer
// agent session starting up in that window contaminates that probe's whole
// distribution — including its min, which is the figure reported. Spreading a
// probe's repeats across rounds means a load spike lands once in each probe's
// distribution rather than wholly inside one, and the min still has a clean
// round to find. The first round is additionally the cold one (page cache,
// dynamic linking), which min-of-rounds discards without needing a warm-up mode
// to configure.
//
// WRITES LAST. Probes are ordered reads-before-writes so every read figure
// describes the store at exactly the size the table claims, and the store's
// bytes — sampled before any probe runs — describe the same store the read
// timings do. A write probe interleaved with reads would grow the store
// underneath them, making the last round's reads measure a size no column names.
func measure(bin litBinary, store generatedStore) ([]sample, error) {
	ordered := slices.Clone(probes)
	slices.SortStableFunc(ordered, func(a, b probe) int {
		switch {
		case a.mutates == b.mutates:
			return 0
		case a.mutates:
			return 1
		default:
			return -1
		}
	})
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

	mins := make([]time.Duration, len(ordered))
	maxs := make([]time.Duration, len(ordered))
	for round := range repeats {
		for i, p := range ordered {
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
	samples := make([]sample, len(ordered))
	for i, p := range ordered {
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
