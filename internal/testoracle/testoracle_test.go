package testoracle

import (
	"fmt"
	"os"
	"os/exec"
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
	// The .exe is in the name itself, so the same file is an executable on
	// PATH both where LookPath reads the mode's exec bits and where it reads
	// PATHEXT; LookPath runs nothing, so what the file holds is beside the point.
	present := filepath.Join(dir, "present-oracle.exe")
	if err := os.WriteFile(present, nil, 0o755); err != nil {
		t.Fatalf("write fake oracle: %v", err)
	}
	t.Setenv("PATH", dir)
	// The lookup error's text is the platform's: Windows names %PATH%.
	notFound := (&exec.Error{Name: "absent-oracle", Err: exec.ErrNotFound}).Error()

	cases := []struct {
		name, oracle, githubActions, wantPath, wantVerdict string
	}{
		{"present in CI", "present-oracle.exe", "true", present, ""},
		{"present locally", "present-oracle.exe", "", present, ""},
		{"missing in CI", "absent-oracle", "true", "", "fail: absent-oracle not on PATH, but CI installs every test oracle (.github/actions/install-absent-oracle): " + notFound},
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
