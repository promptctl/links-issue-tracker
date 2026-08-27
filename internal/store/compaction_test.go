package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/store/chunks"
)

// The policy is a pure function of two numbers, so these assert the decision it
// reaches — never how it reaches it. A different implementation of the same
// contract passes unchanged. [LAW:behavior-not-structure]
func TestDueModeSelectsDepthByFootprint(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		footprint storeFootprint
		wantMode  GCMode
		wantDue   bool
	}{
		{
			name:      "an untouched store needs nothing",
			footprint: storeFootprint{},
			wantDue:   false,
		},
		{
			name:      "a journal below the threshold needs nothing",
			footprint: storeFootprint{JournalBytes: journalDueBytes - 1},
			wantDue:   false,
		},
		{
			name:      "a journal at the threshold earns a shallow pass",
			footprint: storeFootprint{JournalBytes: journalDueBytes},
			wantMode:  GCNewGen,
			wantDue:   true,
		},
		{
			name:      "archives below the threshold do not earn a deep pass",
			footprint: storeFootprint{OldGenArchives: archivesDueCount - 1},
			wantDue:   false,
		},
		{
			name:      "archives at the threshold earn a deep pass",
			footprint: storeFootprint{OldGenArchives: archivesDueCount},
			wantMode:  GCFull,
			wantDue:   true,
		},
		{
			// The deep pass collects the new generation too, so a store that
			// warrants both wants one deep pass rather than a shallow one now
			// and a deep one later.
			name: "a store warranting both is collected deeply, once",
			footprint: storeFootprint{
				JournalBytes:   journalDueBytes * 4,
				OldGenArchives: archivesDueCount,
			},
			wantMode: GCFull,
			wantDue:  true,
		},
		{
			// The gap this whole feature exists for: a workspace that never
			// pushes accumulates journal and never accumulates archives, so
			// the archive threshold alone would never fire for it.
			name:      "a never-pushed store is caught by the journal alone",
			footprint: storeFootprint{JournalBytes: journalDueBytes * 2, OldGenArchives: 0},
			wantMode:  GCNewGen,
			wantDue:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mode, due := dueMode(tc.footprint)
			if due != tc.wantDue {
				t.Fatalf("dueMode(%+v) due = %v, want %v", tc.footprint, due, tc.wantDue)
			}
			if due && mode != tc.wantMode {
				t.Fatalf("dueMode(%+v) mode = %v, want %v", tc.footprint, mode, tc.wantMode)
			}
		})
	}
}

// writeStore lays down the parts of a Dolt store layout the footprint reads,
// using the same constants the production derivation uses so the fixture cannot
// drift from what it stands in for.
func writeStore(t *testing.T, journalBytes int, archives int) string {
	t.Helper()
	root := t.TempDir()
	noms := filepath.Join(root, doltDatabaseName, dbfactory.DoltDir, dbfactory.DataDir)
	oldgen := filepath.Join(noms, oldGenDirName)
	if err := os.MkdirAll(oldgen, 0o755); err != nil {
		t.Fatalf("lay down store: %v", err)
	}
	if journalBytes > 0 {
		journal := filepath.Join(noms, chunks.JournalFileID)
		if err := os.WriteFile(journal, make([]byte, journalBytes), 0o644); err != nil {
			t.Fatalf("write journal: %v", err)
		}
	}
	for i := range archives {
		name := filepath.Join(oldgen, strings.Repeat("a", 8)+string(rune('a'+i%26))+"_"+itoa(i)+archiveFileExt)
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("write archive: %v", err)
		}
	}
	return root
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestMeasureFootprintReadsJournalAndArchives(t *testing.T) {
	t.Parallel()

	root := writeStore(t, 4096, 3)
	got, err := measureFootprint(root)
	if err != nil {
		t.Fatalf("measureFootprint() error = %v", err)
	}
	if got.JournalBytes != 4096 {
		t.Fatalf("JournalBytes = %d, want 4096", got.JournalBytes)
	}
	if got.OldGenArchives != 3 {
		t.Fatalf("OldGenArchives = %d, want 3", got.OldGenArchives)
	}
}

// A store with nothing in it is a real state with a real answer, not a failure:
// this is what every workspace looks like immediately after `lit init`, and a
// backstop that errored here would be loud on every fresh repo.
func TestMeasureFootprintReadsAnAbsentStoreAsEmpty(t *testing.T) {
	t.Parallel()

	got, err := measureFootprint(t.TempDir())
	if err != nil {
		t.Fatalf("measureFootprint() on an absent store error = %v", err)
	}
	if got != (storeFootprint{}) {
		t.Fatalf("measureFootprint() on an absent store = %+v, want the empty footprint", got)
	}
}

// Only Dolt's own archive suffix counts. The old generation also holds a
// manifest and a lock file, and counting those would inflate the archive count
// and trigger deep passes that reclaim nothing.
func TestMeasureFootprintCountsOnlyArchives(t *testing.T) {
	t.Parallel()

	root := writeStore(t, 0, 2)
	oldgen := filepath.Join(root, doltDatabaseName, dbfactory.DoltDir, dbfactory.DataDir, oldGenDirName)
	for _, name := range []string{"manifest", "LOCK"} {
		if err := os.WriteFile(filepath.Join(oldgen, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, err := measureFootprint(root)
	if err != nil {
		t.Fatalf("measureFootprint() error = %v", err)
	}
	if got.OldGenArchives != 2 {
		t.Fatalf("OldGenArchives = %d, want 2 (manifest and LOCK must not count)", got.OldGenArchives)
	}
}

func TestGCProcedureArgsRendersEachDepth(t *testing.T) {
	t.Parallel()

	shallow, err := gcProcedureArgs(GCNewGen)
	if err != nil {
		t.Fatalf("gcProcedureArgs(GCNewGen) error = %v", err)
	}
	if len(shallow) != 0 {
		t.Fatalf("gcProcedureArgs(GCNewGen) = %v, want no arguments", shallow)
	}

	deep, err := gcProcedureArgs(GCFull)
	if err != nil {
		t.Fatalf("gcProcedureArgs(GCFull) error = %v", err)
	}
	if len(deep) != 1 || deep[0] != "--full" {
		t.Fatalf("gcProcedureArgs(GCFull) = %v, want [--full]", deep)
	}
}

// Falling through to the shallow depth on an unrecognized mode would collect at
// the wrong depth and report success — exactly the silent wrong-work the typed
// depth exists to prevent.
func TestGCProcedureArgsRefusesAnUnknownDepth(t *testing.T) {
	t.Parallel()

	if _, err := gcProcedureArgs(GCMode(99)); err == nil {
		t.Fatal("gcProcedureArgs() accepted a depth outside the enum; it must refuse rather than pick one")
	}
}

func TestCompactionReportSpeaksOnlyWhenItHasSomethingToSay(t *testing.T) {
	t.Parallel()

	if got := compactionReport(GCNewGen, nil); got != "" {
		t.Fatalf("compactionReport(GCNewGen, nil) = %q, want empty — the routine pass is not news", got)
	}
	if got := compactionReport(GCFull, nil); got == "" {
		t.Fatal("compactionReport(GCFull, nil) was empty; a deep pass explains a slow push and must be reported")
	}
	// The compaction still ran, so the report must say so AND name the
	// measurement failure — not imply nothing happened.
	got := compactionReport(GCNewGen, errors.New("disk on fire"))
	if !strings.Contains(got, "newgen") || !strings.Contains(got, "disk on fire") {
		t.Fatalf("compactionReport with a failed measurement = %q, want both the depth that ran and the cause", got)
	}
}

func TestJoinMaintenanceDropsSilentReports(t *testing.T) {
	t.Parallel()

	if got := joinMaintenance("", ""); got != "" {
		t.Fatalf("joinMaintenance of two silent reports = %q, want empty", got)
	}
	if got := joinMaintenance("", "pruned"); got != "pruned" {
		t.Fatalf("joinMaintenance = %q, want %q", got, "pruned")
	}
	if got := joinMaintenance("compacted", "pruned"); got != "compacted; pruned" {
		t.Fatalf("joinMaintenance = %q, want %q", got, "compacted; pruned")
	}
}
