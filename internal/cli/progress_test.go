package cli

import (
	"bytes"
	"testing"
)

// captureProgress redirects progress lines into a buffer for the test's
// lifetime, restoring stderr on cleanup. These tests must not run in parallel
// (they mutate a package var, like the adopt-timeout tests).
func captureProgress(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := progressOut
	buf := &bytes.Buffer{}
	progressOut = buf
	t.Cleanup(func() { progressOut = prev })
	return buf
}

func TestProgressfPrefixesOperationAndTerminatesLine(t *testing.T) {
	buf := captureProgress(t)
	progressf("sync pull", "pulling lit data from %s/%s", "origin", "master")
	want := "lit: sync pull: pulling lit data from origin/master\n"
	if got := buf.String(); got != want {
		t.Fatalf("progressf() wrote %q, want %q", got, want)
	}
}

// TestRemoteSituationLine pins the situation narration to the sealed adopt
// states: every benign non-adopt outcome names what was found, while failed
// stays empty — the failure already has its one loud channel and must not be
// double-reported. [LAW:single-enforcer]
func TestRemoteSituationLine(t *testing.T) {
	cases := []struct {
		state     initSyncState
		wantEmpty bool
	}{
		{state: initSyncHasLocalTickets},
		{state: initSyncNotConfigured},
		{state: initSyncRemoteEmpty},
		{state: initSyncNoRemoteData},
		{state: initSyncAdopted, wantEmpty: true},
		{state: initSyncFailed, wantEmpty: true},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			got := remoteSituationLine(tc.state)
			if tc.wantEmpty && got != "" {
				t.Fatalf("remoteSituationLine(%q) = %q, want empty (state has its own reporting channel)", tc.state, got)
			}
			if !tc.wantEmpty && got == "" {
				t.Fatalf("remoteSituationLine(%q) = empty, want a situation description", tc.state)
			}
		})
	}
}
