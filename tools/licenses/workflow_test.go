package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReleaseGateBlocksPublish pins this ticket's core wiring
// (links-supply-chain-w6m9.5): the release workflow's publish job must depend on
// the license-gate job, and that gate must run the policy check. Without this
// dependency edge a non-free build could be published; a future refactor that
// dropped `license-gate` from `publish.needs` — silently un-gating releases —
// fails here. Parsed structurally (not string-matched) so reformatting the YAML
// doesn't break it, and only the actual invariant does. [LAW:verifiable-goals]
func TestReleaseGateBlocksPublish(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/release-validate.yml")
	if err != nil {
		t.Fatalf("read release-validate.yml: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Needs yaml.Node `yaml:"needs"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}

	gate, ok := wf.Jobs["license-gate"]
	if !ok {
		t.Fatal("release-validate.yml has no license-gate job")
	}
	ranCheck := false
	for _, s := range gate.Steps {
		if strings.Contains(s.Run, "tools/licenses -check") {
			ranCheck = true
		}
	}
	if !ranCheck {
		t.Error("license-gate job does not run `go run ./tools/licenses -check`")
	}

	publish, ok := wf.Jobs["publish"]
	if !ok {
		t.Fatal("release-validate.yml has no publish job")
	}
	if !nodeContains(publish.Needs, "license-gate") {
		t.Errorf("publish.needs does not include license-gate — releases are not gated on the license posture (needs: %v)", publish.Needs)
	}
	// publish must ALSO depend on validate: it consumes validate's uploaded
	// artifact, so dropping validate would let publish race the build and fail
	// with a confusing "artifact not found" at release time.
	if !nodeContains(publish.Needs, "validate") {
		t.Errorf("publish.needs does not include validate — publish would run before the artifact is built (needs: %v)", publish.Needs)
	}
}

// nodeContains reports whether a YAML `needs:` node — which may be a scalar
// ("validate") or a sequence (["validate", "license-gate"]) — contains want.
func nodeContains(n yaml.Node, want string) bool {
	if n.Kind == yaml.ScalarNode {
		return n.Value == want
	}
	for _, c := range n.Content {
		if c.Value == want {
			return true
		}
	}
	return false
}

// TestShippedArtifactsCiteOnlyShippedDocuments ties two representations of one
// fact together: what the generated artifacts POINT AT, and what the release
// archive CARRIES.
//
// The bundle's Source lines and the report's Source-column legend tell a
// recipient to "see FORKS.md" for what a substitution changes and why. That
// sentence is only true if FORKS.md is in the archive they received — and until
// links-licensing-c0ce.15 added it to .goreleaser.yml's archives.files, it was
// not, so the compliance artifacts cited a document the release did not carry.
//
// The cited names are EXTRACTED from rendered artifacts rather than listed here.
// A hardcoded list would be a third copy of the same fact, free to drift from
// both the prose and the archive; deriving it means a future renderer that
// points at a new document fails this test until the document ships.
// [FRAMING:representation] the map names the territory, so the test asks
// whether the territory is there.
func TestShippedArtifactsCiteOnlyShippedDocuments(t *testing.T) {
	var bundle, report strings.Builder
	if err := WriteBundle(&bundle, replacementEntries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if err := WriteReport(&report, replacementEntries); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	// Repository documents are the ALL-CAPS-with-optional-extension names this
	// project uses at its root (FORKS.md, LICENSE-REPORT.md,
	// THIRD_PARTY_LICENSES). Prose words are lowercase, so the pattern picks up
	// citations without picking up sentences.
	docPattern := regexp.MustCompile(`\b[A-Z][A-Z0-9_]*(?:-[A-Z0-9_]+)*(?:\.md)?\b`)
	cited := map[string]bool{}
	for _, text := range []string{bundle.String(), report.String()} {
		for _, m := range docPattern.FindAllString(text, -1) {
			if strings.Contains(m, ".") || strings.Contains(m, "_") {
				cited[m] = true
			}
		}
	}
	if len(cited) == 0 {
		t.Fatal("no repository documents cited in the generated artifacts; the check went unverified")
	}

	shipped := archiveFiles(t)
	for name := range cited {
		if !shipped[name] {
			t.Errorf("the generated artifacts tell a recipient to see %s, but .goreleaser.yml's archives.files does not ship it: %v",
				name, shipped)
		}
	}
}

// archiveFiles reads the set of files .goreleaser.yml puts in every release
// archive. Parsed structurally so reformatting the YAML cannot break the test
// and only the actual file list can. [LAW:verifiable-goals]
func archiveFiles(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatalf("read .goreleaser.yml: %v", err)
	}
	var cfg struct {
		Archives []struct {
			ID    string   `yaml:"id"`
			Files []string `yaml:"files"`
		} `yaml:"archives"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yml: %v", err)
	}
	// Selected by id, not by position. There is one archives entry today, so
	// [0] is correct today — and would silently start validating a different
	// archive's file list the moment a second entry were added ahead of it,
	// passing or failing on data that has nothing to do with what lit ships.
	// [LAW:types-are-the-program] the name identifies the archive; its index
	// is an accident of the file.
	const archiveID = "lit"
	for _, a := range cfg.Archives {
		if a.ID != archiveID {
			continue
		}
		out := map[string]bool{}
		for _, f := range a.Files {
			out[f] = true
		}
		return out
	}
	t.Fatalf(".goreleaser.yml declares no archive with id %q", archiveID)
	return nil
}
