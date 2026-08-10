package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// TestBuildStatusNoteReleaseBuild pins the release-build phrasing: a stamped
// Version means IsDev is false, and the note never mentions build age (a
// release build's currency is tracked by its version number, not its age).
func TestBuildStatusNoteReleaseBuild(t *testing.T) {
	got := buildStatusNote(version.Info{Version: "v0.4.0", IsDev: false}, time.Now())
	if got != "build: release v0.4.0" {
		t.Fatalf("buildStatusNote() = %q, want %q", got, "build: release v0.4.0")
	}
}

// TestBuildStatusNoteDevBuildFresh pins the common case for `just build`: no
// stamped Version (IsDev true) but a stamped Date, well within
// version.StaleBuildThreshold.
func TestBuildStatusNoteDevBuildFresh(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	info := version.Info{IsDev: true, Date: now.Add(-3 * time.Hour).Format(time.RFC3339)}
	got := buildStatusNote(info, now)
	if !strings.Contains(got, "dev build, built 3 hours ago") {
		t.Fatalf("buildStatusNote() = %q, want a fresh dev-build line", got)
	}
	if strings.Contains(got, "STALE") {
		t.Fatalf("buildStatusNote() = %q, wrongly flagged a 3h build as stale", got)
	}
}

// TestBuildStatusNoteDevBuildStale pins the staleness escalation: a Date older
// than version.StaleBuildThreshold (7 days) must say so and name the fix.
func TestBuildStatusNoteDevBuildStale(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	info := version.Info{IsDev: true, Date: now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)}
	got := buildStatusNote(info, now)
	if !strings.Contains(got, "dev build, built 10 days ago") {
		t.Fatalf("buildStatusNote() = %q, want the age phrase", got)
	}
	if !strings.Contains(got, "STALE") || !strings.Contains(got, "just build") {
		t.Fatalf("buildStatusNote() = %q, want a STALE flag naming the fix", got)
	}
}

// TestBuildStatusNoteDevBuildNoDate pins the no-fabrication guard: a dev build
// with no stamped Date (the pre-ldflags `go build`) states the date is
// unknown rather than rendering a fabricated age.
func TestBuildStatusNoteDevBuildNoDate(t *testing.T) {
	got := buildStatusNote(version.Info{IsDev: true}, time.Now())
	if got != "build: dev build (build date unknown)" {
		t.Fatalf("buildStatusNote() = %q, want %q", got, "build: dev build (build date unknown)")
	}
}

// TestResolveBuildStatusNoteReflectsProcessVersionInfo pins the boundary
// function against the actual package-level version vars, mirroring the
// stubbing pattern version_test.go uses for runVersion.
func TestResolveBuildStatusNoteReflectsProcessVersionInfo(t *testing.T) {
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = "vSENTINEL"
	version.Commit = "abc1234"
	version.Date = ""

	got := resolveBuildStatusNote(time.Now())
	if got != "build: release vSENTINEL" {
		t.Fatalf("resolveBuildStatusNote() = %q, want %q", got, "build: release vSENTINEL")
	}
}
