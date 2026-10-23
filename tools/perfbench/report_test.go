package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"
)

// Asking for help is a request that was granted. Returning flag.ErrHelp would
// have main print "flag: help requested" under the usage text and exit 1,
// spending the failure code on a success — the same defect as the reverse.
func TestRunTreatsHelpAsSuccess(t *testing.T) {
	var out, progress bytes.Buffer
	if err := run([]string{"-h"}, &out, &progress); err != nil {
		t.Errorf("run(-h) = %v, want nil; asking for help is not a failure", err)
	}
	if !strings.Contains(progress.String(), "-sizes") {
		t.Errorf("run(-h) did not write usage to the progress stream:\n%s", progress.String())
	}
	if out.Len() != 0 {
		t.Errorf("run(-h) wrote %q to the report stream; usage is not a report", out.String())
	}
}

// A stray positional argument must fail rather than be ignored. flag stops at
// the first non-flag word and hands the rest back, so `perfbench 0,118` (meant
// as --sizes) would otherwise run the default table and report it as though it
// were what was asked for.
func TestRunRefusesAStrayPositionalArgument(t *testing.T) {
	var out, progress bytes.Buffer
	err := run([]string{"0,118"}, &out, &progress)
	if err == nil {
		t.Fatal("run(0,118) returned no error; the default table would be reported as the requested one")
	}
	if !strings.Contains(err.Error(), "--sizes") {
		t.Errorf("error %q does not point at the flag the caller probably meant", err)
	}
	if out.Len() != 0 {
		t.Errorf("run wrote a report (%q) despite a stray argument", out.String())
	}
}

// An explicitly-empty --sizes is a mistake, not a request for the default. Only
// omitting the flag selects the default envelope.
func TestRunRefusesAnExplicitlyEmptySizes(t *testing.T) {
	var out, progress bytes.Buffer
	err := run([]string{"--sizes", ""}, &out, &progress)
	if err == nil {
		t.Fatal("run(--sizes \"\") returned no error; the default table would be reported as the requested one")
	}
	if out.Len() != 0 {
		t.Errorf("run wrote a report (%q) despite an empty --sizes", out.String())
	}
}

// A bad flag value must fail before anything is built or generated, and must
// not be mistaken for the help request above.
func TestRunRejectsABadSizeBeforeDoingAnyWork(t *testing.T) {
	var out, progress bytes.Buffer
	err := run([]string{"--sizes", "12x"}, &out, &progress)
	if err == nil {
		t.Fatal("run(--sizes 12x) returned no error")
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Errorf("a bad size was reported as a help request: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("run wrote a report (%q) despite a bad size", out.String())
	}
}

// Each result gets its OWN sample slice with its own values. Sharing one slice
// between results would make the fixture blind to any defect in how the report
// indexes per result — every column would render identically whether the code
// read the right result or not.
func fixedResults() []result {
	samplesAt := func(quickMin, quickMax, backlogMin, backlogMax time.Duration) []sample {
		return []sample{
			{probe: probe{name: "quickstart"}, outcome: completed{min: quickMin, max: quickMax}},
			{probe: probe{name: "backlog"}, outcome: completed{min: backlogMin, max: backlogMax}},
		}
	}
	return []result{
		{size: size{name: "empty", rows: 0}, bytes: 20_000, compacted: 15_000,
			samples: samplesAt(70*time.Millisecond, 80*time.Millisecond, 7970*time.Millisecond, 8200*time.Millisecond)},
		{size: size{name: "today", rows: 118}, bytes: 4_100_000, compacted: 3_900_000,
			samples: samplesAt(110*time.Millisecond, 120*time.Millisecond, 3330*time.Millisecond, 4440*time.Millisecond),
			history: historyCost{edits: 100, before: 3_900_000, after: 5_250_000}},
	}
}

// The report's contract is that every measured figure reaches the reader: each
// size becomes a column, each probe a row, the min and the max both printed, and
// the store's bytes stated per size. A figure measured and then dropped in
// formatting is the failure this pins.
func TestRenderReportStatesEveryMeasuredFigure(t *testing.T) {
	got := renderReport(probes, fixedResults(), latencyBudget, conditions{goos: "darwin", goarch: "arm64", cpus: 12})
	for _, want := range []string{
		"darwin/arm64", "12 CPUs",
		"empty (0 rows)", "today (118 rows)",
		"quickstart", "backlog",
		"0.07s", "0.08s", // the empty column's quickstart min and max
		"7.97s", "8.20s", // the empty column's backlog
		"0.11s", "0.12s", // the 118-row column's quickstart — distinct values, so a
		"3.33s", "4.44s", // report reading the wrong result could not still pass
		"store bytes", "20.00 KB", "4.10 MB",
		"compacted bytes", "15.00 KB", "3.90 MB",
		// 1.35 MB over 100 edits; the empty store had no row to edit.
		"bytes/commit", "13.50 KB", "—",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report omits %q\n--- report ---\n%s", want, got)
		}
	}
}

// The epic's own figures are quoted in seconds to two decimals ("lit backlog
// 7.97s"), so a regenerated table has to be comparable to the pasted one
// without converting units.
func TestRenderReportMatchesTheEpicsUnits(t *testing.T) {
	got := renderReport(probes, fixedResults(), latencyBudget, conditions{goos: "linux", goarch: "amd64", cpus: 2})
	if strings.Contains(got, "ms") {
		t.Errorf("report prints milliseconds; the epic states its figures in seconds:\n%s", got)
	}
	if !strings.Contains(got, "MIN") {
		t.Errorf("report does not tell the reader the figure is a minimum:\n%s", got)
	}
}

// Nothing measured must print no rows. A header listing every probe name over
// columns that were never measured is a report that looks like a result.
func TestRenderReportOverNoResultsNamesNoProbes(t *testing.T) {
	got := renderReport(probes, nil, latencyBudget, conditions{goos: "darwin", goarch: "arm64", cpus: 1})
	for _, name := range []string{"backlog", "quickstart", "next"} {
		if strings.Contains(got, name) {
			t.Errorf("report over zero results names probe %q:\n%s", name, got)
		}
	}
}

func TestFormatBytesUsesDecimalMegabytes(t *testing.T) {
	// 1e6 bytes is 1.00 MB decimal and 0.95 MiB binary; the epic's 50 MB
	// ceiling and 188 MB observation are decimal, and switching base would
	// move every number by 5% with nothing on screen to say so.
	if got := formatBytes(1_000_000); got != "1.00 MB" {
		t.Errorf("formatBytes(1e6) = %q, want %q", got, "1.00 MB")
	}
}

// The empty store is the control for lit's fixed on-disk cost, and at two
// decimals of MB it would print "0.00 MB" — a real number rendered as zero.
func TestFormatBytesDoesNotPrintASmallStoreAsZero(t *testing.T) {
	got := formatBytes(4_300)
	if strings.HasPrefix(got, "0.00") {
		t.Errorf("formatBytes(4300) = %q, which reads as an empty store rather than a small one", got)
	}
	if got != "4.30 KB" {
		t.Errorf("formatBytes(4300) = %q, want %q", got, "4.30 KB")
	}
}

// A probe one store lacks must not move another command's figures. The empty
// control has no `show`, and it is the first column; a report reading cells by
// position would print the populated store's show time on the next probe's
// row, with nothing to say so.
func TestRenderReportKeepsEveryFigureUnderItsOwnProbe(t *testing.T) {
	results := []result{
		{size: size{name: "empty", rows: 0}, samples: []sample{
			{probe: probe{name: "ls"}, outcome: completed{min: 110 * time.Millisecond, max: 120 * time.Millisecond}},
			{probe: probe{name: "doctor"}, outcome: completed{min: 210 * time.Millisecond, max: 220 * time.Millisecond}},
		}},
		{size: size{name: "today", rows: 118}, samples: []sample{
			{probe: probe{name: "ls"}, outcome: completed{min: 130 * time.Millisecond, max: 140 * time.Millisecond}},
			{probe: probe{name: "show"}, outcome: completed{min: 330 * time.Millisecond, max: 340 * time.Millisecond}},
			{probe: probe{name: "doctor"}, outcome: completed{min: 430 * time.Millisecond, max: 440 * time.Millisecond}},
		}},
	}
	got := renderReport(probes, results, latencyBudget, conditions{goos: "linux", goarch: "amd64", cpus: 4})
	for name, want := range map[string]string{
		"ls":     "0.11s (0.12s) 0.13s (0.14s)",
		"show":   "— 0.33s (0.34s)",
		"doctor": "0.21s (0.22s) 0.43s (0.44s)",
	} {
		row := reportRow(t, got, name)
		if cells := strings.Join(strings.Fields(row)[1:], " "); cells != want {
			t.Errorf("row %q = %q, want its figures %q\n--- report ---\n%s", name, row, want, got)
		}
	}
}

// reportRow is the table line whose first column is name.
func reportRow(t *testing.T, report, name string) string {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, name+"  ") {
			return line
		}
	}
	t.Fatalf("report has no row for %q:\n%s", name, report)
	return ""
}

// The verdict: a completed probe breaches when its min is over budget, not
// when only its max is, since noise only adds time; a killed one always does.
func TestBreachesJudgesTheMinAndEveryKill(t *testing.T) {
	r := result{size: size{name: "today", rows: 118}, samples: []sample{
		{probe: probe{name: "ls"}, outcome: completed{min: 900 * time.Millisecond, max: 3 * time.Second}},
		{probe: probe{name: "backlog"}, outcome: completed{min: 2500 * time.Millisecond, max: 2600 * time.Millisecond}},
		{probe: probe{name: "next"}, outcome: killed{round: 3, after: 30 * time.Second}},
	}}
	got := breaches(r, time.Second)
	if len(got) != 2 || got[0].probe != "backlog" || got[1].probe != "next" {
		t.Fatalf("breaches = %v, want backlog (min over) and next (killed), and not ls (only its max is over)", got)
	}
	for i, want := range []string{
		"store today (118 rows): backlog took 2.50s, 2.5x its 1s budget",
		"store today (118 rows): next was killed in its round 3, still running at 30s, over 30x its 1s budget",
	} {
		if got[i].String() != want {
			t.Errorf("breach %d reads %q, want %q", i, got[i], want)
		}
	}
}

// The report ends with the verdict, so the table and the reason the run failed
// are read together, and says so explicitly when nothing is over.
func TestRenderReportStatesTheBudgetVerdict(t *testing.T) {
	cond := conditions{goos: "linux", goarch: "amd64", cpus: 4}
	over := renderReport(probes, fixedResults(), latencyBudget, cond)
	for _, want := range []string{"BUDGET: every probe under 1s", "OVER  store empty (0 rows): backlog took 7.97s", "OVER  store today (118 rows): backlog took 3.33s"} {
		if !strings.Contains(over, want) {
			t.Errorf("report omits %q\n--- report ---\n%s", want, over)
		}
	}
	if strings.Contains(over, "quickstart took") {
		t.Errorf("report names quickstart, which is under budget, as over:\n%s", over)
	}
	under := renderReport(probes, fixedResults(), time.Minute, cond)
	if !strings.Contains(under, "none over budget") {
		t.Errorf("report with nothing over budget does not say so:\n%s", under)
	}
}
