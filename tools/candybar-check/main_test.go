package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
// with whatever cells it could make out.
func TestCellsRefusesAPrintItCannotRead(t *testing.T) {
	const ok = "✓ /c.json5: config OK\n"
	for name, printed := range map[string]string{
		"nothing printed":   "",
		"no config OK line": "[🍫](x)  phase plan\n",
		"two bar lines":     ok + "\x1b]8;;m\x1b\\🍫\x1b]8;;\x1b\\\nphase plan\n",
		"no menu cell":      ok + "phase plan  next nothing ready\n",
		"unread escape":     ok + "\x1b]8;;m\x1b\\🍫\x1b]8;;\x1b\\  \x1b[2Kphase plan\n",
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
