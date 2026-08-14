package cli

import (
	"os"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The cadence decision is the load-bearing logic the policy ticket adds: only
// a write command under the on-change policy mirrors. Every other combination
// — read-mode commands, the opt-in on-push policy — stays silent. The truth
// table pins all four cells so neither axis can drift into a spurious push.
func TestShouldSyncAfterMutation(t *testing.T) {
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
// allows; a marker inside the interval blocks. The single-flight engine lock
// makes an over-eager allow a harmless no-op, so the boundary errs toward allow.
func TestShouldReceiveNowDebounce(t *testing.T) {
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

// TestShouldSpawnMirrorNowDebounce pins the mirror-spawn instance of the shared
// debounce primitives (shouldRunNow/markRunAttempt): a missing marker allows a
// spawn, a fresh marker blocks inside the interval, an aged one allows again —
// and the two debounce instances are independent facts, so marking a mirror
// spawn must not consume the receive debounce or vice versa.
func TestShouldSpawnMirrorNowDebounce(t *testing.T) {
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	now := time.Now()
	interval := time.Second

	if !shouldSpawnMirrorNow(ws, now, interval) {
		t.Fatalf("missing marker should allow a mirror spawn")
	}
	if err := markMirrorSpawnAttempt(ws); err != nil {
		t.Fatalf("markMirrorSpawnAttempt error = %v", err)
	}
	if _, err := os.Stat(mirrorSpawnMarkerPath(ws)); err != nil {
		t.Fatalf("mirror-spawn marker not created: %v", err)
	}
	if shouldSpawnMirrorNow(ws, now.Add(100*time.Millisecond), interval) {
		t.Fatalf("mirror spawn inside the debounce interval should be blocked")
	}
	if !shouldSpawnMirrorNow(ws, now.Add(interval+time.Second), interval) {
		t.Fatalf("mirror spawn past the debounce interval should be allowed")
	}
	// Independence: the mirror marker must not have consumed the receive
	// debounce — each instance owns its own marker file.
	if !shouldReceiveNow(ws, now, interval) {
		t.Fatalf("marking a mirror spawn must not debounce the receive")
	}
}

// TestIsTruthyEnv pins the kill-switch parsing: only explicit boolean-true values
// enable it; empty, unset, and unrecognized strings are false so background sync
// is never disabled by accident.
func TestIsTruthyEnv(t *testing.T) {
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
