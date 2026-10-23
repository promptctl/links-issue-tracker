package main

import (
	"fmt"
	"strings"
	"time"
)

// The verdict: which probes broke the latency budget, and by how much.
//
// It is what turns this tool from a table someone has to read into a gate that
// fails on its own. `lit backlog` reached 11.3 seconds on 2026-08-25 with every
// row correct and the whole suite green, and it was a human noticing that
// caught it. The budget is the scale epic's destination, so a breach here is a
// breach anywhere: in the CI job, and on a developer's machine running `just
// perf`.
//
// WHAT IT CANNOT SEE. A generated store has no git remote, so a command's
// receive path (the automatic fetch a live workspace pays) never runs here,
// and a slowdown confined to it passes this gate.

// breach is one probe over budget against one store. Its String is the one
// rendering of a verdict: the line written to progress the moment the verdict
// is reached and the line in the report's BUDGET section are the same text.
// [LAW:one-source-of-truth]
type breach struct {
	store   size
	probe   string
	outcome outcome
	budget  time.Duration
}

func (b breach) String() string {
	at := fmt.Sprintf("store %s (%d rows): %s", b.store.name, b.store.rows, b.probe)
	switch o := b.outcome.(type) {
	case completed:
		return fmt.Sprintf("%s took %s, %.1fx its %s budget", at, formatDuration(o.min), o.min.Seconds()/b.budget.Seconds(), b.budget)
	case killed:
		return fmt.Sprintf("%s was killed in its %s, still running at %s, over %.0fx its %s budget",
			at, roundName(o.round), o.after, o.after.Seconds()/b.budget.Seconds(), b.budget)
	default:
		panic(fmt.Sprintf("breach: unknown outcome %T", b.outcome))
	}
}

// breaches judges one store's samples against budget. A completed probe
// breaches when its min is over budget. A killed one always does, since the
// kill limit is far above any budget.
func breaches(r result, budget time.Duration) []breach {
	var out []breach
	for _, s := range r.samples {
		if over(s.outcome, budget) {
			out = append(out, breach{store: r.size, probe: s.probe.name, outcome: s.outcome, budget: budget})
		}
	}
	return out
}

func over(o outcome, budget time.Duration) bool {
	switch o := o.(type) {
	case completed:
		return o.min > budget
	case killed:
		return true
	default:
		panic(fmt.Sprintf("over: unknown outcome %T", o))
	}
}

// budgetError is a run that measured everything it was asked to and found
// probes over budget. It is its own type so main can give it its own exit
// status: a breach is the gate doing its job, and a caller has to be able to
// tell it from the tool failing to do its job. [LAW:no-silent-failure]
type budgetError struct{ breaches []breach }

func (e budgetError) Error() string {
	lines := make([]string, len(e.breaches))
	for i, b := range e.breaches {
		lines[i] = "  " + b.String()
	}
	return fmt.Sprintf("%d probe(s) over budget:\n%s", len(e.breaches), strings.Join(lines, "\n"))
}

// exitBudgetBreached is the exit status of a run whose only failure is
// probes over budget; any other failure exits 1.
const exitBudgetBreached = 2
