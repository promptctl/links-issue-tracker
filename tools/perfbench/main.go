// perfbench measures what a lit command costs a user: it generates lit stores at
// controlled row counts, times every user-facing command against each one, and
// reports the store's bytes on disk alongside.
//
// It exists because the scale epic (links-scale-dy46) stated its case with four
// numbers measured by hand in one session — `lit backlog` at 7.97s, `lit next`
// at 2.83s, `lit quickstart` at 0.07s, a 188 MB store — and a hand-measured
// number has no way to stay true. Within a month all four had moved: backlog had
// fallen to 0.54s as the query work landed, while the store had grown to 279 MB.
// An epic whose every remaining ticket is judged against its opening figures
// cannot afford them to decay silently, so the measurement is a command.
// [LAW:one-source-of-truth] the numbers have one home, and it is executable.
//
//	just perf                      # the whole table
//	just perf --sizes 0,118,590    # pick the store sizes
//
// WHAT IT IS NOT. This times one invocation at a time, so it says nothing about
// contention — the epic's hundred concurrent writers need a harness that runs
// writers against each other, which is its own ticket. Reading these figures as
// though they covered the write path under load is the one misuse to avoid.
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
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "perfbench: %v\n", err)
		os.Exit(1)
	}
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
	sizesFlag := fs.String("sizes", "", "comma-separated store row counts to measure (default: the repository's envelope, 0,118,590)")
	keep := fs.String("keep", "", "directory to build the stores in and leave behind (default: a temporary directory, removed on exit)")
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

	results := make([]result, 0, len(sizes))
	for _, sz := range sizes {
		fmt.Fprintf(progress, "generating %s store (%d rows) ...\n", sz.name, sz.rows)
		store, err := generate(bin, workDir, sz)
		if err != nil {
			return err
		}
		// Bytes before probes, because one probe writes: this is the size of
		// the store the read timings below describe.
		bytes, err := storeBytes(store.databasePath)
		if err != nil {
			return err
		}
		fmt.Fprintf(progress, "measuring %s store, %d repeats per probe ...\n", sz.name, repeats)
		samples, err := measure(bin, store)
		if err != nil {
			return err
		}
		results = append(results, result{size: sz, bytes: bytes, samples: samples})
	}

	cond := conditions{goos: runtime.GOOS, goarch: runtime.GOARCH, cpus: runtime.NumCPU()}
	fmt.Fprint(out, renderReport(results, cond))
	return nil
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
		f = strings.TrimSpace(f)
		rows, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("--sizes: %q is not a row count", f)
		}
		if rows < 0 {
			return nil, fmt.Errorf("--sizes: %d is not a row count", rows)
		}
		// Duplicates are refused rather than deduplicated. Each size generates
		// its own workspace named for it, so two entries of the same row count
		// would name one directory twice — and generation into a directory
		// that already holds a store is exactly what generate() now refuses.
		// Failing here names the real mistake instead of surfacing it as a
		// directory error three steps later.
		for _, seen := range sizes {
			if seen.rows == rows {
				return nil, fmt.Errorf("--sizes: %d appears more than once", rows)
			}
		}
		// A named size carries its name into the column header, and a size
		// given on the command line has only its row count to be called by.
		sizes = append(sizes, size{name: fmt.Sprintf("n=%d", rows), rows: rows})
	}
	return sizes, nil
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
