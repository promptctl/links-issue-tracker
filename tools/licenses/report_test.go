package main

import (
	"strings"
	"testing"
)

func TestWriteReport(t *testing.T) {
	entries := []Entry{
		{Module: Module{Path: "github.com/a/a", Version: "v1.0.0"}, LicenseName: "MIT"},
		{Module: Module{Path: "github.com/b/b", Version: "v2.0.0"}, LicenseName: "Apache-2.0"},
		{Module: Module{Path: "github.com/c/c", Version: "v3.0.0"}, LicenseName: "MIT"},
	}

	var buf strings.Builder
	if err := WriteReport(&buf, entries); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	out := buf.String()

	// Rows are pinned to their full width, trailing `|` included. An
	// unterminated prefix stays green when a column is added or removed, which
	// is how the Source column's own rendering once went unverified.
	for _, want := range []string{
		"| github.com/a/a | v1.0.0 | MIT | - |",
		"| github.com/b/b | v2.0.0 | Apache-2.0 | - |",
		"| github.com/c/c | v3.0.0 | MIT | - |",
		"| Apache-2.0 | 1 |",
		"| MIT | 2 |",
		"3 third-party components",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestWriteReportEmptyVersionRendersPlaceholder(t *testing.T) {
	// parseModuleList (modules.go) deliberately accepts an empty version for
	// a local `replace` directive; the report must render it as a visible
	// cell, not an invisible one that breaks the table's column alignment.
	entries := []Entry{
		{Module: Module{Path: "github.com/a/a", Version: ""}, LicenseName: "MIT"},
	}
	var buf strings.Builder
	if err := WriteReport(&buf, entries); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if want := "| github.com/a/a | - | MIT | - |"; !strings.Contains(buf.String(), want) {
		t.Errorf("output missing %q\n%s", want, buf.String())
	}
}

// TestWriteReportSourceColumnNamesTheReplacement is links-licensing-c0ce.15's
// acceptance criterion for the human-readable half: LICENSE-REPORT.md must say,
// on the row itself, that a component's source came from a coordinate other
// than the one the Module column names.
//
// The three rows are the three shapes, and each is pinned to its full width so
// a column silently dropped or reordered fails here. The module and directory
// rows carry DISTINCT source values rather than both reading "-", which is what
// keeps this from passing if the Source cell were wired to the Version column's
// placeholder or to a constant.
func TestWriteReportSourceColumnNamesTheReplacement(t *testing.T) {
	entries := replacementEntries

	var buf strings.Builder
	if err := WriteReport(&buf, entries); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"| github.com/dolthub/dolt/go | v0.40.5 | Apache-2.0 | github.com/promptctl/dolt/go@v0.40.5-later |",
		"| github.com/dolthub/driver | v0.2.1 | Apache-2.0 | ./internal/vendor/dolthub-driver |",
		"| github.com/spf13/cobra | v1.8.0 | Apache-2.0 | - |",
		// The header must gain the column too, or every row's fourth cell is
		// markdown a renderer drops on the floor.
		"| Module | Version | License | Source |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}

	// A reader meeting a bare coordinate in the Source column has to be told
	// what the column means; without the legend the substitution is disclosed
	// but not explained.
	if !strings.Contains(out, "The Source column names where a component's source came from when that is not the component named in the first column") {
		t.Errorf("report has a Source column but no legend explaining it\n%s", out)
	}
}

func TestWriteReportSummaryOrderIsSorted(t *testing.T) {
	entries := []Entry{
		{Module: Module{Path: "github.com/z/z", Version: "v1"}, LicenseName: "Zlib"},
		{Module: Module{Path: "github.com/a/a", Version: "v1"}, LicenseName: "Apache-2.0"},
	}
	var buf strings.Builder
	if err := WriteReport(&buf, entries); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	out := buf.String()
	apacheIdx := strings.Index(out, "| Apache-2.0 | 1 |")
	zlibIdx := strings.Index(out, "| Zlib | 1 |")
	if apacheIdx == -1 || zlibIdx == -1 || apacheIdx > zlibIdx {
		t.Fatalf("summary rows not alphabetically sorted:\n%s", out)
	}
}
