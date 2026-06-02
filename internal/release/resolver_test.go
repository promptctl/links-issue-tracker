package release

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// fixtureManifest is the manifest the test server returns. Mirrors the shape
// mkmanifest produces — v-stripped Version, "<goos>/<goarch>" platforms,
// 64-hex-char SHA256s.
func fixtureManifest() Manifest {
	return Manifest{
		Info: version.Info{
			Version: "0.4.1",
			Commit:  "abc1234",
			Date:    "2026-05-01T00:00:00Z",
			IsDev:   false,
			Schema:  version.SchemaSupport{Min: 1, Max: 3},
		},
		Artifacts: []Artifact{
			{Platform: "darwin/arm64", URL: "https://example/lit_0.4.1_darwin_arm64.tar.gz",
				SHA256: strings.Repeat("a", 64)},
			{Platform: "linux/amd64", URL: "https://example/lit_0.4.1_linux_amd64.tar.gz",
				SHA256: strings.Repeat("b", 64)},
		},
	}
}

func newManifestServer(t *testing.T, tag string, m Manifest, status int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/"+tag+"/release-manifest.json", func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			http.Error(w, "manifest missing", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&m)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPResolverResolvesPlatformArtifact(t *testing.T) {
	srv := newManifestServer(t, "v0.4.1", fixtureManifest(), 0)
	r := &HTTPResolver{BaseURL: srv.URL}
	tgt, err := r.Resolve(context.Background(), "v0.4.1", "darwin/arm64")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tgt.Manifest.Version != "0.4.1" {
		t.Errorf("manifest version: got %q want 0.4.1", tgt.Manifest.Version)
	}
	if tgt.Artifact.Platform != "darwin/arm64" {
		t.Errorf("selected platform: got %q want darwin/arm64", tgt.Artifact.Platform)
	}
	if tgt.Manifest.Schema.Max != 3 {
		t.Errorf("schema max: got %d want 3", tgt.Manifest.Schema.Max)
	}
}

func TestHTTPResolverUnknownPlatformErrors(t *testing.T) {
	srv := newManifestServer(t, "v0.4.1", fixtureManifest(), 0)
	r := &HTTPResolver{BaseURL: srv.URL}
	_, err := r.Resolve(context.Background(), "v0.4.1", "freebsd/riscv64")
	if err == nil {
		t.Fatal("expected error for unsupported platform, got nil")
	}
	if !strings.Contains(err.Error(), "freebsd/riscv64") {
		t.Errorf("error should name requested platform: %v", err)
	}
	if !strings.Contains(err.Error(), "darwin/arm64") {
		t.Errorf("error should list available platforms: %v", err)
	}
}

func TestHTTPResolverRejectsUnprefixedTag(t *testing.T) {
	r := &HTTPResolver{BaseURL: "http://example.invalid"}
	_, err := r.Resolve(context.Background(), "0.4.1", "darwin/arm64")
	if err == nil || !strings.Contains(err.Error(), "v-prefixed") {
		t.Fatalf("expected v-prefix rejection, got %v", err)
	}
}

func TestHTTPResolverRejectsUnknownFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// version.Info-shaped payload with a stray field at the top level.
		_, _ = w.Write([]byte(`{"version":"0.4.1","commit":"x","date":"y","is_dev":false,"schema_support":{"min":1,"max":1},"artifacts":[],"surprise":"hi"}`))
	}))
	t.Cleanup(srv.Close)
	r := &HTTPResolver{BaseURL: srv.URL}
	_, err := r.Resolve(context.Background(), "v0.4.1", "darwin/arm64")
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown-field rejection, got %v", err)
	}
}

func TestHTTPResolverRejectsTrailingData(t *testing.T) {
	m := fixtureManifest()
	// Two adjacent top-level JSON documents — the prior `dec.More()` check
	// returned false for this case (More() only sees nested elements), so a
	// second `Decode` is what catches it. This test fails if the resolver
	// regresses to the More()-based check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(&m)
		_, _ = w.Write([]byte(`{"second":"doc"}`))
	}))
	t.Cleanup(srv.Close)
	r := &HTTPResolver{BaseURL: srv.URL}
	_, err := r.Resolve(context.Background(), "v0.4.1", "darwin/arm64")
	if err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("expected trailing-data rejection, got %v", err)
	}
}

func TestHTTPResolverRejectsURLMetacharsInTag(t *testing.T) {
	r := &HTTPResolver{BaseURL: "http://example.invalid"}
	for _, bad := range []string{
		"v0.4.1?inject=1",
		"v0.4.1#frag",
		"v0.4.1%2F..",
		"v0.4.1 ",
		"v",     // empty after prefix
		"v..",   // path-traversal-shaped
		"v/0.1", // separator
	} {
		if _, err := r.Resolve(context.Background(), bad, "darwin/arm64"); err == nil {
			t.Errorf("Resolve(%q) accepted; want refusal", bad)
		}
	}
}

func TestHTTPResolver404IsSurfaced(t *testing.T) {
	srv := newManifestServer(t, "v0.4.2", fixtureManifest(), http.StatusNotFound)
	r := &HTTPResolver{BaseURL: srv.URL}
	_, err := r.Resolve(context.Background(), "v0.4.1", "darwin/arm64") // tag mismatch → 404
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "HTTP") {
		t.Errorf("error should mention the HTTP failure: %v", err)
	}
}
