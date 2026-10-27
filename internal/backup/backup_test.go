package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
	"github.com/promptctl/primitives/filelock"
)

// take runs Take for export and fails the test unless it wrote a snapshot.
func take(t *testing.T, storageDir string, keep int, export model.Export) *Written {
	t.Helper()
	taken, err := Take(context.Background(), storageDir, keep, func() (model.Export, error) { return export, nil })
	if err != nil || taken == nil || taken.Written == nil {
		t.Fatalf("Take(%s) = %+v, %v", export.WorkspaceID, taken, err)
	}
	return taken.Written
}

// age sets a snapshot's mtime ago into the past. A take's mtime is the
// filesystem's clock, which on Linux ticks coarsely enough that two takes
// microseconds apart can tie, so a test that depends on their order sets it.
func age(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	at := time.Now().Add(-ago)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
}

func TestTakeListLatestAndPrune(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	export := model.Export{Version: 1, WorkspaceID: "ws-test", ExportedAt: time.Now().UTC()}
	first := take(t, storageDir, Budget, export)
	age(t, first.Path, 2*time.Hour)
	export.WorkspaceID = "ws-test-2"
	second := take(t, storageDir, Budget, export)
	age(t, second.Path, time.Hour)
	export.WorkspaceID = "ws-test-3"
	third := take(t, storageDir, 2, export)
	if first.Pruned != 0 || second.Pruned != 0 || third.Pruned != 1 {
		t.Errorf("Pruned = %d, %d, %d; want 0, 0, 1", first.Pruned, second.Pruned, third.Pruned)
	}

	list, err := List(storageDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Path != third.Path || list[1].Path != second.Path {
		t.Fatalf("List = %+v, want %s then %s, the oldest pruned", list, third.Path, second.Path)
	}
	latest, err := Latest(storageDir)
	if err != nil || latest == nil || latest.Path != third.Path {
		t.Fatalf("Latest = %+v, %v; want %s", latest, err, third.Path)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Errorf("oldest snapshot survived a prune past it: stat error = %v", err)
	}
}

// A keep that admits no snapshot is refused before anything is read or
// written.
func TestTakeRejectsNonPositiveKeep(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	taken, err := Take(context.Background(), storageDir, 0, func() (model.Export, error) {
		t.Fatalf("Take(keep=0) read the store")
		return model.Export{}, nil
	})
	if err == nil || taken != nil {
		t.Fatalf("Take(keep=0) = %+v, %v; want an error and no snapshot", taken, err)
	}
	if _, err := os.Stat(filepath.Join(storageDir, "backups")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Take(keep=0) created the backups dir: stat error = %v", err)
	}
}

// A take reads the store, writes and prunes only while it holds the set's
// lock, so a holder in front of it keeps it from reading at all: a read made
// before the lock could land after a newer one, and a prune beside another
// take could remove the name that take just refreshed.
func TestTakeWaitsForTheSetsLock(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	release, acquired, err := filelock.Acquire(context.Background(), lockPath(storageDir), true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("hold the backups lock: acquired=%v, %v", acquired, err)
	}
	var readAt atomic.Pointer[time.Time]
	started := make(chan struct{})
	done := make(chan *Taken, 1)
	go func() {
		close(started)
		taken, err := Take(context.Background(), storageDir, Budget, func() (model.Export, error) {
			now := time.Now()
			readAt.Store(&now)
			return model.Export{Version: 1, WorkspaceID: "ws-test"}, nil
		})
		if err != nil {
			t.Errorf("Take after the release: %v", err)
		}
		done <- taken
	}()
	<-started
	time.Sleep(200 * time.Millisecond)
	releasedAt := time.Now()
	if err := release(); err != nil {
		t.Fatalf("release the backups lock: %v", err)
	}
	taken := <-done
	if at := readAt.Load(); at == nil || at.Before(releasedAt) {
		t.Fatalf("Take read the store at %v, want after the lock's release at %v", at, releasedAt)
	}
	if taken == nil || taken.Waited <= 0 {
		t.Errorf("Take = %+v, want the wait for the lock recorded", taken)
	}
}

// A take of content the set already holds lands on the snapshot that holds it,
// so repeated takes of an unchanged store leave one file, the newest, holding
// the newer take.
func TestTakeHoldsEachContentOnce(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	export := model.Export{Version: 1, WorkspaceID: "ws-test", ExportedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	first := take(t, storageDir, Budget, export)
	age(t, first.Path, 2*time.Hour)
	if first.Refreshed {
		t.Errorf("Take(first).Refreshed = true, want false: the set held nothing")
	}
	other := take(t, storageDir, Budget, model.Export{Version: 1, WorkspaceID: "ws-other", ExportedAt: time.Now().UTC()})
	age(t, other.Path, time.Hour)
	if other.Refreshed || other.Path == first.Path {
		t.Fatalf("Take(other) = %+v, want a new snapshot beside %s", other, first.Path)
	}

	export.ExportedAt = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	again := take(t, storageDir, Budget, export)
	if !again.Refreshed || again.Path != first.Path {
		t.Fatalf("Take(again) = %+v, want %s refreshed", again, first.Path)
	}
	list, err := List(storageDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Path != first.Path {
		t.Fatalf("List = %+v, want 2 snapshots with %s newest", list, first.Path)
	}
	payload, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("read %s: %v", first.Path, err)
	}
	held, _, err := syncfile.Decode(payload)
	if err != nil {
		t.Fatalf("decode %s: %v", first.Path, err)
	}
	if !held.ExportedAt.Equal(export.ExportedAt) {
		t.Errorf("snapshot exported_at = %v, want the newer take's %v", held.ExportedAt, export.ExportedAt)
	}
}

// A snapshot an older lit named by its time is listed and pruned by age with
// the rest, and never stands for content a new take holds.
func TestPruneAgesOutAnOlderLitsSnapshot(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	export := model.Export{Version: 1, WorkspaceID: "ws-test", ExportedAt: time.Now().UTC()}
	take(t, storageDir, Budget, export)
	legacy := filepath.Join(storageDir, "backups", "20260101-000000.000000000.json")
	if _, err := syncfile.WriteAtomic(legacy, export); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}
	age(t, legacy, time.Hour)
	taken := take(t, storageDir, Budget, export)
	if taken.Path == legacy {
		t.Fatalf("Take landed on the older lit's snapshot %s", legacy)
	}
	list, err := List(storageDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[1].Path != legacy {
		t.Fatalf("List = %+v, want the older lit's snapshot listed last", list)
	}
	take(t, storageDir, 1, export)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("older lit's snapshot survived a prune past it: stat error = %v", err)
	}
}
