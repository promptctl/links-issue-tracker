package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

func TestTakeListLatestAndPrune(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	export := model.Export{Version: 1, WorkspaceID: "ws-test", ExportedAt: time.Now().UTC()}
	first := take(t, storageDir, Budget, export)
	export.WorkspaceID = "ws-test-2"
	second := take(t, storageDir, Budget, export)
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
	if first.Refreshed {
		t.Errorf("Take(first).Refreshed = true, want false: the set held nothing")
	}
	other := take(t, storageDir, Budget, model.Export{Version: 1, WorkspaceID: "ws-other", ExportedAt: time.Now().UTC()})
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
	setMTime(t, legacy, time.Now().Add(-time.Hour))
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

// setMTime sets a snapshot's mtime: the way a test makes a filesystem clock
// the take must beat, whatever the real one's resolution.
func setMTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("set mtime of %s: %v", path, err)
	}
}

// takeN takes n distinct exports in a row and returns their snapshots in the
// order taken.
func takeN(t *testing.T, storageDir string, keep, n int) []*Written {
	t.Helper()
	taken := make([]*Written, 0, n)
	for i := range n {
		taken = append(taken, take(t, storageDir, keep, model.Export{Version: 1, WorkspaceID: fmt.Sprintf("ws-%d", i)}))
	}
	return taken
}

// Takes made back to back are all kept, listed newest-first in the order they
// were taken, and a prune removes the oldest taken, whatever the clock
// resolution: the snapshot seeded an hour ahead is what a clock stepped back
// looks like, and a take made now must still be newer than it.
func TestTakesKeepTheOrderTheyWereTaken(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	stale := take(t, storageDir, Budget, model.Export{Version: 1, WorkspaceID: "ws-before-the-step"})
	setMTime(t, stale.Path, time.Now().Add(time.Hour))
	const n, keep = 6, 4
	taken := takeN(t, storageDir, keep, n)

	list, err := List(storageDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got, want []string
	for _, s := range list {
		got = append(got, s.Path)
	}
	for i := n - 1; i >= n-keep; i-- {
		want = append(want, taken[i].Path)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("List = %v, want the last %d takes newest-first %v", got, keep, want)
	}
	latest, err := Latest(storageDir)
	if err != nil || latest == nil || latest.Path != taken[n-1].Path {
		t.Fatalf("Latest = %+v, %v; want the last take %s", latest, err, taken[n-1].Path)
	}
	if _, err := os.Stat(stale.Path); !os.IsNotExist(err) {
		t.Errorf("the snapshot from before the step survived a prune past it: stat error = %v", err)
	}
	if !latest.Created.Equal(list[0].Created) {
		t.Errorf("Take reported Created %v, List reads %v", latest.Created, list[0].Created)
	}
}

// A take of content the set holds becomes the newest even when it is stamped
// against the snapshots beside it, never its own old mtime.
func TestARefreshedTakeIsTheNewest(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	first := take(t, storageDir, Budget, model.Export{Version: 1, WorkspaceID: "ws-a"})
	setMTime(t, first.Path, time.Now().Add(time.Hour))
	take(t, storageDir, Budget, model.Export{Version: 1, WorkspaceID: "ws-b"})
	again := take(t, storageDir, Budget, model.Export{Version: 1, WorkspaceID: "ws-a"})
	latest, err := Latest(storageDir)
	if err != nil || latest == nil || latest.Path != first.Path || !again.Refreshed {
		t.Fatalf("Latest = %+v, %v; want the refreshed %s", latest, err, first.Path)
	}
}

// Snapshots whose mtimes tie (copied by a tool that rounds them) list by name
// descending, the same order a prune removes from the tail of.
func TestListOrdersTiedMTimesByNameDescending(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	at := time.Now().Add(-time.Hour)
	for _, name := range []string{"20260101-000000.json", "20260102-000000.json", "20260103-000000.json"} {
		path := filepath.Join(storageDir, "backups", name)
		if _, err := syncfile.WriteAtomic(path, model.Export{Version: 1, WorkspaceID: name}); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		setMTime(t, path, at)
	}
	list, err := List(storageDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	if want := []string{"20260103-000000.json", "20260102-000000.json", "20260101-000000.json"}; !slices.Equal(names, want) {
		t.Errorf("List = %v, want %v", names, want)
	}
}

// nextStamp puts a take strictly after the newest other snapshot and leaves a
// mtime the clock already put there alone.
func TestNextStamp(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		newestOther  time.Time
		written, out time.Time
	}{
		{"no other snapshot", time.Time{}, at, at},
		{"the clock is well past it", at.Add(-time.Hour), at, at},
		{"the same tick", at, at, at.Add(orderStep)},
		{"inside the step", at.Add(-orderStep / 2), at, at.Add(orderStep / 2)},
		{"exactly a step past", at.Add(-orderStep), at, at},
		{"the clock stepped back", at.Add(time.Hour), at, at.Add(time.Hour + orderStep)},
	}
	for _, c := range cases {
		if got := nextStamp(c.newestOther, c.written); !got.Equal(c.out) {
			t.Errorf("%s: nextStamp(%v, %v) = %v, want %v", c.name, c.newestOther, c.written, got, c.out)
		}
	}
}

// A take reports whether its mtime was moved past the clock's: not when the
// set holds nothing newer, and so when it holds a snapshot dated ahead.
func TestTakeReportsWhetherItWasRestamped(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	first := take(t, storageDir, Budget, model.Export{Version: 1, WorkspaceID: "ws-a"})
	setMTime(t, first.Path, time.Now().Add(time.Hour))
	second := take(t, storageDir, Budget, model.Export{Version: 1, WorkspaceID: "ws-b"})
	if first.Restamped || !second.Restamped {
		t.Errorf("Restamped = %v, %v; want false, true", first.Restamped, second.Restamped)
	}
}
