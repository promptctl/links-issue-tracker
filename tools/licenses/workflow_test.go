package main

import (
	"os"
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
