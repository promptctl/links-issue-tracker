package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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
	origin := remoteName(t, "origin")

	cases := []struct {
		name        string
		report      doctorSyncReport
		fetch       *remoteFetch
		wantLines   int
		wantSubstrs []string
		dontWant    []string
	}{
		{
			name:      "unresolved emits nothing",
			report:    doctorSyncReport{Kind: doctorSyncUnresolved, Detail: "boom"},
			wantLines: 0,
		},
		{
			name:        "unresolved still warns about a stale fetch",
			report:      doctorSyncReport{Kind: doctorSyncUnresolved, Detail: "boom"},
			fetch:       &remoteFetch{remote: origin, age: staleAge},
			wantLines:   1,
			wantSubstrs: []string{"sync: last successful fetch from origin was", "lit sync fetch"},
		},
		{
			name: "up to date and freshly fetched emits nothing",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge},
			wantLines: 0,
		},
		{
			name: "fetch age unknown (never fetched yet) does not warn",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			wantLines: 0,
		},
		{
			name: "ahead warns and names the push fix",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 3,
			}},
			fetch:       &remoteFetch{remote: origin, age: freshAge},
			wantLines:   1,
			wantSubstrs: []string{"sync:", "3 local change(s) not pushed to origin/master", "as of last fetch", "lit sync push"},
		},
		{
			name: "stale fetch warns and names the fetch fix",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetch:       &remoteFetch{remote: origin, age: staleAge},
			wantLines:   1,
			wantSubstrs: []string{"sync:", "last successful fetch from origin was", "ago", "at least 24 hours old", "lit sync fetch"},
		},
		{
			name: "ahead AND stale fetch produce both lines",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 1,
			}},
			fetch:       &remoteFetch{remote: origin, age: staleAge},
			wantLines:   2,
			wantSubstrs: []string{"not pushed", "last successful fetch"},
		},
		{
			name: "diverged does not duplicate the sync-failure escalation",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 2, Behind: 3,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge},
			wantLines: 0,
			dontWant:  []string{"not pushed"},
		},
		{
			name: "diverged AND stale fetch still warns about the fetch (independent conditions)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 2, Behind: 3,
			}},
			fetch:       &remoteFetch{remote: origin, age: staleAge},
			wantLines:   1,
			wantSubstrs: []string{"last successful fetch"},
			dontWant:    []string{"not pushed"},
		},
		{
			name: "behind alone does not warn (auto-receive fast-forwards it)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Behind: 4,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge},
			wantLines: 0,
		},
		{
			name: "never synced alone (no local Ahead signal) does not warn",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: false,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge},
			wantLines: 0,
		},
		{
			name: "boundary: exactly the threshold warns (>=, not >)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetch:     &remoteFetch{remote: origin, age: unfetchedStalenessThreshold},
			wantLines: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := syncStalenessLines(tc.report, tc.fetch)
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
// marker reads as unknown (not zero-age, not infinite-age), a fetch of another
// remote leaves it unknown, and marking the remote itself makes the age
// observable and monotonically increasing from that write.
func TestMarkFetchSuccessAndLastFetchSuccessAge(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.CheckoutRoot{})}
	origin, fork := remoteName(t, "origin"), remoteName(t, "fork")
	now := time.Now()

	if _, ok := lastFetchSuccessAge(ws, origin, now); ok {
		t.Fatalf("lastFetchSuccessAge() ok = true before any fetch ever succeeded")
	}
	if err := markFetchSuccess(ws, fork); err != nil {
		t.Fatalf("markFetchSuccess(fork) error = %v", err)
	}
	if _, ok := lastFetchSuccessAge(ws, origin, now); ok {
		t.Fatalf("lastFetchSuccessAge(origin) ok = true after only fork was fetched")
	}

	if err := markFetchSuccess(ws, origin); err != nil {
		t.Fatalf("markFetchSuccess(origin) error = %v", err)
	}
	age, ok := lastFetchSuccessAge(ws, origin, now.Add(90*time.Minute))
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
// failure, surfaced to stderr). A NUL byte in StorageDir makes os.Stat refuse
// the marker path as an invalid argument: a non-IsNotExist error on every
// platform and for every user, root included, which a permission fixture is
// not. A regular file where a directory belongs is not either: Windows
// reports that path not found.
func TestLastFetchSuccessAgeSurfacesNonNotExistStatErrorsToStderr(t *testing.T) {
	ws := workspace.Info{Location: workspace.Location{StorageDir: filepath.Join(t.TempDir(), "storage\x00")}, Remotes: workspace.NewRemotes(testEnv(), workspace.CheckoutRoot{})}

	stderr := captureStderr(t, func() {
		if _, ok := lastFetchSuccessAge(ws, remoteName(t, "origin"), time.Now()); ok {
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
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.CheckoutRoot{})}
	origin := remoteName(t, "origin")
	if err := markFetchSuccess(ws, origin); err != nil {
		t.Fatalf("markFetchSuccess() error = %v", err)
	}
	backdated := time.Now().Add(-30 * time.Hour)
	if err := os.Chtimes(fetchSuccessMarkerPath(ws, origin), backdated, backdated); err != nil {
		t.Fatalf("os.Chtimes() error = %v", err)
	}
	age, ok := lastFetchSuccessAge(ws, origin, time.Now())
	if !ok {
		t.Fatalf("lastFetchSuccessAge() ok = false for a backdated marker")
	}
	if age < unfetchedStalenessThreshold {
		t.Fatalf("lastFetchSuccessAge() = %v, want >= threshold %v", age, unfetchedStalenessThreshold)
	}
}

// TestPrintSyncStalenessWarningResolvesAgainstRealWorkspace drives
// printStalenessWarning's boundary step (resolveDoctorSyncFreshness +
// lastTargetFetch) against a real git repo with no remote — the common
// single-machine case — and confirms it stays silent rather than printing a
// no-remote warning: this banner is supplementary, not a diagnostic in its
// own right, matching resolveDoctorSyncFreshness's own no-remote handling.
func TestPrintSyncStalenessWarningResolvesAgainstRealWorkspace(t *testing.T) {
	_, ws := initBootstrapTestRepo(t)
	ctx := underInvocation(context.Background())
	st, err := engine.Open(ctx, testEnv(), engine.Sync, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	defer st.Close()

	// A stale marker on disk for a remote the workspace does not have: with no
	// sync remote there is nothing to fetch, so no fetch line, whatever is on
	// disk.
	origin := remoteName(t, "origin")
	if err := markFetchSuccess(ws, origin); err != nil {
		t.Fatalf("markFetchSuccess() error = %v", err)
	}
	backdated := time.Now().Add(-30 * time.Hour)
	if err := os.Chtimes(fetchSuccessMarkerPath(ws, origin), backdated, backdated); err != nil {
		t.Fatalf("os.Chtimes() error = %v", err)
	}

	var buf bytes.Buffer
	if err := printStalenessWarning(ctx, testEnv(), &buf, ws, st, time.Now()); err != nil {
		t.Fatalf("printStalenessWarning() error = %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("printStalenessWarning() = %q, want silence for a no-remote workspace", buf.String())
	}
}

// TestFetchSuccessMarkerPathLivesUnderStorageDir pins the marker's location
// so a future refactor cannot accidentally point it outside the workspace's
// own storage directory (e.g. a shared/global path that would leak state
// across workspaces).
func TestFetchSuccessMarkerPathLivesUnderStorageDir(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: "/tmp/some-workspace/.lit"}, Remotes: workspace.NewRemotes(testEnv(), workspace.CheckoutRoot{})}
	got := fetchSuccessMarkerPath(ws, remoteName(t, "origin"))
	want := filepath.Join("/tmp/some-workspace/.lit", "fetched.origin")
	if got != want {
		t.Fatalf("fetchSuccessMarkerPath() = %q, want %q", got, want)
	}
}
