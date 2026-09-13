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

// TestHTTPResolverToleratesFieldsFromANewerProducer pins the compatibility
// direction this format actually has. `lit upgrade` runs the INSTALLED binary
// to discover a newer release, so the consumer is always older than the
// producer and a field added after this binary shipped must decode, not fail.
// The resolver used to set DisallowUnknownFields, which asserted the opposite
// and would have broken the upgrade path for every binary in the field at the
// first additive release — with no in-band way out, since the remedy for a
// broken `lit upgrade` is `lit upgrade`.
//
// The payload carries a field no version of this struct has ever had, and the
// assertions read the fields that decide behavior, so the test fails both if
// the strict decoder returns and if tolerance were bought by dropping the
// payload on the floor.
func TestHTTPResolverToleratesFieldsFromANewerProducer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.4.1","commit":"x","date":"y","is_dev":false,"schema_support":{"min":1,"max":1},"artifacts":[{"platform":"darwin/arm64","url":"https://example.invalid/lit","sha256":"abc"}],"a_field_from_the_future":"hi"}`))
	}))
	t.Cleanup(srv.Close)
	r := &HTTPResolver{BaseURL: srv.URL}
	target, err := r.Resolve(context.Background(), "v0.4.1", "darwin/arm64")
	if err != nil {
		t.Fatalf("a manifest from a newer producer must still resolve, got %v", err)
	}
	if target.Artifact.URL != "https://example.invalid/lit" || target.Artifact.SHA256 != "abc" {
		t.Errorf("known fields must still bind past the unknown one, got %+v", target.Artifact)
	}
}

// TestManifestNeverSerializesFromSource is the other half of that
// compatibility story, and the half that protects binaries ALREADY installed.
// Those decode with DisallowUnknownFields compiled in; nothing this repo does
// now can change them, so the only lever left is never emitting a field they
// do not know. version.Info.FromSource is tagged `json:"-"` for that reason,
// and this asserts the serialized shape rather than the tag, so re-tagging it
// or promoting it into Manifest both fail here.
func TestManifestNeverSerializesFromSource(t *testing.T) {
	m := fixtureManifest()
	m.FromSource = true // the value that would leak if the field were ever tagged
	out, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if strings.Contains(string(out), "from_source") {
		t.Errorf("manifest serialized from_source; every already-installed lit decodes with DisallowUnknownFields and would fail to upgrade: %s", out)
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

// The release feed is a third-party API that grows fields freely; LatestTag
// reads tag_name and ignores the rest.
func TestHTTPResolverLatestTagResolvesFromFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"url":"https://api.example/1","id":1,"tag_name":"v0.5.0","name":"v0.5.0","draft":false,"prerelease":false}`))
	}))
	t.Cleanup(srv.Close)
	r := &HTTPResolver{LatestURL: srv.URL}
	tag, err := r.LatestTag(context.Background())
	if err != nil {
		t.Fatalf("LatestTag: %v", err)
	}
	if tag != "v0.5.0" {
		t.Errorf("LatestTag = %q; want v0.5.0", tag)
	}
}

// A 200 with no tag_name is a feed-shape change, not "no releases" — refused
// loudly rather than returned as an empty tag that fails far from its cause.
func TestHTTPResolverLatestTagRefusesMissingTagName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"name":"mystery"}`))
	}))
	t.Cleanup(srv.Close)
	r := &HTTPResolver{LatestURL: srv.URL}
	if _, err := r.LatestTag(context.Background()); err == nil || !strings.Contains(err.Error(), "tag_name") {
		t.Fatalf("expected missing-tag_name refusal, got %v", err)
	}
}

// A feed-served tag passes through the same accept shape as a user's --to;
// a malformed one is refused, never interpolated into a manifest URL.
func TestHTTPResolverLatestTagRefusesMalformedTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"0.5.0/../evil"}`))
	}))
	t.Cleanup(srv.Close)
	r := &HTTPResolver{LatestURL: srv.URL}
	if _, err := r.LatestTag(context.Background()); err == nil {
		t.Fatal("expected malformed feed tag to be refused")
	}
}

func TestHTTPResolverLatestTagSurfacesHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	r := &HTTPResolver{LatestURL: srv.URL}
	_, err := r.LatestTag(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected HTTP failure to surface with its status, got %v", err)
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
