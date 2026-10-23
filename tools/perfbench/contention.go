package main

import (
	"fmt"
	"strings"
	"sync"
)

// The contention gate: a hundred writers against one store, at once, and every
// one of them must complete.
//
// It is the scale epic's headline bar ("100 concurrent writers all complete,
// with no writer failing on contention") made a check that fails the build.
// Every probe above runs alone, so none of them can see a writer refused
// because another writer holds the store; this is the one measurement that
// can. On 2026-10-23 it found 9 to 84 of 100 `lit new` failing workspace_busy
// in roughly one run in three, on a store every single probe called healthy.

// stormWriters is how many writers the gate runs against each store at once:
// the epic's number, not a figure fitted to what lit sustains today.
const stormWriters = 100

// writerFailure is one storm writer that did not complete: which writer, and
// the error naming what it did instead, its stderr included. Its String is the
// one rendering of the verdict, logged the moment it is reached and printed in
// the report's CONTENTION section alike. [LAW:one-source-of-truth]
type writerFailure struct {
	store  size
	writer int
	err    error
}

func (f writerFailure) String() string {
	return fmt.Sprintf("store %s (%d rows): writer %d of %d did not complete: %v",
		f.store.name, f.store.rows, f.writer, stormWriters, f.err)
}

// stormStep is writer i's invocation: an epic, for the reason the write probe
// files one (the one type that names no parent), titled by its writer so a
// failure can be found in the store it left. Each writer is bounded by
// runBudget like every other step, so a writer that never returns is named as
// a failure rather than holding the run.
func stormStep(bin litBinary, writer int) step {
	return step{budget: runBudget, argv: []string{
		bin.path, "new", "--title", fmt.Sprintf("perfbench storm writer %d", writer), "--type", "epic", "--topic", "bench",
	}}
}

// storm starts stormWriters writers against store at once and returns every one
// that did not complete, in writer order. Every writer runs whatever the
// others do: the verdict is the full list, because "writers 3, 41 and 77
// failed" is the shape of a contention bug, and the first failure alone hides
// it. [LAW:dataflow-not-control-flow]
func storm(bin litBinary, store generatedStore, run stepRunner) []writerFailure {
	errs := make([]error, stormWriters)
	var wg sync.WaitGroup
	for i := range stormWriters {
		wg.Go(func() {
			_, errs[i] = run(store.root, store.env, stormStep(bin, i+1))
		})
	}
	wg.Wait()
	var failed []writerFailure
	for i, err := range errs {
		if err != nil {
			failed = append(failed, writerFailure{store: store.size, writer: i + 1, err: err})
		}
	}
	return failed
}

// renderContention is the report's CONTENTION section: the bar, then each
// writer that failed it, or the line saying none did.
func renderContention(failed []writerFailure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nCONTENTION: %d concurrent `lit new` against each store, every one must complete.\n", stormWriters)
	for _, f := range failed {
		fmt.Fprintf(&b, "  FAILED  %s\n", f)
	}
	if len(failed) == 0 {
		b.WriteString("  every writer completed\n")
	}
	return b.String()
}
