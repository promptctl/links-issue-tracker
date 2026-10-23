package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
)

// snaps builds automatic snapshots newest first, one per kind letter: 'r' a
// reconcile snapshot, 'm' a migration one, 'd' a downgrade one. Each is size
// bytes and named for its position, so a test reads which ones went.
func snaps(size int64, kinds string) []sizedSnapshot {
	labels := map[rune]string{'r': reconcileSnapshotLabel, 'm': migrationSnapshotLabel, 'd': downgradeSnapshotLabel}
	out := make([]sizedSnapshot, 0, len(kinds))
	for i, k := range kinds {
		out = append(out, sizedSnapshot{
			Snapshot: dbsnapshot.Snapshot{Name: fmt.Sprintf("%d%c", i, k)},
			kind:     labels[k],
			bytes:    size,
		})
	}
	return out
}

func names(ss []sizedSnapshot) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func TestSnapshotsToPrune(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		f       footprint
		ceiling int64
		want    []string
	}{
		{
			name:    "room for every snapshot retires none",
			f:       footprint{chunkStore: 100, automatic: snaps(10, "rrrm")},
			ceiling: 1000,
			want:    []string{},
		},
		{
			// Three reconciles fit after the chunk store and the newest of
			// each kind; the fourth does not, and nothing older survives it.
			name:    "a budget that admits part of a kind retires the oldest",
			f:       footprint{chunkStore: 100, automatic: snaps(10, "rrrrrrm")},
			ceiling: 100 + 10*5,
			want:    []string{"4r", "5r"},
		},
		{
			name:    "one byte over retires one snapshot, not all but one per kind",
			f:       footprint{chunkStore: 100, automatic: snaps(10, "rrrrrm")},
			ceiling: 100 + 10*6 - 1,
			want:    []string{"4r"},
		},
		{
			// An older kind's newest snapshot is reserved before any newer
			// snapshot of another kind takes the room it needs.
			name:    "the newest of each kind is reserved before the rest spend the room",
			f:       footprint{chunkStore: 100, automatic: snaps(10, "rrrmd")},
			ceiling: 100 + 10*4,
			want:    []string{"2r"},
		},
		{
			name:    "a budget below zero keeps only the newest of each kind",
			f:       footprint{chunkStore: 5000, otherSnapshots: 5000, automatic: snaps(10, "rmrmdd")},
			ceiling: 100,
			want:    []string{"2r", "3m", "5d"},
		},
		{
			name:    "the per-kind cap holds however much room there is",
			f:       footprint{automatic: snaps(1, "rrrrrrrrrrrrm")},
			ceiling: 1 << 40,
			want:    []string{"10r", "11r"},
		},
		{
			name:    "the user's snapshots and the temp files spend the room retention may not touch",
			f:       footprint{chunkStore: 100, tempTableFiles: 20, otherSnapshots: 30, automatic: snaps(10, "rrr")},
			ceiling: 100 + 20 + 30 + 10*2,
			want:    []string{"2r"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := names(snapshotsToPrune(tc.f, tc.ceiling))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("snapshotsToPrune() = %v, want %v", got, tc.want)
			}
		})
	}
}

// measureFootprint reads each part from the directory its owner writes it to,
// and itemizes only the snapshots an automatic kind stamped: a user snapshot
// and an in-flight take land in the user's part, never in what retention may
// retire.
func TestMeasureFootprintReadsEveryPart(t *testing.T) {
	t.Parallel()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	write := func(path string, n int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshotsDir := dbsnapshot.DirFor(doltRoot)
	write(filepath.Join(nomsDir(doltRoot), "journal"), 100)
	write(filepath.Join(nomsDir(doltRoot), oldGenDirName, "a"+archiveFileExt), 50)
	write(filepath.Join(TempTableFilesDir(doltRoot), tempTableFilePrefix+"1"), 7)
	write(filepath.Join(remoteCacheBaseFor(doltRoot), "k", "repo.git", "pack"), 1000)
	write(filepath.Join(snapshotsDir, "2000-"+migrationSnapshotLabel+"-2000", "f"), 30)
	write(filepath.Join(snapshotsDir, "3000-"+reconcileSnapshotLabel+"-3000", "f"), 20)
	write(filepath.Join(snapshotsDir, "1000-mine", "f"), 5)
	write(filepath.Join(snapshotsDir, ".take-in-flight", "f"), 3)

	f, err := measureFootprint(doltRoot)
	if err != nil {
		t.Fatalf("measureFootprint() error = %v", err)
	}
	if f.chunkStore != 150 || f.tempTableFiles != 7 || f.remoteCache != 1000 || f.otherSnapshots != 8 {
		t.Fatalf("footprint = %+v, want chunk store 150, temp 7, remote cache 1000, other snapshots 8", f)
	}
	gotAutomatic := make([]string, 0, len(f.automatic))
	for _, s := range f.automatic {
		gotAutomatic = append(gotAutomatic, fmt.Sprintf("%s %s %d", s.Name, s.kind, s.bytes))
	}
	wantAutomatic := []string{
		"3000-" + reconcileSnapshotLabel + "-3000 " + reconcileSnapshotLabel + " 20",
		"2000-" + migrationSnapshotLabel + "-2000" + " " + migrationSnapshotLabel + " 30",
	}
	if !slices.Equal(gotAutomatic, wantAutomatic) {
		t.Fatalf("automatic snapshots = %v, want %v, newest first", gotAutomatic, wantAutomatic)
	}

	fp := f.contract(1)
	if got := fp.GovernedBytes(); got != 150+7+50+8 {
		t.Fatalf("governed bytes = %d, want every part but the remote cache (%d)", got, 150+7+50+8)
	}
}

// An absent store has no size, so measuring one fails, naming the chunk
// store: a store has a chunk store from the moment it exists, and a reading of
// 0 bytes would pass a wrong path or a replaced store off as a measurement.
// The parts a store can truly lack (its snapshots, temp files and remote
// cache) still read as 0 when absent.
func TestMeasureFootprintOfAnAbsentStoreFails(t *testing.T) {
	t.Parallel()
	_, err := measureFootprint(filepath.Join(t.TempDir(), "dolt"))
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "chunk store") {
		t.Fatalf("measureFootprint() on an absent store error = %v, want the chunk store's absence", err)
	}
}

// The ceiling budgets for the history the default window keeps and the user
// snapshots the default budget keeps, so its figure and those defaults are
// one fact. A default that moves without the ceiling would leave the ceiling
// budgeting for a store no default setting produces. [LAW:one-source-of-truth]
func TestTheCeilingBudgetsTheDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := config.Load(environ.Of(os.Environ()), pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if got := cfg.Sync.HistoryWindow.ReRootAt(); got != designHistoryCommits {
		t.Fatalf("the default window re-roots at %d commits, but the ceiling budgets for %d", got, designHistoryCommits)
	}
	if got := cfg.Snapshot.RetentionBudget; got != designUserSnapshots {
		t.Fatalf("the default snapshot budget keeps %d user snapshots, but the ceiling budgets for %d", got, designUserSnapshots)
	}
}
