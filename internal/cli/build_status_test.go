package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// TestBuildStatusNoteReleaseBuild pins the release-build phrasing: a binary
// goreleaser stamped (FromSource false) never mentions build age, because a
// release build's currency is tracked by its version number rather than by how
// long ago it was produced.
func TestBuildStatusNoteReleaseBuild(t *testing.T) {
	t.Parallel()
	got := buildStatusNote(version.Info{Version: "v0.4.0", FromSource: false}, time.Now())
	if got != "build: release v0.4.0" {
		t.Fatalf("buildStatusNote() = %q, want %q", got, "build: release v0.4.0")
	}
}

// TestBuildStatusNoteInstalledSourceBuildIsNotARelease is the regression pin for
// links-build-status-1svs. `just install` stamps Version from `git describe`, so
// the binary this repo puts on a PATH has IsDev == false while being built from
// a working tree — and the note used to key on IsDev and call it a release, age
// unmentioned. FromSource is what separates the two, and this is the exact shape
// the field binary has: a stamped Version AND source provenance.
func TestBuildStatusNoteInstalledSourceBuildIsNotARelease(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	info := version.Info{
		Version:    "0.14.0-21-g613d76e",
		IsDev:      false,
		FromSource: true,
		Date:       now.Add(-10 * 24 * time.Hour).Format(time.RFC3339),
	}
	got := buildStatusNote(info, now)
	if strings.Contains(got, "release") {
		t.Fatalf("buildStatusNote() = %q, want a `just install` binary reported as a source build, not a release", got)
	}
	if !strings.Contains(got, "STALE") {
		t.Fatalf("buildStatusNote() = %q, want STALE for a 10-day-old source build", got)
	}
}

// TestBuildStatusNoteDevBuildFresh pins the common case for `just build`: a
// stamped Date well within version.StaleBuildThreshold.
func TestBuildStatusNoteDevBuildFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	info := version.Info{IsDev: true, FromSource: true, Date: now.Add(-3 * time.Hour).Format(time.RFC3339)}
	got := buildStatusNote(info, now)
	if !strings.Contains(got, "dev build, built 3 hours ago") {
		t.Fatalf("buildStatusNote() = %q, want a fresh dev-build line", got)
	}
	if strings.Contains(got, "STALE") {
		t.Fatalf("buildStatusNote() = %q, wrongly flagged a 3h build as stale", got)
	}
}

// TestBuildStatusNoteDevBuildStale pins the staleness escalation: a Date older
// than version.StaleBuildThreshold (7 days) must say so and name the fix. The
// remedy names both from-source entrypoints, since `just build` alone does not
// refresh a binary that `just install` put on the PATH.
func TestBuildStatusNoteDevBuildStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	info := version.Info{IsDev: true, FromSource: true, Date: now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)}
	got := buildStatusNote(info, now)
	if !strings.Contains(got, "dev build, built 10 days ago") {
		t.Fatalf("buildStatusNote() = %q, want the age phrase", got)
	}
	if !strings.Contains(got, "STALE") || !strings.Contains(got, "just build") {
		t.Fatalf("buildStatusNote() = %q, want a STALE flag naming the fix", got)
	}
	if !strings.Contains(got, "just install") {
		t.Fatalf("buildStatusNote() = %q, want the remedy to name `just install` too", got)
	}
}

// TestBuildStatusNoteDevBuildAtExactThreshold pins the boundary: the comparison
// behind the verdict is >=, so an age exactly equal to
// version.StaleBuildThreshold must already read STALE, not "one tick short." A
// future change that silently swaps >= for > would flip this case.
func TestBuildStatusNoteDevBuildAtExactThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	info := version.Info{IsDev: true, FromSource: true, Date: now.Add(-version.StaleBuildThreshold).Format(time.RFC3339)}
	got := buildStatusNote(info, now)
	if !strings.Contains(got, "STALE") {
		t.Fatalf("buildStatusNote() = %q, want STALE at age == StaleBuildThreshold exactly", got)
	}
}

// TestBuildStatusNoteDevBuildNoDate pins the no-fabrication guard: a source
// build with no stamped Date (the pre-ldflags `go build`) states the date is
// unknown rather than rendering a fabricated age.
func TestBuildStatusNoteDevBuildNoDate(t *testing.T) {
	t.Parallel()
	got := buildStatusNote(version.Info{IsDev: true, FromSource: true}, time.Now())
	if got != "build: dev build (build date unknown)" {
		t.Fatalf("buildStatusNote() = %q, want %q", got, "build: dev build (build date unknown)")
	}
}

// TestResolveBuildStatusNoteReflectsProcessVersionInfo pins the boundary
// function against the actual package-level version vars, mirroring the
// stubbing pattern version_test.go uses for runVersion.
func TestResolveBuildStatusNoteReflectsProcessVersionInfo(t *testing.T) {
	// serial: no t.Parallel — rewrites the process-global
	// version.Version/Commit/Date/Origin; parallel readers of it would race.
	origV, origC, origD, origO := version.Version, version.Commit, version.Date, version.Origin
	t.Cleanup(func() {
		version.Version, version.Commit, version.Date, version.Origin = origV, origC, origD, origO
	})
	version.Version = "vSENTINEL"
	version.Commit = "abc1234"
	version.Date = ""
	version.Origin = version.OriginRelease

	got := resolveBuildStatusNote(time.Now())
	if got != "build: release vSENTINEL" {
		t.Fatalf("resolveBuildStatusNote() = %q, want %q", got, "build: release vSENTINEL")
	}
}

// TestBuildStalenessLinesShapeTable is the accept/reject table for the rare
// next/backlog banner, executable. The reject rows are the point: each one is an
// ordinary invocation that must stay silent, because a banner that fires on
// every run is tuned out and takes the loud case with it.
func TestBuildStalenessLinesShapeTable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	cases := []struct {
		name string
		info version.Info
		warn bool
	}{
		{
			name: "source build past the threshold warns",
			info: version.Info{FromSource: true, Date: at(10 * 24 * time.Hour)},
			warn: true,
		},
		{
			name: "source build exactly at the threshold warns",
			info: version.Info{FromSource: true, Date: at(version.StaleBuildThreshold)},
			warn: true,
		},
		{
			name: "installed `just install` build past the threshold warns despite a stamped Version",
			info: version.Info{Version: "0.14.0-21-g613d76e", FromSource: true, Date: at(21 * 24 * time.Hour)},
			warn: true,
		},
		{
			name: "release build past the threshold stays silent",
			info: version.Info{Version: "0.14.0", FromSource: false, Date: at(90 * 24 * time.Hour)},
			warn: false,
		},
		{
			name: "fresh source build stays silent",
			info: version.Info{FromSource: true, Date: at(3 * time.Hour)},
			warn: false,
		},
		{
			name: "source build one tick inside the threshold stays silent",
			info: version.Info{FromSource: true, Date: at(version.StaleBuildThreshold - time.Minute)},
			warn: false,
		},
		{
			name: "source build with no stamped date stays silent",
			info: version.Info{FromSource: true},
			warn: false,
		},
		{
			name: "source build with an unparseable date stays silent",
			info: version.Info{FromSource: true, Date: "last tuesday"},
			warn: false,
		},
		{
			name: "source build dated in the future stays silent",
			info: version.Info{FromSource: true, Date: now.Add(48 * time.Hour).Format(time.RFC3339)},
			warn: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines := buildStalenessLines(tc.info, now)
			if tc.warn && len(lines) != 1 {
				t.Fatalf("buildStalenessLines() = %q, want exactly one warning line", lines)
			}
			if !tc.warn && len(lines) != 0 {
				t.Fatalf("buildStalenessLines() = %q, want silence", lines)
			}
		})
	}
}

// TestBuildStalenessLineNamesAgeAndRemedy pins what the one line must carry: the
// age that earned the warning, the threshold it crossed, and the command that
// fixes it. A warning that says only "stale" sends its reader looking for the
// number somewhere else.
func TestBuildStalenessLineNamesAgeAndRemedy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	info := version.Info{FromSource: true, Date: now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)}
	lines := buildStalenessLines(info, now)
	if len(lines) != 1 {
		t.Fatalf("buildStalenessLines() = %q, want one line", lines)
	}
	for _, want := range []string{"built 10 days ago", "7 days", "just install"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("buildStalenessLines() = %q, missing %q", lines[0], want)
		}
	}
}
