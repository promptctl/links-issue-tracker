package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The cadence decision is the load-bearing logic the policy ticket adds: only
// a write command under the on-change policy mirrors. Every other combination
// — read-mode commands, the opt-in on-push policy — stays silent. The truth
// table pins all four cells so neither axis can drift into a spurious push.
func TestShouldSyncAfterMutation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		access  app.AccessMode
		cadence config.SyncCadence
		want    bool
	}{
		{"write + on-change pushes", app.AccessWrite, config.SyncCadenceOnChange, true},
		{"write + on-push stays on the hook", app.AccessWrite, config.SyncCadenceOnPush, false},
		{"read + on-change never pushes", app.AccessRead, config.SyncCadenceOnChange, false},
		{"read + on-push never pushes", app.AccessRead, config.SyncCadenceOnPush, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSyncAfterMutation(tc.access, tc.cadence); got != tc.want {
				t.Fatalf("shouldSyncAfterMutation(%q, %q) = %v, want %v", tc.access, tc.cadence, got, tc.want)
			}
		})
	}
}

// TestShouldReceiveNowDebounce pins the receive debounce: a missing marker means
// "never received" so a receive is allowed; a marker older than the interval
// allows; a marker inside the interval blocks. One-write-engine-per-path
// serialization makes an over-eager allow a harmless no-op, so the boundary
// errs toward allow.
func TestShouldReceiveNowDebounce(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	now := time.Now()
	interval := 10 * time.Second

	if !shouldReceiveNow(ws, now, interval) {
		t.Fatalf("missing marker should allow receive")
	}

	if err := markReceiveAttempt(ws); err != nil {
		t.Fatalf("markReceiveAttempt error = %v", err)
	}
	if _, err := os.Stat(receiveMarkerPath(ws)); err != nil {
		t.Fatalf("marker not created: %v", err)
	}

	// Marker just written: a receive one second later is debounced.
	if shouldReceiveNow(ws, now.Add(1*time.Second), interval) {
		t.Fatalf("receive inside the debounce interval should be blocked")
	}
	// Past the interval: allowed again.
	if !shouldReceiveNow(ws, now.Add(interval+time.Second), interval) {
		t.Fatalf("receive past the debounce interval should be allowed")
	}
}

// TestEnsureMirrorCoverageDebouncesRemoteAbsent pins the unconnected-workspace
// rate bound: the first mutation on a remote-less workspace confirms the
// absence (git subprocess) and stamps remote-absent.last; mutations inside the
// recheck interval short-circuit before the claim — no marker churn, no git
// call, observable as the absence marker's mtime standing still — and no
// mirror-pending claim survives either call.
func TestEnsureMirrorCoverageDebouncesRemoteAbsent(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	ws := workspace.Info{RootDir: root, Location: workspace.LocationFromStorageDir(filepath.Join(root, ".lit"))}
	ctx := context.Background()

	ensureMirrorCoverage(ctx, ws)
	info, err := os.Stat(remoteAbsentMarkerPath(ws))
	if err != nil {
		t.Fatalf("first remote-less mutation did not stamp the absence marker: %v", err)
	}
	if mirrorPendingSet(ws) {
		t.Fatal("a remote-less mutation left a mirror-pending claim behind")
	}
	// Claim and answering hold share one lifetime: the remote-less path
	// released its claim above, so its beacon hold must be gone with it — a
	// stale answering hold here would falsely cover later markers this
	// command will never clear.
	verdict, err := store.ProbeMirrorBeacon(ws.DatabasePath)
	if err != nil {
		t.Fatalf("probe beacon after released claim: %v", err)
	}
	if verdict != store.BeaconUnheld {
		t.Fatalf("an un-claimed claimant must stop answering the instant its obligation ends; want BeaconUnheld, got %v", verdict)
	}
	stamped := info.ModTime()

	ensureMirrorCoverage(ctx, ws)
	info, err = os.Stat(remoteAbsentMarkerPath(ws))
	if err != nil {
		t.Fatalf("absence marker vanished: %v", err)
	}
	if !info.ModTime().Equal(stamped) {
		t.Fatal("a mutation inside the recheck interval re-ran the absence confirmation; the debounce must short-circuit it")
	}
	if mirrorPendingSet(ws) {
		t.Fatal("a debounced remote-less mutation left a mirror-pending claim behind")
	}
}

// TestIsTruthyEnv pins the kill-switch parsing: only explicit boolean-true values
// enable it; empty, unset, and unrecognized strings are false so background sync
// is never disabled by accident.
func TestIsTruthyEnv(t *testing.T) {
	t.Parallel()
	truthy := []string{"1", "t", "T", "true", "TRUE", "True", " 1 "}
	for _, v := range truthy {
		if !isTruthyEnv(v) {
			t.Fatalf("isTruthyEnv(%q) = false, want true", v)
		}
	}
	falsy := []string{"", "0", "false", "no", "yes", "on", "off", "  ", "garbage"}
	for _, v := range falsy {
		if isTruthyEnv(v) {
			t.Fatalf("isTruthyEnv(%q) = true, want false", v)
		}
	}
}
