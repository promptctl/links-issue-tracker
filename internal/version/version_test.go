package version

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// TestGetReportsRegistryBounds is the contract test that pins the single
// source of truth: Info.Schema.{Min,Max} are exactly what the embedded
// migration registry reports. Adding a migration must change Info.Schema.Max
// automatically — that property is what makes Info trustworthy.
func TestGetReportsRegistryBounds(t *testing.T) {
	info, err := Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	wantMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion() error = %v", err)
	}
	if info.Schema.Min != migrations.Baseline {
		t.Errorf("Schema.Min = %d, want %d (migrations.Baseline)", info.Schema.Min, migrations.Baseline)
	}
	if info.Schema.Max != wantMax {
		t.Errorf("Schema.Max = %d, want %d (migrations.MaxVersion)", info.Schema.Max, wantMax)
	}
	if info.Schema.Min > info.Schema.Max {
		t.Errorf("Schema.Min (%d) > Schema.Max (%d) — empty/inverted range", info.Schema.Min, info.Schema.Max)
	}
}

// TestIsDevPromotesVersionAbsence pins the discriminator: when the link-time
// Version is empty, IsDev is true; when populated, IsDev is false. Consumers
// rely on this field (not on `info.Version == ""`) so that the dev-detection
// rule lives in exactly one place.
func TestIsDevPromotesVersionAbsence(t *testing.T) {
	// Snapshot the link-time variable, restore after.
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = ""
	devInfo, err := Get()
	if err != nil {
		t.Fatalf("Get() (dev) error = %v", err)
	}
	if !devInfo.IsDev {
		t.Error("IsDev = false on empty Version, want true")
	}

	Version = "v9.9.9"
	relInfo, err := Get()
	if err != nil {
		t.Fatalf("Get() (release) error = %v", err)
	}
	if relInfo.IsDev {
		t.Error("IsDev = true on populated Version, want false")
	}
	if relInfo.Version != "v9.9.9" {
		t.Errorf("Version = %q, want %q (link-time field not surfaced)", relInfo.Version, "v9.9.9")
	}
}

// TestInfoFieldsRoundTripFromLinkTimeVariables pins that all three link-time
// strings reach Info verbatim — no transformation, no parsing. The string the
// linker writes is the string consumers see. This is the contract that lets
// goreleaser inject `-ldflags -X ...=<value>` without any further processing.
func TestInfoFieldsRoundTripFromLinkTimeVariables(t *testing.T) {
	origV, origC, origD := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origV, origC, origD })

	Version = "v1.2.3"
	Commit = "abcdef0"
	Date = "2026-05-24T15:21:00Z"

	info, err := Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if info.Version != Version {
		t.Errorf("Version round-trip: got %q, set %q", info.Version, Version)
	}
	if info.Commit != Commit {
		t.Errorf("Commit round-trip: got %q, set %q", info.Commit, Commit)
	}
	if info.Date != Date {
		t.Errorf("Date round-trip: got %q, set %q", info.Date, Date)
	}
}
