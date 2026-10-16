package main

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/statusline"
)

// testdata/render.txt is what cc-candybar 1.130.5 printed for a checkout
// working on t-1 with no phase, whose last lit next found nothing: a solid
// arrow after the menu, then a thin one between two cells of one colour.
func TestCellsReadsTheBarCandybarPrints(t *testing.T) {
	printed, err := os.ReadFile(filepath.Join("testdata", "render.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cells(string(printed))
	if err != nil {
		t.Fatalf("cells: %v", err)
	}
	want := []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1)", "next nothing ready"}
	if !slices.Equal(got, want) {
		t.Fatalf("cells = %q, want %q", got, want)
	}
}

// A print the check cannot read as a bar fails the case rather than passing it
// with whatever cells it could make out. Each print is the readable one but
// for the one fault it is named for.
func TestCellsRefusesAPrintItCannotRead(t *testing.T) {
	const (
		ok   = "✓ /c.json5: config OK\n"
		menu = "\ue0b0 \x1b]8;;m\x1b\\🍫\x1b]8;;\x1b\\ \ue0b0"
		bar  = menu + " phase plan \ue0b0\n"
	)
	if got, err := cells(ok + bar); err != nil || !slices.Equal(got, []string{"phase plan"}) {
		t.Fatalf("the readable print: cells = %q, %v; want [\"phase plan\"]", got, err)
	}
	for name, printed := range map[string]string{
		"nothing printed":             "",
		"no config OK line":           "✓ /c.json5: config failed\n" + bar,
		"two bar lines":               ok + bar + bar,
		"no menu cell":                ok + "\ue0b0 phase plan \ue0b0 next nothing ready \ue0b0\n",
		"text before the first arrow": ok + "x" + menu + " phase plan \ue0b0\n",
		"no closing arrow":            ok + menu + " phase plan\n",
		"unread escape":               ok + menu + " \x1b[2Kphase plan \ue0b0\n",
	} {
		if got, err := cells(printed); err == nil {
			t.Errorf("%s: cells = %q, want an error", name, got)
		}
	}
}

// The no-phase-key case is a file an older lit wrote: working_on as this lit
// writes it, less the phase key. A null phase and a missing one hide the cell
// alike, so the render cannot tell the case did not drop it; this does.
func TestOlderLitWritesWorkingOnWithoutAPhaseKey(t *testing.T) {
	i := slices.IndexFunc(cases, func(c renderCase) bool { return c.name == "no-phase-key" })
	if i < 0 {
		t.Fatal("no no-phase-key case")
	}
	dir := t.TempDir()
	loc := statusline.LocationOf(dir, filepath.Join(dir, ".git"))
	if err := cases[i].write(loc); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(loc.File)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		SchemaVersion int                        `json:"schema_version"`
		WorkingOn     map[string]json.RawMessage `json:"working_on"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(file.WorkingOn))
	want := []string{"recorded_at", "rollup_progress", "ticket"}
	if file.SchemaVersion != 1 || !slices.Equal(got, want) {
		t.Fatalf("schema_version %d, working_on keys %q; want 1 and %q", file.SchemaVersion, got, want)
	}
}

// A segment whose template renders only spaces is drawn as a blank cell
// between two arrows, as cc-candybar 1.130.5 draws it; it reads as a cell, so
// a case expecting the segment hidden fails.
func TestCellsKeepsABlankCell(t *testing.T) {
	printed := "✓ /c.json5: config OK\n\ue0b0 \x1b]8;;m\x1b\\🍫\x1b]8;;\x1b\\ \ue0b0   \ue0b0 next nothing ready \ue0b0\n"
	got, err := cells(printed)
	if err != nil {
		t.Fatalf("cells: %v", err)
	}
	if want := []string{"", "next nothing ready"}; !slices.Equal(got, want) {
		t.Fatalf("cells = %q, want %q", got, want)
	}
}

// The run's verdicts are its gate: a case that renders other cells, or does
// not render, fails the run, and every case still prints its line.
func TestCheckFailsTheRunOnAnyCaseThatDoesNotRender(t *testing.T) {
	cases := []renderCase{
		{name: "renders", cells: []string{"phase plan"}},
		{name: "other-cells", cells: []string{"phase plan"}},
		{name: "no-render", cells: []string{}},
	}
	rendered := map[string][]string{"renders": {"phase plan"}, "other-cells": {}}
	render := func(c renderCase) ([]string, error) {
		if got, ok := rendered[c.name]; ok {
			return got, nil
		}
		return nil, errors.New("plugin did not load")
	}
	var stdout strings.Builder
	if code := check(&stdout, io.Discard, cases, render); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	want := `candybar-check: case=renders verdict=ok cells=["phase plan"]
candybar-check: case=other-cells verdict=mismatch cells=[]
candybar-check: case=no-render verdict=error cells=[]
candybar-check: cc-candybar=1.130.5 cases=3 failed=2
`
	if stdout.String() != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout.String(), want)
	}
	if code := check(io.Discard, io.Discard, cases[:1], render); code != 0 {
		t.Errorf("every case rendered: exit %d, want 0", code)
	}
}
