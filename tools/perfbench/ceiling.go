package main

import (
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/store/designstore"
)

// The ceiling-input gate: the store size ceiling is derived from what a
// design-size store was measured to cost (internal/store/designstore), with
// headroom over each figure. A change that makes rows or commits cost more on
// disk past that headroom makes the derivation false, and this is the check
// that says so.
//
// It judges only a store at the design size, because the allowances are facts
// about that one size and per-row cost grows with the store: below it a
// regression has to be many times over to show, and above it every store
// fails. That row count is the one discriminator, and it is the domain's.
// Each allowance is a fixed figure in the code; no run is compared with an
// earlier one, since a ratchet fails on noise and then gets muted.

// ceilingInput is one figure the ceiling is derived from: how the report
// names it, how a result carries it, and the most a design-size store may
// measure. Two inputs, as values, so the judgment and the report walk one
// list. [LAW:one-type-per-behavior]
type ceilingInput struct {
	name      string
	of        func(result) int64
	allowance int64
}

var ceilingInputs = []ceilingInput{
	{name: "compacted bytes", of: func(r result) int64 { return r.compacted }, allowance: designstore.CompactedBytesAllowance},
	{name: "bytes/commit", of: func(r result) int64 { return r.history.perCommit() }, allowance: designstore.CommitBytesAllowance},
}

// designSize is the store the ceiling is derived to hold, by the name
// `--sizes design` asks for it with, so a caller measures it without writing
// its row count down a second time. [LAW:one-source-of-truth]
var designSize = size{name: "design", rows: designstore.Rows}

// inputBreach is one ceiling input that a design-size store measured over
// its allowance.
type inputBreach struct {
	store    size
	input    string
	measured int64
	allowed  int64
}

// String is the one rendering of an input verdict: the progress line and the
// report's CEILING INPUTS line are the same text. [LAW:one-source-of-truth]
func (b inputBreach) String() string {
	return fmt.Sprintf("store %s (%d rows): %s %s, over its %s allowance (internal/store/designstore)",
		b.store.name, b.store.rows, b.input, formatBytes(b.measured), formatBytes(b.allowed))
}

// inputBreaches judges one store's figures against the ceiling's allowances,
// or judges nothing when the store is not at the design size.
func inputBreaches(r result) []inputBreach {
	if r.size.rows != designstore.Rows {
		return nil
	}
	var out []inputBreach
	for _, in := range ceilingInputs {
		if got := in.of(r); got > in.allowance {
			out = append(out, inputBreach{store: r.size, input: in.name, measured: got, allowed: in.allowance})
		}
	}
	return out
}

// renderCeilingInputs states the input verdict. A run that measured no store
// at the design size says so in as many words: it judged nothing, and a blank
// section would read as a pass.
func renderCeilingInputs(results []result, found []inputBreach) string {
	var b strings.Builder
	var limits []string
	for _, in := range ceilingInputs {
		limits = append(limits, fmt.Sprintf("%s within %s", in.name, formatBytes(in.allowance)))
	}
	fmt.Fprintf(&b, "\nCEILING INPUTS: at the design size (%d rows), %s.\n", designstore.Rows, strings.Join(limits, ", "))
	judged := false
	for _, r := range results {
		judged = judged || r.size.rows == designstore.Rows
	}
	for _, br := range found {
		fmt.Fprintf(&b, "  OVER  %s\n", br)
	}
	switch {
	case !judged:
		b.WriteString("  not judged: no store at the design size was measured (--sizes design)\n")
	case len(found) == 0:
		b.WriteString("  none over allowance\n")
	}
	return b.String()
}
