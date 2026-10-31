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
			fetch:       &remoteFetch{remote: origin, age: staleAge, basis: lastFetchSucceeded},
			wantLines:   1,
			wantSubstrs: []string{"sync: last successful fetch from origin was", "lit sync fetch"},
		},
		{
			name: "up to date and freshly fetched emits nothing",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge, basis: lastFetchSucceeded},
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
			fetch:       &remoteFetch{remote: origin, age: freshAge, basis: lastFetchSucceeded},
			wantLines:   1,
			wantSubstrs: []string{"sync:", "3 local change(s) not pushed to origin/master", "as of last fetch", "lit sync push"},
		},
		{
			name: "stale fetch warns and names the fetch fix",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetch:       &remoteFetch{remote: origin, age: staleAge, basis: lastFetchSucceeded},
			wantLines:   1,
			wantSubstrs: []string{"sync:", "last successful fetch from origin was", "ago", "at least 24 hours old", "lit sync fetch"},
		},
		{
			name: "ahead AND stale fetch produce both lines",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 1,
			}},
			fetch:       &remoteFetch{remote: origin, age: staleAge, basis: lastFetchSucceeded},
			wantLines:   2,
			wantSubstrs: []string{"not pushed", "last successful fetch"},
		},
		{
			name: "diverged does not duplicate the sync-failure escalation",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 2, Behind: 3,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge, basis: lastFetchSucceeded},
			wantLines: 0,
			dontWant:  []string{"not pushed"},
		},
		{
			name: "diverged AND stale fetch still warns about the fetch (independent conditions)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: 2, Behind: 3,
			}},
			fetch:       &remoteFetch{remote: origin, age: staleAge, basis: lastFetchSucceeded},
			wantLines:   1,
			wantSubstrs: []string{"last successful fetch"},
			dontWant:    []string{"not pushed"},
		},
		{
			name: "behind alone does not warn (auto-receive fast-forwards it)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Behind: 4,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge, basis: lastFetchSucceeded},
			wantLines: 0,
		},
		{
			name: "never synced alone (no local Ahead signal) does not warn",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: false,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge, basis: lastFetchSucceeded},
			wantLines: 0,
		},
		{
			name: "boundary: exactly the threshold warns (>=, not >)",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetch:     &remoteFetch{remote: origin, age: unfetchedStalenessThreshold, basis: lastFetchSucceeded},
			wantLines: 1,
		},
		{
			name: "no success on record past the threshold warns, without claiming one happened",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetch:       &remoteFetch{remote: origin, age: staleAge, basis: noFetchSucceeded},
			wantLines:   1,
			wantSubstrs: []string{"no successful fetch from origin on record", "lit started tracking it", "lit sync fetch"},
			dontWant:    []string{"last successful fetch"},
		},
		{
			name: "no success on record under the threshold is silent",
			report: doctorSyncReport{Kind: doctorSyncResolved, Freshness: storage.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true,
			}},
			fetch:     &remoteFetch{remote: origin, age: freshAge, basis: noFetchSucceeded},
			wantLines: 0,
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

// TestFetchRecordRoundTrip pins the record's states: none reads as missing
// (not zero-age, not infinite-age), a fetch of another remote leaves it
// missing, starting it reads as no success on record, starting it again keeps
// its clock, a success replaces it, and a later start leaves the success.
func TestFetchRecordRoundTrip(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}
	origin, fork := remoteName(t, "origin"), remoteName(t, "fork")
	now := time.Now()

	if fetch, missing := readFetchRecord(testStderr(t), ws, origin, now); fetch != nil || !missing {
		t.Fatalf("readFetchRecord() = %+v, %v before any record; want nil, missing", fetch, missing)
	}
	markFetchSuccess(testStderr(t), ws, fork)
	if _, missing := readFetchRecord(testStderr(t), ws, origin, now); !missing {
		t.Fatalf("readFetchRecord(origin) not missing after only fork was fetched")
	}

	if err := startFetchRecord(ws, origin); err != nil {
		t.Fatalf("startFetchRecord() error = %v", err)
	}
	started := now.Add(-30 * time.Hour)
	if err := os.Chtimes(fetchRecordPath(ws, origin), started, started); err != nil {
		t.Fatal(err)
	}
	if err := startFetchRecord(ws, origin); err != nil {
		t.Fatalf("second startFetchRecord() error = %v", err)
	}
	fetch, _ := readFetchRecord(testStderr(t), ws, origin, now)
	if fetch == nil || fetch.basis != noFetchSucceeded || fetch.age < 29*time.Hour {
		t.Fatalf("readFetchRecord() after start = %+v; want noFetchSucceeded aged from the first start (~30h)", fetch)
	}

	markFetchSuccess(testStderr(t), ws, origin)
	if err := startFetchRecord(ws, origin); err != nil {
		t.Fatalf("startFetchRecord() after success error = %v", err)
	}
	fetch, _ = readFetchRecord(testStderr(t), ws, origin, now.Add(90*time.Minute))
	if fetch == nil || fetch.basis != lastFetchSucceeded {
		t.Fatalf("readFetchRecord() after success = %+v; want lastFetchSucceeded", fetch)
	}
	if fetch.age < 89*time.Minute || fetch.age > 91*time.Minute {
		t.Fatalf("readFetchRecord() age = %v, want ~90m", fetch.age)
	}
}

// TestReadFetchRecordSurfacesNonNotExistErrorsToStderr pins the distinction
// readFetchRecord draws between "record does not exist" (a real, silent
// domain state that starts tracking) and any other open error (a real
// operational failure, surfaced to stderr, and not missing). A NUL byte in
// StorageDir makes os.Open refuse the path as an invalid argument: a
// non-IsNotExist error on every platform and for every user, root included,
// which a permission fixture is not.
func TestReadFetchRecordSurfacesNonNotExistErrorsToStderr(t *testing.T) {
	ws := workspace.Info{Location: workspace.Location{StorageDir: filepath.Join(t.TempDir(), "storage\x00")}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}

	var stderr bytes.Buffer
	if fetch, missing := readFetchRecord(&stderr, ws, remoteName(t, "origin"), time.Now()); fetch != nil || missing {
		t.Errorf("readFetchRecord() = %+v, %v for an unreadable path; want nil, not missing", fetch, missing)
	}
	if !strings.Contains(stderr.String(), "fetch record for origin unreadable") {
		t.Fatalf("readFetchRecord() stderr = %q, want it to report the unreadable record", stderr.String())
	}
}

// TestReadFetchRecordReportsAndKeepsUnknownContent pins that a record holding
// content this lit never writes — a newer lit's, say — is reported, is not
// read as either basis, and is not missing, so no record is started over it.
func TestReadFetchRecordReportsAndKeepsUnknownContent(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}
	origin := remoteName(t, "origin")
	if err := os.WriteFile(fetchRecordPath(ws, origin), []byte("v9 something\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if fetch, missing := readFetchRecord(&stderr, ws, origin, time.Now()); fetch != nil || missing {
		t.Fatalf("readFetchRecord() = %+v, %v for unknown content; want nil, not missing", fetch, missing)
	}
	if !strings.Contains(stderr.String(), "does not know") {
		t.Fatalf("readFetchRecord() stderr = %q, want it to report the unknown content", stderr.String())
	}
}

// TestMarkFetchSuccessBackdatedMarkerIsStale exercises the marker as the
// banner reads it: a success record backdated past the threshold reads as a
// stale lastFetchSucceeded age, exactly the input that triggers the "stale
// fetch" warning line above.
func TestMarkFetchSuccessBackdatedMarkerIsStale(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}
	origin := remoteName(t, "origin")
	markFetchSuccess(testStderr(t), ws, origin)
	backdated := time.Now().Add(-30 * time.Hour)
	if err := os.Chtimes(fetchRecordPath(ws, origin), backdated, backdated); err != nil {
		t.Fatalf("os.Chtimes() error = %v", err)
	}
	fetch, _ := readFetchRecord(testStderr(t), ws, origin, time.Now())
	if fetch == nil || fetch.basis != lastFetchSucceeded {
		t.Fatalf("readFetchRecord() = %+v for a backdated success; want lastFetchSucceeded", fetch)
	}
	if fetch.age < unfetchedStalenessThreshold {
		t.Fatalf("readFetchRecord() age = %v, want >= threshold %v", fetch.age, unfetchedStalenessThreshold)
	}
}

// TestPrintSyncStalenessWarningResolvesAgainstRealWorkspace drives
// printStalenessWarning's boundary step (resolveDoctorSyncFreshness +
// trackTargetFetch) against a real git repo with no remote — the common
// single-machine case — and confirms it stays silent rather than printing a
// no-remote warning: this banner is supplementary, not a diagnostic in its
// own right, matching resolveDoctorSyncFreshness's own no-remote handling.
func TestPrintSyncStalenessWarningResolvesAgainstRealWorkspace(t *testing.T) {
	_, ws := initBootstrapTestRepo(t)
	ctx := underInvocation(context.Background())
	st, err := engine.Open(ctx, testEnv(), testStderr(t), engine.Sync, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	defer st.Close()

	// A stale marker on disk for a remote the workspace does not have: with no
	// sync remote there is nothing to fetch, so no fetch line, whatever is on
	// disk.
	origin := remoteName(t, "origin")
	markFetchSuccess(testStderr(t), ws, origin)
	backdated := time.Now().Add(-30 * time.Hour)
	if err := os.Chtimes(fetchRecordPath(ws, origin), backdated, backdated); err != nil {
		t.Fatalf("os.Chtimes() error = %v", err)
	}

	var buf bytes.Buffer
	if err := printStalenessWarning(ctx, testStderr(t), testEnv(), &buf, ws, st, time.Now()); err != nil {
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
	ws := workspace.Info{Location: workspace.Location{StorageDir: "/tmp/some-workspace/.lit"}, Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}
	got := fetchRecordPath(ws, remoteName(t, "origin"))
	want := filepath.Join("/tmp/some-workspace/.lit", "fetched.origin")
	if got != want {
		t.Fatalf("fetchRecordPath() = %q, want %q", got, want)
	}
}
