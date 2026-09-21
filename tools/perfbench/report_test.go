package main

import (
	"strings"
	"testing"
	"time"
)

func fixedResults() []result {
	reads := []sample{
		{probe: probe{name: "quickstart"}, min: 70 * time.Millisecond, max: 80 * time.Millisecond, runs: repeats},
		{probe: probe{name: "backlog"}, min: 7970 * time.Millisecond, max: 8200 * time.Millisecond, runs: repeats},
	}
	return []result{
		{size: size{name: "empty", rows: 0}, bytes: 20_000, samples: reads},
		{size: size{name: "today", rows: 118}, bytes: 4_100_000, samples: reads},
	}
}

// The report's contract is that every measured figure reaches the reader: each
// size becomes a column, each probe a row, the min and the max both printed, and
// the store's bytes stated per size. A figure measured and then dropped in
// formatting is the failure this pins.
func TestRenderReportStatesEveryMeasuredFigure(t *testing.T) {
	got := renderReport(fixedResults(), conditions{goos: "darwin", goarch: "arm64", cpus: 12})
	for _, want := range []string{
		"darwin/arm64", "12 CPUs",
		"empty (0 rows)", "today (118 rows)",
		"quickstart", "backlog",
		"0.07s", "0.08s", // backlog's min and max
		"7.97s", "8.20s",
		"store bytes", "20.00 KB", "4.10 MB",
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
	got := renderReport(fixedResults(), conditions{goos: "linux", goarch: "amd64", cpus: 2})
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
	got := renderReport(nil, conditions{goos: "darwin", goarch: "arm64", cpus: 1})
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
