package testoracle

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// verdictRecorder stands in for the test Path is handed, so each outcome is
// observable instead of ending this test.
type verdictRecorder struct {
	testing.TB
	verdict string
}

func (r *verdictRecorder) Helper() {}

func (r *verdictRecorder) Fatalf(format string, args ...any) {
	r.verdict = "fail: " + fmt.Sprintf(format, args...)
}

func (r *verdictRecorder) Skipf(format string, args ...any) {
	r.verdict = "skip: " + fmt.Sprintf(format, args...)
}

// TestPathOutcomes pins all three outcomes: an oracle on PATH is returned, a
// missing one skips outside CI, and a missing one fails under GitHub Actions.
// Not parallel: it rewrites PATH and GITHUB_ACTIONS for the process.
func TestPathOutcomes(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present-oracle")
	if err := os.WriteFile(present, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake oracle: %v", err)
	}
	t.Setenv("PATH", dir)

	cases := []struct {
		name, oracle, githubActions, wantPath, wantVerdict string
	}{
		{"present in CI", "present-oracle", "true", present, ""},
		{"present locally", "present-oracle", "", present, ""},
		{"missing in CI", "absent-oracle", "true", "", "fail: absent-oracle not on PATH, but CI installs every test oracle (.github/actions/install-absent-oracle): exec: \"absent-oracle\": executable file not found in $PATH"},
		{"missing locally", "absent-oracle", "", "", "skip: absent-oracle not on PATH; skipping this oracle check"},
		{"missing, GITHUB_ACTIONS not true", "absent-oracle", "false", "", "skip: absent-oracle not on PATH; skipping this oracle check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", tc.githubActions)
			rec := &verdictRecorder{TB: t}
			got := Path(rec, tc.oracle)
			if got != tc.wantPath || rec.verdict != tc.wantVerdict {
				t.Fatalf("Path(%q) = %q, verdict %q; want %q, verdict %q", tc.oracle, got, rec.verdict, tc.wantPath, tc.wantVerdict)
			}
		})
	}
}
