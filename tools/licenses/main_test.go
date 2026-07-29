package main

import (
	"os"
	"strings"
	"testing"

	lc "github.com/google/licenseclassifier"
)

// litPkg is the actual package the release binary builds, referenced by its
// full import path (not "./cmd/lit") so `go list -deps` resolves it
// regardless of the test binary's working directory.
const litPkg = "github.com/promptctl/links-issue-tracker/cmd/lit"

// TestEndToEndAgainstLitCoversDolt is this ticket's acceptance criterion
// (links-supply-chain-w6m9.1) expressed as a test: run the real generator
// against the real release package, and confirm the bundle carries the full
// license text of a known linked dependency (github.com/dolthub/dolt) and
// the report classifies it correctly.
func TestEndToEndAgainstLitCoversDolt(t *testing.T) {
	mods, err := LinkedModules(litPkg)
	if err != nil {
		t.Fatalf("LinkedModules(%s): %v", litPkg, err)
	}

	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		t.Fatalf("build classifier: %v", err)
	}

	var entries []Entry
	var doltEntry *Entry
	for _, m := range mods {
		licensePath, err := FindLicenseFile(m.Dir)
		if err != nil {
			t.Fatalf("%s@%s: %v", m.Path, m.Version, err)
		}
		textBytes, err := os.ReadFile(licensePath)
		if err != nil {
			t.Fatalf("read %s: %v", licensePath, err)
		}
		name, confidence := Classify(classifier, string(textBytes))
		e := Entry{Module: m, LicenseFile: licensePath, LicenseName: name, Confidence: confidence, Text: string(textBytes)}
		entries = append(entries, e)
		if m.Path == "github.com/dolthub/dolt/go" {
			doltEntry = &entries[len(entries)-1]
		}
	}

	if doltEntry == nil {
		t.Fatalf("github.com/dolthub/dolt/go not found among %d linked modules", len(mods))
	}
	if doltEntry.LicenseName != "Apache-2.0" {
		t.Errorf("dolt classified as %q, want Apache-2.0", doltEntry.LicenseName)
	}
	if !strings.Contains(doltEntry.Text, "Apache License") {
		t.Errorf("dolt's bundled text doesn't contain the Apache License header:\n%s", doltEntry.Text)
	}

	var bundle, report strings.Builder
	if err := WriteBundle(&bundle, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if err := WriteReport(&report, entries); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if !strings.Contains(bundle.String(), "github.com/dolthub/dolt/go") {
		t.Error("bundle doesn't mention github.com/dolthub/dolt/go")
	}
	if !strings.Contains(report.String(), "| github.com/dolthub/dolt/go | ") {
		t.Error("report doesn't list github.com/dolthub/dolt/go")
	}
}

// TestLinkedModulesDeterministic pins the "deterministic output for a fixed
// dependency set" acceptance criterion: two independent runs against the same
// package must resolve to byte-identical module lists.
func TestLinkedModulesDeterministic(t *testing.T) {
	first, err := LinkedModules(litPkg)
	if err != nil {
		t.Fatalf("LinkedModules: %v", err)
	}
	second, err := LinkedModules(litPkg)
	if err != nil {
		t.Fatalf("LinkedModules: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("module count differs across runs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("module %d differs across runs: %+v vs %+v", i, first[i], second[i])
		}
	}
}
