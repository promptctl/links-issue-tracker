package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/config"
)

// The cadence decision is the load-bearing logic the policy ticket adds: only
// a write command under the on-change policy mirrors. Every other combination
// — read-mode commands, the default on-push policy — stays silent. The truth
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
