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

	ran := func(depth GCMode) compactionAttempt {
		return compactionAttempt{Depth: depth, Ran: true}
	}

	if got := compactionReport(ran(GCNewGen), nil); got != "" {
		t.Fatalf("compactionReport(newgen ran, nil) = %q, want empty — the routine pass is not news", got)
	}
	if got := compactionReport(ran(GCFull), nil); got == "" {
		t.Fatal("compactionReport(full ran, nil) was empty; a deep pass explains a slow push and must be reported")
	}
	// The compaction still ran, so the report must say so AND name the
	// measurement failure — not imply nothing happened.
	got := compactionReport(ran(GCNewGen), errors.New("disk on fire"))
	if !strings.Contains(got, "newgen") || !strings.Contains(got, "disk on fire") {
		t.Fatalf("compactionReport with a failed measurement = %q, want both the depth that ran and the cause", got)
	}
}

// A pass that never ran must produce no report at ALL — not even the deep-pass
// line, and not even when the measurement also failed. This is the arm that
// stops the error path from announcing "ran full pass, rewriting the old
// generation" beside the error saying the full pass failed, which is precisely
// what it used to do when the caller decided this instead of the reporter.
// [LAW:one-source-of-truth]
func TestCompactionReportSaysNothingAboutAPassThatNeverRan(t *testing.T) {
	t.Parallel()

	for _, depth := range []GCMode{GCNewGen, GCFull} {
		attempt := compactionAttempt{Depth: depth} // Ran stays false
		if got := compactionReport(attempt, nil); got != "" {
			t.Fatalf("compactionReport(%s not run, nil) = %q, want empty — nothing ran, so nothing is worth reporting", depth, got)
		}
		got := compactionReport(attempt, errors.New("disk on fire"))
		if got != "" {
			t.Fatalf("compactionReport(%s not run, measure err) = %q, want empty — this claims a pass ran that did not", depth, got)
		}
	}
}

// The attempt is the single carrier of "what happened", so the rendering of it
// must not invent a reclaim for a pass that produced none. An attempt that ran
// carries its detail; one that did not carries a depth and nothing else, which
// is exactly what the contract reserves an empty Detail for.
func TestCompactionAttemptRendersOnlyWhatHappened(t *testing.T) {
	t.Parallel()

	ran := compactionAttempt{Depth: GCFull, Ran: true}.outcome("journal 1 B -> 0 B")
	if !ran.Ran || ran.Depth != GCFull || ran.Detail == "" {
		t.Fatalf("a completed pass rendered as %+v; it must carry Ran, its depth, and its account", ran)
	}

	notRun := compactionAttempt{Depth: GCFull}.outcome("journal 1 B -> 0 B")
	if notRun.Ran {
		t.Fatalf("an attempt that never ran rendered Ran = true (%+v)", notRun)
	}
	if notRun.Detail != "" {
		t.Fatalf("an attempt that never ran carried Detail %q; there was no reclaim to describe", notRun.Detail)
	}
	if notRun.Depth != GCFull {
		t.Fatalf("an attempt that never ran lost its depth (%+v); the trail needs to say which one failed", notRun)
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
