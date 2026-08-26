package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// TestVersionHumanSurfacesAllInfoFields pins the currently-rendered Info
// fields in the human surface: Version, Commit, Date, and Schema.{Min,Max}.
// Scope is the present surface — this test does not provide automatic
// coverage for new Info fields. Adding a field to version.Info that should
// also appear in the human form requires updating this test explicitly.
// The pin is here so a refactor that drops one of the currently-rendered
// fields fails the build.
func TestVersionHumanSurfacesAllInfoFields(t *testing.T) {
	// serial: no t.Parallel — rewrites the process-global version.Version/Commit/Date;
	// parallel readers of it would race.
	// Stamp link-time fields so the human form has something concrete to render.
	// Use values that can NOT appear anywhere except in their respective fields:
	// version is a sentinel that cannot collide with schema digits ("0.0.0" not
	// "v1.2.3"; the latter contains "1" which is the current Schema.Max).
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = "vSENTINEL-9.9.9"
	version.Commit = "abcdef0"
	version.Date = "2026-05-24T15:21:00Z"

	var stdout bytes.Buffer
	if err := runVersion(&stdout, nil); err != nil {
		t.Fatalf("runVersion (human) error = %v", err)
	}
	out := stdout.String()

	for _, want := range []string{"vSENTINEL-9.9.9", "abcdef0", "2026-05-24T15:21:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q:\n%s", want, out)
		}
	}

	// Schema range — assert the exact rendered substring, not loose substring
	// match. The format string is "schema versions supported: %d–%d\n", so the
	// expected line is fully determined by the registry bounds.
	min := migrations.Baseline
	max, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion error = %v", err)
	}
	wantLine := fmt.Sprintf("schema versions supported: %d–%d", min, max)
	if !strings.Contains(out, wantLine) {
		t.Errorf("human output missing schema-range line %q:\n%s", wantLine, out)
	}
}

// TestVersionHumanLabelsDevBuild pins the "no link-time version stamped"
// surface: the human form shows "dev" in place of an empty Version, and
// "unknown" in place of empty Commit/Date. Consumers of the human form rely
// on these labels (they're more legible than literal empty strings).
func TestVersionHumanLabelsDevBuild(t *testing.T) {
	// serial: no t.Parallel — rewrites the process-global version.Version/Commit/Date;
	// parallel readers of it would race.
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = ""
	version.Commit = ""
	version.Date = ""

	var stdout bytes.Buffer
	if err := runVersion(&stdout, nil); err != nil {
		t.Fatalf("runVersion (dev) error = %v", err)
	}
	out := stdout.String()
	for _, want := range []string{"dev", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("dev-build human output missing %q:\n%s", want, out)
		}
	}
}

// TestVersionRejectsPositionalArgs pins the command shape: `lit version` takes
// no positional args; any positional is a usage error. Prevents silent misuse
// like `lit version v0.1.0` (which a user might think means "show v0.1.0's
// release manifest" — that operation belongs to a different command).
func TestVersionRejectsPositionalArgs(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	err := runVersion(&stdout, []string{"v0.1.0"})
	if err == nil {
		t.Fatal("runVersion with positional arg returned nil, want usage error")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("err = %v, want a usage error message", err)
	}
}

// TestVersionReportsBuildAgeWhenDateStamped pins the ticket-level fix: a
// binary with a stamped Date (what `just build` now produces via
// scripts/version-ldflags.sh, even without a stamped Version) reports how
// long ago it was built instead of leaving the reader to guess from a bare
// timestamp.
func TestVersionReportsBuildAgeWhenDateStamped(t *testing.T) {
	// serial: no t.Parallel — rewrites the process-global version.Version/Commit/Date;
	// parallel readers of it would race.
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = ""
	version.Commit = "abcdef0"
	version.Date = time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)

	var stdout bytes.Buffer
	if err := runVersion(&stdout, nil); err != nil {
		t.Fatalf("runVersion error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "built 3 hours ago") {
		t.Errorf("output missing build-age line for a 3h-old stamp:\n%s", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("output warned about staleness for a 3h-old build:\n%s", out)
	}
}

// TestVersionFlagsStaleBuild pins the staleness half of the ticket: a Date
// older than version.StaleBuildThreshold surfaces a rebuild warning, so an
// agent reading `lit version` learns its binary is worth rebuilding without
// having to compute the age itself.
func TestVersionFlagsStaleBuild(t *testing.T) {
	// serial: no t.Parallel — rewrites the process-global version.Version/Commit/Date;
	// parallel readers of it would race.
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = ""
	version.Commit = "abcdef0"
	version.Date = time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)

	var stdout bytes.Buffer
	if err := runVersion(&stdout, nil); err != nil {
		t.Fatalf("runVersion error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "built 10 days ago") {
		t.Errorf("output missing build-age line for a 10d-old stamp:\n%s", out)
	}
	if !strings.Contains(out, "WARNING") {
		t.Errorf("output did not flag a 10d-old build as stale:\n%s", out)
	}
}

// TestVersionOmitsBuildAgeWithoutDate pins the no-fabrication guard at the CLI
// surface: an unstamped Date (the pre-ticket `just build`, or any build whose
// linker never set it) prints no "built X ago" line and no staleness warning
// — the existing "built unknown" phrasing (TestVersionHumanLabelsDevBuild)
// stays the whole story.
func TestVersionOmitsBuildAgeWithoutDate(t *testing.T) {
	// serial: no t.Parallel — rewrites the process-global version.Version/Commit/Date;
	// parallel readers of it would race.
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = ""
	version.Commit = ""
	version.Date = ""

	var stdout bytes.Buffer
	if err := runVersion(&stdout, nil); err != nil {
		t.Fatalf("runVersion error = %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, "ago") || strings.Contains(out, "WARNING") {
		t.Errorf("output rendered an age/warning with no Date stamped:\n%s", out)
	}
}
