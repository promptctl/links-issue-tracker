package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// tagAcceptPattern enforces a URL-safe single-path-segment shape: leading
// "v" followed by one or more characters from a fixed whitelist
// ([A-Za-z0-9._+-]). It does NOT enforce strict semver — `vfoo` and
// `v1.2.bar` pass the check; the resolver's job is URL safety, and any
// shape-not-published-by-the-pipeline 404s when fetched. mkmanifest's
// producer-side validator is the semver authority; this is the consumer
// mirror that guarantees what gets interpolated into the URL can't be a
// URL metacharacter (?, #, %, etc.), whitespace, or a path-traversal
// sequence.
//
// [LAW:types-are-the-program] Whitelist > denylist for boundary types:
// "what survives URL interpolation cleanly" is finite; "what doesn't" is
// infinite and can never be enumerated correctly.
var tagAcceptPattern = regexp.MustCompile(`^v[A-Za-z0-9._+-]+$`)

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
	Resolve(ctx context.Context, tag, platform string) (*Target, error)
}

// LatestResolver names the latest published release tag, for a caller that
// was given no tag to target. The result feeds Resolver.Resolve unchanged —
// latest-lookup answers only "which tag," never "which artifact."
//
// [LAW:one-source-of-truth] "Latest" is defined by the release feed
// (DefaultLatestReleaseURL), the same authority scripts/install.sh's
// --latest-release consults — not by any local heuristic over tags.
type LatestResolver interface {
	LatestTag(ctx context.Context) (string, error)
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

// acceptTag refuses any tag that cannot be interpolated into a URL path
// segment safely — the one accept shape for tags, whichever door they arrive
// through (a user's --to via Resolve, or the release feed via LatestTag).
//
// [LAW:single-enforcer] The resolver is the boundary that owns URL safety.
// The CLI happens to validate before calling, but refusing here means no
// in-process caller can smuggle path traversal, fragment injection, or
// whitespace through the segment — and a malformed feed response is refused
// by the same rule as a malformed flag.
// [LAW:types-are-the-program] mkmanifest's -tag flag enforces the same
// accept shape; this is the consumer mirror.
func acceptTag(tag string) error {
	if !strings.HasPrefix(tag, "v") {
		return fmt.Errorf("release: tag must be v-prefixed (got %q)", tag)
	}
	if !tagAcceptPattern.MatchString(tag) {
		return fmt.Errorf("release: tag %q must match %s (v-prefix + alphanumerics, dots, dashes, underscores, plus)", tag, tagAcceptPattern)
	}
	if strings.Contains(tag, "..") {
		return fmt.Errorf("release: tag %q contains path-traversal sequence", tag)
	}
	return nil
}

// httpClient returns the configured client or the bounded default.
func (r *HTTPResolver) httpClient() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	// Bounded default — http.DefaultClient is shared and has no Timeout.
	return &http.Client{Timeout: defaultResolverTimeout}
}

// LatestTag fetches the release feed and returns the latest release's tag,
// refused through the same accept shape as an explicitly named tag.
func (r *HTTPResolver) LatestTag(ctx context.Context) (string, error) {
	url := r.LatestURL
	if url == "" {
		url = DefaultLatestReleaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("release: fetch latest release from %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("release: fetch latest release from %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// The feed is a third-party API that grows fields freely, so unlike the
	// manifest decode this one must tolerate unknown fields; tag_name is the
	// only field lit consumes (the same field install.sh's jq reads).
	var feed struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&feed); err != nil {
		return "", fmt.Errorf("release: decode latest release from %s: %w", url, err)
	}
	// [LAW:no-silent-failure] A 200 with no tag_name is a feed-shape change,
	// not "no releases" — refuse loudly rather than hand back an empty tag
	// that would fail later, far from its cause.
	if feed.TagName == "" {
		return "", fmt.Errorf("release: latest release feed at %s returned no tag_name", url)
	}
	if err := acceptTag(feed.TagName); err != nil {
		return "", err
	}
	return feed.TagName, nil
}

// Resolve fetches and parses the manifest, then selects the platform artifact.
func (r *HTTPResolver) Resolve(ctx context.Context, tag, platform string) (*Target, error) {
	if err := acceptTag(tag); err != nil {
		return nil, err
	}
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
	// JSON comes from the network. DisallowUnknownFields rejects schema drift
	// (a field added in a future producer without a consumer-side migration);
	// the trailing-data check rejects multi-document or junk-suffix payloads.
	// Both refuse silently-different-shape inputs by construction.
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.DisallowUnknownFields()
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
