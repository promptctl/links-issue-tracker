// perfbench measures what a lit command costs a user: it generates lit stores at
// controlled row counts, times every user-facing command against each one, and
// reports the store's bytes on disk alongside.
//
// It exists because the scale epic (links-scale-dy46) stated its case with four
// numbers measured by hand in one session — `lit backlog` at 7.97s, `lit next`
// at 2.83s, `lit quickstart` at 0.07s, a 188 MB store — and a hand-measured
// number has no way to stay true. An epic whose every remaining ticket is
// judged against its opening figures cannot afford them to decay silently, so
// the measurement is a command.
// [LAW:one-source-of-truth] the numbers have one home, and it is executable.
//
//	just perf                      # the whole table
//	just perf --sizes 0,118,590    # pick the store sizes
//
// The probes time one invocation at a time, so they say nothing about
// contention; the storm (contention.go) is the measurement that does, a
// hundred writers against each store at once.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "perfbench: %v\n", err)
	// A breach is the gate working and anything else is the tool failing, and
	// a caller (a CI lane, a nightly job) must be able to tell them apart
	// without reading the log.
	if errors.As(err, new(gateError)) {
		os.Exit(exitGateFailed)
	}
	os.Exit(1)
}

// run is main's body with its writers passed in, so the flag surface can be
// exercised without a process exit and without either stream being a global.
// out carries the report — the thing a caller pipes — and progress carries the
// running commentary, which is on a separate stream because generating three
// stores takes long enough that a silent tool reads as a hung one.
// [LAW:effects-at-boundaries]
func run(args []string, out, progress io.Writer) error {
	fs := flag.NewFlagSet("perfbench", flag.ContinueOnError)
	fs.SetOutput(progress)
	sizesFlag := fs.String("sizes", "", "comma-separated store row counts to measure, or a named scale point: design for the store the size ceiling is derived to hold, 10x-store for ten times today's largest store, 10x-fleet for ten times today's stores (default: the repository's envelope, 0,118,590)")
	keep := fs.String("keep", "", "directory to build the stores in and leave behind (default: a temporary directory, removed on exit)")
	limitsPath := fs.String("limits", "", "file to write each verdict to as a JSON line the moment it is reached, keyed by the limit it breaks (default: none)")
	if err := fs.Parse(args); err != nil {
		// -h is a request that was granted, not a failure: Parse has already
		// written the usage to progress, and returning ErrHelp here would have
		// main print "flag: help requested" underneath it and exit 1.
		// [LAW:no-silent-failure] read the other way — an exit status is a
		// contract, and spending the failure code on success is the same defect
		// as spending the success code on failure.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// flag stops at the first non-flag argument and hands the rest back; nobody
	// downstream looks at them, so `just perf 0,118` (meaning --sizes) would
	// run the default table and report it as though it were what was asked
	// for, and `--keep "/tmp/lit perf"` would bind --keep=/tmp/lit and leave
	// `perf` here while announcing a directory the caller never named.
	// [LAW:no-silent-failure]
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument(s) %q: perfbench takes flags only "+
			"(did you mean --sizes %s?)", strings.Join(fs.Args(), " "), fs.Arg(0))
	}
	// An explicitly-passed empty --sizes is a mistake, not a request for the
	// default: the same class the stray-argument guard above rejects by name,
	// and reachable through any wrapper interpolating an unset variable. Only
	// the flag's ABSENCE selects the default envelope, which is why this asks
	// what was set rather than what the value is.
	sizesGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "sizes" {
			sizesGiven = true
		}
	})
	if sizesGiven && strings.TrimSpace(*sizesFlag) == "" {
		return fmt.Errorf("--sizes was given with no row counts; omit it for the default envelope")
	}
	sizes, err := parseSizes(*sizesFlag)
	if err != nil {
		return err
	}
	limits, closeLimits, err := limitsSink(*limitsPath)
	if err != nil {
		return err
	}
	defer closeLimits()

	workDir, cleanup, err := workspaceRoot(*keep, progress)
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Fprintf(progress, "building ./cmd/lit ...\n")
	bin, err := build(workDir, progress)
	if err != nil {
		return err
	}

	configHome := filepath.Join(workDir, "config-home")
	if err := os.MkdirAll(configHome, 0o755); err != nil {
		return fmt.Errorf("creating the hermetic config home: %w", err)
	}
	env := hermeticEnv(os.Environ(), configHome, filepath.Join(workDir, "state-home"))

	results := make([]result, 0, len(sizes))
	var gate gateError
	for _, sz := range sizes {
		r, verdict, err := measureSize(bin, workDir, sz, env, gateLimits, runQuiet, progress, limits)
		if err != nil {
			return err
		}
		results = append(results, r)
		gate = gate.join(verdict)
	}

	cond := conditions{goos: runtime.GOOS, goarch: runtime.GOARCH, cpus: runtime.NumCPU()}
	fmt.Fprint(out, renderReport(probes, results, gateLimits.budget, gate.breaches, cond))
	fmt.Fprint(out, renderCeilingInputs(results, gate.inputs))
	fmt.Fprint(out, renderContention(gate.writers))
	if gate.failed() {
		return gate
	}
	return nil
}

// limitsSink opens the --limits file, created empty so it holds this run's
// verdicts alone, or discards the records when no file was named: every run
// writes them, and only where they land varies. [LAW:dataflow-not-control-flow]
// The file is unbuffered, so a record is the kernel's the moment it is
// written and a run killed later keeps it.
func limitsSink(path string) (io.Writer, func(), error) {
	if path == "" {
		return io.Discard, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("--limits: %w", err)
	}
	return f, func() { f.Close() }, nil
}

// measureSize generates a scale point's stores and takes every measurement of
// them, in the one order they are valid in: bytes, then the probes, then compaction and
// history, which change the store every figure before them describes, then
// the storm, whose hundred writes would change it more than any of them.
//
// Each verdict is written to progress, and its record to limits, as soon as it
// is reached: the latency
// verdicts once the probes are measured, the ceiling-input verdicts once
// compaction and history are, and each storm writer that failed as the storm
// ends. Later steps can still fail or hang, and a CI job can time out during
// them; none of that may take a verdict already reached down with it. Kills
// reach progress sooner still, as they happen (measurePhase). The verdict it
// returns is the one it logged, judged once, and it is what run exits on.
// [LAW:one-source-of-truth]
func measureSize(bin litBinary, workDir string, sz size, env []string, lim limits, run stepRunner, progress, limits io.Writer) (result, gateError, error) {
	fmt.Fprintf(progress, "generating %s: %d store(s) ...\n", sz, len(sz.members()))
	members, err := generate(bin, workDir, sz, env, run)
	if err != nil {
		return result{}, gateError{}, err
	}
	store := members[0]
	// Bytes before probes, because one probe writes: this is the size of
	// the store the read timings below describe, and of the fleet beside it.
	bytes, err := storeBytes(store.databasePath)
	if err != nil {
		return result{}, gateError{}, err
	}
	fleetBytes, err := fleetBytesOf(members)
	if err != nil {
		return result{}, gateError{}, err
	}
	fmt.Fprintf(progress, "measuring %s, %d repeats per probe ...\n", sz, repeats)
	samples, err := measure(bin, store, lim, progress)
	if err != nil {
		return result{}, gateError{}, err
	}
	r := result{size: sz, bytes: bytes, fleetBytes: fleetBytes, samples: samples}
	var verdict gateError
	verdict.breaches = breaches(r, lim.budget)
	if err := logVerdicts(progress, limits, verdict.breaches); err != nil {
		return result{}, gateError{}, err
	}
	// The empty store has no row to edit, so its history is the zero cost the
	// report prints as absent.
	if r.compacted, err = compactedBytes(bin, store); err != nil {
		return result{}, gateError{}, err
	}
	if sz.rows > 0 {
		fmt.Fprintf(progress, "measuring %s history, %d single-row edits ...\n", sz, historyEdits)
		if r.history, err = measureHistory(bin, store, r.compacted); err != nil {
			return result{}, gateError{}, err
		}
	}
	verdict.inputs = inputBreaches(r)
	if err := logVerdicts(progress, limits, verdict.inputs); err != nil {
		return result{}, gateError{}, err
	}
	fmt.Fprintf(progress, "storming %s, %d concurrent writers across %d store(s) ...\n", sz, stormWriters, len(members))
	verdict.writers = storm(bin, members, lim.budget, run)
	if err := logVerdicts(progress, limits, verdict.writers); err != nil {
		return result{}, gateError{}, err
	}
	return r, verdict, nil
}

// logVerdicts logs each of vs as it is reached. [LAW:no-silent-failure] a
// limit that cannot be written fails the run: the lane files from that file,
// so a verdict missing from it is a limit nobody is told about.
func logVerdicts[V limitVerdict](progress, limits io.Writer, vs []V) error {
	for _, v := range vs {
		if err := logVerdict(progress, limits, v); err != nil {
			return err
		}
	}
	return nil
}

// fleetBytesOf totals every member's store, the disk axis of a fleet.
func fleetBytesOf(members []generatedStore) (int64, error) {
	var total int64
	for _, m := range members {
		n, err := storeBytes(m.databasePath)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// parseSizes turns the flag's text into the sizes to measure, or returns the
// envelope the repository already agreed on. Parsing here, once, is what lets
// everything downstream take a []size and never a string to re-interpret.
// [LAW:parse-dont-validate]
func parseSizes(raw string) ([]size, error) {
	if raw == "" {
		return defaultSizes, nil
	}
	fields := strings.Split(raw, ",")
	sizes := make([]size, 0, len(fields))
	for _, f := range fields {
		sz, err := parseSize(strings.TrimSpace(f))
		if err != nil {
			return nil, err
		}
		// Duplicates are refused rather than deduplicated. Each size generates
		// into a directory named for it, so two entries of one name would name
		// one directory twice — and generation into a directory that already
		// holds a store is exactly what generate() refuses. Failing here names
		// the real mistake instead of surfacing it as a directory error three
		// steps later. The name is what collides, so the name is what is
		// compared: `1074,10x-fleet` measure different things in different
		// directories even though their measured stores share a row count.
		for _, seen := range sizes {
			if seen.name == sz.name {
				return nil, fmt.Errorf("--sizes: %s is named twice", sz.name)
			}
		}
		sizes = append(sizes, sz)
	}
	return sizes, nil
}

// parseSize reads one --sizes entry: a named scale point, or a row count. A named size carries its name into the
// column header, and a size given as a count has only its count to be called
// by.
func parseSize(f string) (size, error) {
	for _, named := range namedSizes {
		if f == named.name {
			return named, nil
		}
	}
	rows, err := strconv.Atoi(f)
	if err != nil {
		names := make([]string, 0, len(namedSizes))
		for _, named := range namedSizes {
			names = append(names, strconv.Quote(named.name))
		}
		return size{}, fmt.Errorf("--sizes: %q is not a row count or one of %s", f, strings.Join(names, ", "))
	}
	if rows < 0 {
		return size{}, fmt.Errorf("--sizes: %d is not a row count", rows)
	}
	return size{name: fmt.Sprintf("n=%d", rows), rows: rows}, nil
}

// workspaceRoot resolves where the generated stores live and how they are
// disposed of. The default is a temporary directory removed on exit, because a
// 590-row store is tens of megabytes and leaving three of them behind on every
// run would make this tool a contributor to the disk problem the epic is about.
//
// --keep names a directory to build in and leave: the stores are the artifact
// whenever the question is why a figure moved, and having to re-generate them to
// look would be the reason nobody looks.
func workspaceRoot(keep string, progress io.Writer) (string, func(), error) {
	if keep != "" {
		abs, err := filepath.Abs(keep)
		if err != nil {
			return "", nil, fmt.Errorf("--keep: %w", err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", nil, fmt.Errorf("--keep: %w", err)
		}
		return abs, func() { fmt.Fprintf(progress, "stores left in %s\n", abs) }, nil
	}
	dir, err := os.MkdirTemp("", "lit-perfbench-")
	if err != nil {
		return "", nil, fmt.Errorf("creating work directory: %w", err)
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}
