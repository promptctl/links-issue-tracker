package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultResolverTimeout bounds a single resolver fetch (the release feed or
// a manifest). Both are small JSON documents (well under 1 MiB); 60s is
// generous on a slow link without allowing an indefinite hang. The CLI calls with context.Background() at
// time of writing, so without this bound a stalled server would wedge the
// command forever.
//
// [LAW:types-are-the-program] The accept shape of "an HTTP manifest fetch"
// includes a deadline. http.DefaultClient has none; the boundary needs
// its own bounded default.
const defaultResolverTimeout = 60 * time.Second

// Resolver translates a release tag + platform into the Target the installer
// will consume.
//
// [LAW:single-enforcer] Resolver owns "find the manifest, parse it, pick the
// platform artifact." The downgrade CLI never composes manifest URLs itself
// or fishes a specific artifact out of a Manifest.
type Resolver interface {
	Resolve(ctx context.Context, tag Tag, platform string) (*Target, error)
}

// LatestResolver names the latest published release tag, for a caller that
// was given no tag to target. The result feeds Resolver.Resolve unchanged —
// latest-lookup answers only "which tag," never "which artifact."
//
// [LAW:one-source-of-truth] "Latest" is defined by the release feed
// (DefaultLatestReleaseURL), the same authority scripts/install.sh's
// --latest-release consults — not by any local heuristic over tags.
type LatestResolver interface {
	LatestTag(ctx context.Context) (Tag, error)
}

// DefaultBaseURL is the GitHub Release download root for published lit
// artifacts. mkmanifest writes per-platform URLs under <base>/<tag>/<filename>
// and publishes release-manifest.json under <base>/<tag>/release-manifest.json,
// so the consumer fetches the manifest from the same base.
//
// [LAW:one-source-of-truth] Same value as scripts/install.sh's
// REPO_DOWNLOAD_BASE. If the repo moves, both move together.
const DefaultBaseURL = "https://github.com/promptctl/links-issue-tracker/releases/download"

// DefaultLatestReleaseURL is the release-feed endpoint that names the latest
// published release. GitHub's `releases/latest` API returns the newest
// non-prerelease, non-draft release; its `tag_name` field is the tag lit
// installs when the user names none.
//
// [LAW:one-source-of-truth] Same value as scripts/install.sh's
// REPO_LATEST_API. If the repo moves, both move together.
const DefaultLatestReleaseURL = "https://api.github.com/repos/promptctl/links-issue-tracker/releases/latest"

// HTTPResolver is the default Resolver and LatestResolver. It HTTP-GETs the
// manifest at <BaseURL>/<tag>/release-manifest.json and decodes it into a
// Manifest; LatestTag consults the release feed at LatestURL.
type HTTPResolver struct {
	BaseURL   string       // empty defaults to DefaultBaseURL
	LatestURL string       // empty defaults to DefaultLatestReleaseURL
	Client    *http.Client // nil defaults to a client bounded by defaultResolverTimeout
}

// httpClient returns the configured client or the bounded default.
func (r *HTTPResolver) httpClient() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	// Bounded default — http.DefaultClient is shared and has no Timeout.
	return &http.Client{Timeout: defaultResolverTimeout}
}

// LatestTag fetches the release feed and parses the latest release's tag
// through the same ParseTag as an explicitly named one.
func (r *HTTPResolver) LatestTag(ctx context.Context) (Tag, error) {
	url := r.LatestURL
	if url == "" {
		url = DefaultLatestReleaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Tag{}, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return Tag{}, fmt.Errorf("release: fetch latest release from %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return Tag{}, fmt.Errorf("release: fetch latest release from %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// The feed is a third-party API that grows fields freely, so unlike the
	// manifest decode this one must tolerate unknown fields; tag_name is the
	// only field lit consumes (the same field install.sh's jq reads).
	var feed struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&feed); err != nil {
		return Tag{}, fmt.Errorf("release: decode latest release from %s: %w", url, err)
	}
	// [LAW:no-silent-failure] A 200 with no tag_name is a feed-shape change,
	// not "no releases" — refuse loudly rather than hand back an empty tag
	// that would fail later, far from its cause.
	if feed.TagName == "" {
		return Tag{}, fmt.Errorf("release: latest release feed at %s returned no tag_name", url)
	}
	tag, err := ParseTag(feed.TagName)
	if err != nil {
		return Tag{}, fmt.Errorf("release: latest release feed at %s: %w", url, err)
	}
	return tag, nil
}

// Resolve fetches and parses the manifest, then selects the platform artifact.
func (r *HTTPResolver) Resolve(ctx context.Context, tag Tag, platform string) (*Target, error) {
	base := r.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	url := fmt.Sprintf("%s/%s/release-manifest.json", strings.TrimRight(base, "/"), tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("release: fetch %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// [LAW:types-are-the-program] Manifest decoding is a trust boundary — the
	// JSON comes from the network — but the strongest TRUE theorem about this
	// payload is "it carries at least the fields I need", never "exactly the
	// fields I know". `lit upgrade` is run by the INSTALLED binary to discover
	// a newer release, so a manifest's consumer is by construction older than
	// its producer, and a field the producer added after this binary shipped
	// is the normal case rather than an attack. DisallowUnknownFields would
	// make that case a hard failure and turn every additive field into an
	// unrecoverable break of the upgrade path — unrecoverable because the
	// in-band remedy for a broken `lit upgrade` is `lit upgrade`. Nothing on
	// either side of the wire rejects an unknown field, and that is the intent
	// rather than a gap left behind: release-validate.yml's "Assert manifest
	// shape" step asserts presence and format of the fields this decoder
	// needs — `.version` a non-empty string, `.schema_support.min`/`.max`
	// numbers, every artifact's platform, url and sha256 matching their
	// patterns with the tag segment present in the url — and that the artifact
	// platform set is exactly the release contract. It never inspects the
	// manifest's key set, and no check anywhere else does.
	//
	// Shape is not what protects the consumer in any case: SelectArtifact
	// takes only the artifact whose Platform equals
	// runtime.GOOS+"/"+runtime.GOARCH exactly (target.go), and Install
	// verifies the downloaded bytes against that artifact's recorded SHA256
	// before extracting anything (installer.go). The trailing-data check below
	// still rejects multi-document and junk-suffix payloads.
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("release: decode %s: %w", url, err)
	}
	// dec.More() reports nested-element pendingness, not top-level trailing
	// content; a second Decode is the correct way to assert "manifest is the
	// only document on the stream." io.EOF means clean exit; anything else
	// (a second object, junk bytes) is a refusal.
	var trailing json.RawMessage
	err = dec.Decode(&trailing)
	switch {
	case errors.Is(err, io.EOF):
		// clean — nothing followed the manifest
	case err == nil:
		return nil, fmt.Errorf("release: decode %s: unexpected trailing JSON after manifest", url)
	default:
		return nil, fmt.Errorf("release: decode %s: unexpected trailing data after manifest: %w", url, err)
	}
	artifact, err := SelectArtifact(m, platform)
	if err != nil {
		return nil, err
	}
	return &Target{Manifest: m, Artifact: artifact}, nil
}
