package main

import (
	"strings"
	"testing"
)

func TestWriteBundle(t *testing.T) {
	entries := []Entry{
		{Module: Module{Path: "github.com/a/a", Version: "v1.0.0"}, LicenseName: "MIT", Text: "MIT text here\n"},
		{Module: Module{Path: "github.com/b/b", Version: "v2.0.0"}, LicenseName: "Unknown", Text: "some unusual license text\n"},
	}

	var buf strings.Builder
	if err := WriteBundle(&buf, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	out := buf.String()

	// Order follows entries exactly — WriteBundle does no re-sorting of its
	// own, so module a must appear before module b in the output.
	aIdx := strings.Index(out, "github.com/a/a")
	bIdx := strings.Index(out, "github.com/b/b")
	if aIdx == -1 || bIdx == -1 || aIdx > bIdx {
		t.Fatalf("expected github.com/a/a before github.com/b/b in output:\n%s", out)
	}

	for _, want := range []string{
		"github.com/a/a v1.0.0", "License: MIT", "MIT text here",
		"github.com/b/b v2.0.0", "License: Unknown", "some unusual license text",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestWriteBundleDeterministic(t *testing.T) {
	entries := []Entry{
		{Module: Module{Path: "github.com/a/a", Version: "v1.0.0"}, LicenseName: "MIT", Text: "text\n"},
	}
	var first, second strings.Builder
	if err := WriteBundle(&first, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if err := WriteBundle(&second, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("WriteBundle is not deterministic for a fixed input:\n%s\n---\n%s", first.String(), second.String())
	}
}

// TestWriteBundleSourceLine covers the third shipped artifact's half of
// links-licensing-c0ce.15. THIRD_PARTY_LICENSES is the file that legally
// accompanies the binary, and it is flat text with no index — so a recipient
// who wants to know whether a section's coordinate is really where the source
// came from has nothing to consult but the section itself.
//
// The unreplaced case is asserted as an ABSENCE of the whole line, not merely
// as a different value. A "Source: -" placeholder would be the report's
// convention imported into a document that has no columns to keep aligned, and
// it would put a line reading like a claim on 150 sections that make none.
func TestWriteBundleSourceLine(t *testing.T) {
	entries := replacementEntries

	var buf strings.Builder
	if err := WriteBundle(&buf, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	out := buf.String()

	// The Source line must sit directly under the coordinate it qualifies, so
	// the two are pinned together as one block rather than as two independent
	// substring hits that could land in different sections.
	for _, want := range []string{
		"github.com/dolthub/dolt/go v0.40.5\nSource: github.com/promptctl/dolt/go@v0.40.5-later (a go.mod replace directive substitutes this coordinate's source with that fork; see FORKS.md in lit's source repository)\nLicense: Apache-2.0\n",
		// The directory shape must additionally say that nothing published
		// identifies the source, or the line invites a reader to go looking for
		// a coordinate that does not exist.
		"github.com/dolthub/driver v0.2.1\nSource: ./internal/vendor/dolthub-driver (a go.mod replace directive substitutes this coordinate's source with that patched copy inside lit's own repository; no published coordinate identifies it — see FORKS.md in lit's source repository)\nLicense: Apache-2.0\n",
		"github.com/spf13/cobra v1.8.0\nLicense: Apache-2.0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bundle missing block %q\n%s", want, out)
		}
	}

	if got, want := strings.Count(out, "Source: "), 2; got != want {
		t.Errorf("bundle carries %d Source lines, want %d — only replaced sections may claim one", got, want)
	}
}
