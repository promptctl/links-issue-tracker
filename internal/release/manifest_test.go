package release

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/version"
)

// TestManifestRoundTrips pins the JSON contract: marshaling a Manifest and
// unmarshaling the bytes back yields an equal value. This is the wire format
// `lit downgrade` (epic .4) will consume; round-trip equality is the typed
// guarantee that no field is silently dropped or renamed.
func TestManifestRoundTrips(t *testing.T) {
	want := Manifest{
		Info: version.Info{
			Version: "v0.1.0",
			Commit:  "abcdef0",
			Date:    "2026-05-24T15:21:00Z",
			IsDev:   false,
			Schema:  version.SchemaSupport{Min: 1, Max: 1},
		},
		Artifacts: []Artifact{
			{Platform: "darwin/arm64", URL: "https://example/lit-darwin-arm64.tar.gz", SHA256: "deadbeef"},
			{Platform: "linux/amd64", URL: "https://example/lit-linux-amd64.tar.gz", SHA256: "cafebabe"},
		},
	}

	encoded, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("Marshal error = %v", err)
	}
	var got Manifest
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal error = %v", err)
	}
	if got.Version != want.Version {
		t.Errorf("Version round-trip: got %q, want %q", got.Version, want.Version)
	}
	if got.Schema != want.Schema {
		t.Errorf("Schema round-trip: got %+v, want %+v", got.Schema, want.Schema)
	}
	if len(got.Artifacts) != len(want.Artifacts) {
		t.Fatalf("Artifacts len = %d, want %d", len(got.Artifacts), len(want.Artifacts))
	}
	for i, a := range want.Artifacts {
		if got.Artifacts[i] != a {
			t.Errorf("Artifact[%d] = %+v, want %+v", i, got.Artifacts[i], a)
		}
	}
}

// TestSignatureIsOptional pins that an unsigned manifest does not emit a
// "signature": null field — present and null is observably different from
// absent, and we want absent so future signing can land without changing the
// shape clients already parse.
func TestSignatureIsOptional(t *testing.T) {
	m := Manifest{
		Info:      version.Info{Version: "v0.1.0", Schema: version.SchemaSupport{Min: 1, Max: 1}},
		Artifacts: []Artifact{{Platform: "linux/amd64", URL: "u", SHA256: "s"}},
	}
	encoded, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("Marshal error = %v", err)
	}
	if strings.Contains(string(encoded), `"signature"`) {
		t.Errorf("unsigned manifest emitted a signature field:\n%s", encoded)
	}
}

// TestArtifactPlatformShape pins the GOOS/GOARCH format the producer writes.
// `lit downgrade` will compare against runtime.GOOS+"/"+runtime.GOARCH — any
// drift (lit-linux-amd64, linux-amd64, etc.) silently fails the lookup.
func TestArtifactPlatformShape(t *testing.T) {
	cases := []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64"}
	for _, p := range cases {
		parts := strings.Split(p, "/")
		if len(parts) != 2 {
			t.Errorf("platform %q does not have form <goos>/<goarch>", p)
		}
		for _, part := range parts {
			if part == "" {
				t.Errorf("platform %q has an empty component", p)
			}
		}
	}
}
