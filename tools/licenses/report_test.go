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

	for _, want := range []string{
		"| github.com/a/a | v1.0.0 | MIT |",
		"| github.com/b/b | v2.0.0 | Apache-2.0 |",
		"| github.com/c/c | v3.0.0 | MIT |",
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
	if want := "| github.com/a/a | - | MIT |"; !strings.Contains(buf.String(), want) {
		t.Errorf("output missing %q\n%s", want, buf.String())
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
