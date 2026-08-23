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
		"github.com/dolthub/dolt/go v0.40.5\nSource: github.com/promptctl/dolt/go@v0.40.5-later" + sourceSuffixFork + "\nLicense: Apache-2.0\n",
		// The directory shape must additionally say that nothing published
		// identifies the source, or the line invites a reader to go looking for
		// a coordinate that does not exist.
		"github.com/dolthub/driver v0.2.1\nSource: ./internal/vendor/dolthub-driver" + sourceSuffixDirectory + "\nLicense: Apache-2.0\n",
		// A version pin is disclosed like any other substitution, and its
		// parenthetical is the one that must NOT say "fork".
		"github.com/spf13/viper v1.18.0\nSource: github.com/spf13/viper@v1.19.0" + sourceSuffixVersion + "\nLicense: Apache-2.0\n",
		"github.com/spf13/cobra v1.8.0\nLicense: Apache-2.0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bundle missing block %q\n%s", want, out)
		}
	}

	if got, want := strings.Count(out, "Source: "), 3; got != want {
		t.Errorf("bundle carries %d Source lines, want %d — only replaced sections may claim one", got, want)
	}
}

// TestBundleSourceSuffixesClaimOnlyWhatTheyCanBack pins the two corrections
// review round 2 forced on this wording.
//
// The subject must not be a deictic. "this coordinate" printed immediately to
// the RIGHT of the substitute reads as saying the fork's source was replaced by
// itself — the reverse of the SBOM note's direction, in the file that legally
// accompanies the binary.
//
// And the directory line must claim no containment: modfile.IsDirectoryPath
// accepts `../sibling` and absolute paths, so "inside lit's own repository" is
// a sentence the format cannot back for a sibling checkout or /opt/src.
func TestBundleSourceSuffixesClaimOnlyWhatTheyCanBack(t *testing.T) {
	for name, suffix := range map[string]string{
		"fork":      sourceSuffixFork,
		"version":   sourceSuffixVersion,
		"directory": sourceSuffixDirectory,
	} {
		if strings.Contains(suffix, "this coordinate") {
			t.Errorf("%s suffix says \"this coordinate\" beside the substitute it names, which reads as the reverse of the substitution: %q", name, suffix)
		}
		if !strings.Contains(suffix, "the module named above") {
			t.Errorf("%s suffix does not name its subject unambiguously: %q", name, suffix)
		}
	}
	for _, claim := range []string{"inside lit's own repository", "repository-relative"} {
		if strings.Contains(sourceSuffixDirectory, claim) {
			t.Errorf("directory suffix claims %q, but a replace target may be ../sibling or absolute: %q", claim, sourceSuffixDirectory)
		}
	}
	// A version pin is not a fork, and the line a recipient reads must not call
	// it one.
	if strings.Contains(sourceSuffixVersion, "fork; ") || !strings.Contains(sourceSuffixVersion, "no fork is involved") {
		t.Errorf("version-pin suffix does not disclaim a fork: %q", sourceSuffixVersion)
	}
}

// TestBundleSourceLineRefusesAnUnknownKind is the third renderer's
// exhaustiveness arm. String() and componentPedigree each have their own; this
// one was the copy nothing exercised, so `default: return ""` written here
// would have survived the whole suite while THIRD_PARTY_LICENSES — the file
// that legally accompanies the binary — silently dropped the disclosure.
func TestBundleSourceLineRefusesAnUnknownKind(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("sourceLine returned normally for an unhandled ReplacementKind; the bundle would omit the Source line")
		}
	}()
	_ = sourceLine(Replacement{Kind: ReplacementKind(99), Path: "github.com/example/whatever"})
}
