package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The empty store is zero imports, not an empty one: `lit import` refuses a
// spec with no issues, so a batch list that yielded one empty batch would fail
// generation at the size the epic uses as its control.
func TestImportBatchesYieldsNoBatchForTheEmptyStore(t *testing.T) {
	if got := importBatches(0); len(got) != 0 {
		t.Fatalf("importBatches(0) returned %d batches, want none", len(got))
	}
}

func TestImportBatchesBuildsTheRequestedRows(t *testing.T) {
	batches := importBatches(9)
	if len(batches) != 1 {
		t.Fatalf("importBatches(9) returned %d batches, want 1", len(batches))
	}
	recs := batches[0]
	if len(recs) != 9 {
		t.Fatalf("importBatches(9) produced %d records, want 9", len(recs))
	}
	// Row sizes are derived from this repository's own export (median
	// description 1310 bytes, title 69); a row padded to some other size makes
	// the byte figures incomparable to the real store.
	for i, r := range recs {
		if len(r.Description) != descriptionBytes {
			t.Errorf("record %d description is %d bytes, want %d", i, len(r.Description), descriptionBytes)
		}
		if len(r.Title) != titleBytes {
			t.Errorf("record %d title is %d bytes, want %d", i, len(r.Title), titleBytes)
		}
		if r.LocalID == "" || r.Topic == "" || r.Type == "" {
			t.Errorf("record %d is missing a key `lit import` requires: %+v", i, r)
		}
	}
	// Roughly a third blocked, matching the 42-of-118 the live store reports.
	// The first row has no predecessor, so 9 rows give edges at 3, 6 — hence 2.
	blocked := 0
	for _, r := range recs {
		if len(r.DependsOn) > 0 {
			blocked++
		}
	}
	if blocked != 2 {
		t.Errorf("importBatches(9) wired %d blocked rows, want 2 (indices 3 and 6)", blocked)
	}
	// An edge may only name another local_id in the same file — `lit import`'s
	// rule, and a spec violating it fails the whole import.
	ids := map[string]bool{}
	for _, r := range recs {
		ids[r.LocalID] = true
	}
	for _, r := range recs {
		for _, dep := range r.DependsOn {
			if !ids[dep] {
				t.Errorf("record %s depends on %q, which is not a local_id in the same spec", r.LocalID, dep)
			}
		}
	}
}

// Padding with repeated prose rather than a repeated character is deliberate: a
// run of identical bytes compresses to almost nothing in the store's chunker,
// which would understate every byte figure the tool reports.
func TestPadFillsToLengthWithoutBecomingOneRepeatedByte(t *testing.T) {
	got := pad("generated description ", 1310)
	if len(got) != 1310 {
		t.Fatalf("pad(...) produced %d bytes, want 1310", len(got))
	}
	distinct := map[rune]bool{}
	for _, r := range got {
		distinct[r] = true
	}
	if len(distinct) < 5 {
		t.Errorf("pad produced %d distinct bytes; an incompressible row understates store bytes", len(distinct))
	}
}

// Tolerance for a nonzero exit is confined to the one command that has a
// documented legitimate one. Widening any other probe's set would not fail a
// run or look wrong in the table — it would quietly let that command's failures
// set its fastest time, which is the exact defect okCodes exists to prevent, so
// the confinement is pinned rather than left to review.
func TestOnlyNextToleratesANonZeroExit(t *testing.T) {
	for _, p := range probes {
		for _, code := range p.okCodes {
			if code == 0 {
				continue
			}
			if p.name != "next" {
				t.Errorf("probe %q accepts exit %d; only `lit next` has a documented "+
					"nonzero answer (6, \"no ready work\" on the empty store), and every "+
					"other tolerated code lets that command's failures win the minimum",
					p.name, code)
			}
			if code != 6 {
				t.Errorf("probe %q accepts exit %d, which is not the documented "+
					"\"no ready work\" code 6", p.name, code)
			}
		}
	}
}

func TestEveryProbeDeclaresItsProvingExitCodes(t *testing.T) {
	for _, p := range probes {
		if len(p.okCodes) == 0 {
			t.Errorf("probe %q declares no expected exit codes, so any failure would be "+
				"recorded as its fastest run", p.name)
		}
	}
}

func TestParseSizes(t *testing.T) {
	if got, err := parseSizes(""); err != nil || len(got) != len(defaultSizes) {
		t.Errorf("parseSizes(\"\") = (%v, %v), want the default envelope", got, err)
	}
	got, err := parseSizes("0, 42,590")
	if err != nil {
		t.Fatalf("parseSizes error = %v", err)
	}
	want := []int{0, 42, 590}
	if len(got) != len(want) {
		t.Fatalf("parseSizes returned %d sizes, want %d", len(got), len(want))
	}
	for i, sz := range got {
		if sz.rows != want[i] {
			t.Errorf("size %d rows = %d, want %d", i, sz.rows, want[i])
		}
		if !strings.Contains(sz.name, "42") && i == 1 {
			t.Errorf("size %d name %q does not identify its row count", i, sz.name)
		}
	}
	for _, bad := range []string{"12x", "-1", ""} {
		if _, err := parseSizes("0," + bad); err == nil {
			t.Errorf("parseSizes(%q) accepted a value that is not a row count", "0,"+bad)
		}
	}
}

// Apparent size and recursive: the same store on two filesystems with different
// block sizes has to report the same number, or figures stop comparing across
// machines.
func TestStoreBytesTotalsApparentSizeRecursively(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "noms", "oldgen")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep"), make([]byte, 2_000), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := storeBytes(dir)
	if err != nil {
		t.Fatalf("storeBytes error = %v", err)
	}
	if got != 2_100 {
		t.Errorf("storeBytes = %d, want 2100 (apparent sizes, nested files included)", got)
	}
}

func TestStoreBytesReportsAMissingStore(t *testing.T) {
	if _, err := storeBytes(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("storeBytes over a nonexistent directory returned no error; a missing store is not a store of zero bytes")
	}
}
