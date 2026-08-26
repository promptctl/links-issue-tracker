package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestSyncStalenessLines pins the pure rendering: which report/age
// combinations produce a warning, and exactly what each line says. Table
// style mirrors TestPrintSyncFreshness, the sibling this shares its
// resolve/render split with.
func TestSyncStalenessLines(t *testing.T) {
	t.Parallel()
	freshAge := 2 * time.Hour
	staleAge := 25 * time.Hour

	cases := []struct {
		name          string
		report        doctorSyncReport
		fetchAge      time.Duration
		fetchAgeKnown bool
		wantLines     int
		wantSubstrs   []string
		dontWant      []string
	}{
		{
			name:      "no remote emits nothing",
			report:    doctorSyncReport{Kind: doctorSyncNoRemote},
			wantLines: 0,
		},
		{
			name:      "unresolved emits nothing",
			report:    doctorSyncReport{Kind: doctorSyncUnresolved, Detail: "boom"},
			wantLines: 0,
		},
		{
			name: "up to date and freshly fetched emits nothing",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetchAge:      freshAge,
			fetchAgeKnown: true,
			wantLines:     0,
		},
		{
			name: "fetch age unknown (never fetched yet) does not warn",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetchAgeKnown: false,
			wantLines:     0,
		},
		{
			name: "ahead warns and names the push fix",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 3,
			}},
			fetchAge:      freshAge,
			fetchAgeKnown: true,
			wantLines:     1,
			wantSubstrs:   []string{"sync:", "3 local change(s) not pushed to origin/master", "as of last fetch", "lit sync push"},
		},
		{
			name: "stale fetch warns and names the fetch fix",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetchAge:      staleAge,
			fetchAgeKnown: true,
			wantLines:     1,
			wantSubstrs:   []string{"sync:", "last successful fetch from origin/master", "ago", "over 24 hours", "lit sync fetch"},
		},
		{
			name: "ahead AND stale fetch produce both lines",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 1,
			}},
			fetchAge:      staleAge,
			fetchAgeKnown: true,
			wantLines:     2,
			wantSubstrs:   []string{"not pushed", "last successful fetch"},
		},
		{
			name: "diverged does not duplicate the sync-failure escalation",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 2, Behind: 3,
			}},
			fetchAge:      freshAge,
			fetchAgeKnown: true,
			wantLines:     0,
			dontWant:      []string{"not pushed"},
		},
		{
			name: "diverged AND stale fetch still warns about the fetch (independent conditions)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 2, Behind: 3,
			}},
			fetchAge:      staleAge,
			fetchAgeKnown: true,
			wantLines:     1,
			wantSubstrs:   []string{"last successful fetch"},
			dontWant:      []string{"not pushed"},
		},
		{
			name: "behind alone does not warn (auto-receive fast-forwards it)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Behind: 4,
			}},
			fetchAge:      freshAge,
			fetchAgeKnown: true,
			wantLines:     0,
		},
		{
			name: "never synced alone (no local Ahead signal) does not warn",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: false,
			}},
			fetchAge:      freshAge,
			fetchAgeKnown: true,
			wantLines:     0,
		},
		{
			name: "boundary: exactly the threshold warns (>=, not >)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetchAge:      unfetchedStalenessThreshold,
			fetchAgeKnown: true,
			wantLines:     1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := syncStalenessLines(tc.report, tc.fetchAge, tc.fetchAgeKnown)
			if len(lines) != tc.wantLines {
				t.Fatalf("syncStalenessLines() = %v (%d lines), want %d", lines, len(lines), tc.wantLines)
			}
			joined := strings.Join(lines, "\n")
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(joined, want) {
					t.Fatalf("syncStalenessLines() = %q, missing %q", joined, want)
				}
			}
			for _, unwanted := range tc.dontWant {
				if strings.Contains(joined, unwanted) {
					t.Fatalf("syncStalenessLines() = %q, must not contain %q", joined, unwanted)
				}
			}
		})
	}
}

// TestMarkFetchSuccessAndLastFetchSuccessAge pins the marker's round trip: no
// marker reads as unknown (not zero-age, not infinite-age), and writing it
// makes the age observable and monotonically increasing from that write.
func TestMarkFetchSuccessAndLastFetchSuccessAge(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	now := time.Now()

	if _, ok := lastFetchSuccessAge(ws, now); ok {
		t.Fatalf("lastFetchSuccessAge() ok = true before any fetch ever succeeded")
	}

	if err := markFetchSuccess(ws); err != nil {
		t.Fatalf("markFetchSuccess() error = %v", err)
	}
	age, ok := lastFetchSuccessAge(ws, now.Add(90*time.Minute))
	if !ok {
		t.Fatalf("lastFetchSuccessAge() ok = false after markFetchSuccess")
	}
	if age < 89*time.Minute || age > 91*time.Minute {
		t.Fatalf("lastFetchSuccessAge() = %v, want ~90m", age)
	}
}

// TestLastFetchSuccessAgeSurfacesNonNotExistStatErrorsToStderr pins the
// distinction lastFetchSuccessAge draws between "marker does not exist" (a
// real, silent domain state) and any other stat error (a real operational
// failure, surfaced to stderr). Pointing StorageDir's marker path through a
// PARENT that is a regular file, not a directory, makes os.Stat return
// ENOTDIR — a genuine, portable, deterministic non-IsNotExist error on any
// OS/user, unlike a permission-denied fixture, which real CI often ignores
// when run as root.
func TestLastFetchSuccessAgeSurfacesNonNotExistStatErrorsToStderr(t *testing.T) {
	tmp := t.TempDir()
	notADir := filepath.Join(tmp, "not-a-directory")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file error = %v", err)
	}
	ws := workspace.Info{Location: workspace.Location{StorageDir: notADir}}

	stderr := captureStderr(t, func() {
		if _, ok := lastFetchSuccessAge(ws, time.Now()); ok {
			t.Errorf("lastFetchSuccessAge() ok = true for an unreadable marker path")
		}
	})
	if !strings.Contains(stderr, "fetch-success marker unreadable") {
		t.Fatalf("lastFetchSuccessAge() stderr = %q, want it to report the unreadable marker", stderr)
	}
}

// TestMarkFetchSuccessBackdatedMarkerIsStale exercises the marker as
// syncStalenessLines' resolveSyncStalenessWarning boundary reads it: a marker
// backdated past the threshold makes lastFetchSuccessAge report a stale age,
// exactly the input that triggers the "stale fetch" warning line above.
func TestMarkFetchSuccessBackdatedMarkerIsStale(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	if err := markFetchSuccess(ws); err != nil {
		t.Fatalf("markFetchSuccess() error = %v", err)
	}
	backdated := time.Now().Add(-30 * time.Hour)
	if err := os.Chtimes(fetchSuccessMarkerPath(ws), backdated, backdated); err != nil {
		t.Fatalf("os.Chtimes() error = %v", err)
	}
	age, ok := lastFetchSuccessAge(ws, time.Now())
	if !ok {
		t.Fatalf("lastFetchSuccessAge() ok = false for a backdated marker")
	}
	if age < unfetchedStalenessThreshold {
		t.Fatalf("lastFetchSuccessAge() = %v, want >= threshold %v", age, unfetchedStalenessThreshold)
	}
}

// TestPrintSyncStalenessWarningResolvesAgainstRealWorkspace drives
// printSyncStalenessWarning's boundary step (resolveDoctorSyncFreshness +
// lastFetchSuccessAge) against a real git repo with no remote — the common
// single-machine case — and confirms it stays silent rather than printing a
// no-remote warning: this banner is supplementary, not a diagnostic in its
// own right, matching resolveDoctorSyncFreshness's own no-remote handling.
func TestPrintSyncStalenessWarningResolvesAgainstRealWorkspace(t *testing.T) {
	_, ws := initBootstrapTestRepo(t)
	ctx := context.Background()
	st, err := store.OpenSync(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("store.OpenSync() error = %v", err)
	}
	defer st.Close()

	var buf bytes.Buffer
	if err := printSyncStalenessWarning(ctx, &buf, ws, st, time.Now()); err != nil {
		t.Fatalf("printSyncStalenessWarning() error = %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("printSyncStalenessWarning() = %q, want silence for a no-remote workspace", buf.String())
	}
}

// TestFetchSuccessMarkerPathLivesUnderStorageDir pins the marker's location
// so a future refactor cannot accidentally point it outside the workspace's
// own storage directory (e.g. a shared/global path that would leak state
// across workspaces).
func TestFetchSuccessMarkerPathLivesUnderStorageDir(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: "/tmp/some-workspace/.lit"}}
	got := fetchSuccessMarkerPath(ws)
	want := filepath.Join("/tmp/some-workspace/.lit", "fetch-success.last")
	if got != want {
		t.Fatalf("fetchSuccessMarkerPath() = %q, want %q", got, want)
	}
}
