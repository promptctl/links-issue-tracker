// Package release defines the per-release artifact manifest. A manifest is
// produced by tools/mkmanifest from the workflow that built a release and
// published as a release asset alongside the per-platform archives.
//
// This package owns the schema only. It is consumed by:
//   - tools/mkmanifest (writes the manifest after goreleaser builds artifacts).
//   - `lit downgrade` (downgrade epic .4, not yet built): fetches the target
//     version's manifest from the GitHub Release, resolves the current
//     platform's Artifact, verifies SHA256.
//   - The refusal-message upgrade (downgrade epic .5, not yet built):
//     consults a manifest to name a concrete prior version.
//
// NOTE: an earlier draft of this comment claimed the manifest is "embedded in
// each binary" so `lit version --json` could expose Artifact lists locally.
// That embedding is not implemented in this PR; `lit version` only reports
// version.Info. Embedding can be added later via go:embed if downstream
// tickets need it.
//
// [LAW:one-source-of-truth] One schema definition. The bytes mkmanifest writes
// and the bytes a downgrade client reads share this Go type — there is no
// parallel JSON schema description that could drift from this struct.
package release

import "github.com/promptctl/links-issue-tracker/internal/version"

// Manifest is the per-release index. It embeds version.Info so a release's
// identity (Version, Commit, Date, Schema) is recorded in exactly the same
// shape a running binary reports. Artifacts and Signature are release-only
// metadata.
//
// [LAW:types-are-the-program] Embedding version.Info means any change to that
// shape propagates automatically; the release format does not maintain its
// own copy of the binary-identity fields. IsDev will always serialize false
// for published manifests (a release is by definition not a dev build); the
// field is left in place for symmetry rather than diverging the schemas.
type Manifest struct {
	version.Info
	Artifacts []Artifact `json:"artifacts"`
	Signature *Signature `json:"signature,omitempty"`
}

// Artifact is one per-platform binary published with a release.
//
// [LAW:types-are-the-program] Platform is the discriminator a downgrade
// client uses to pick the right artifact. Producer (goreleaser) writes
// "<GOOS>/<GOARCH>" matching runtime.GOOS+"/"+runtime.GOARCH so the consumer
// match is exact; no fuzzy string contains or fallback chain.
type Artifact struct {
	Platform string `json:"platform"` // e.g. "darwin/arm64", "linux/amd64"
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
}

// Signature is reserved for a future signing scheme (cosign / minisign / GPG).
// Designed as optional so adding signatures later does not require bumping a
// manifest format version — clients that don't yet verify signatures simply
// ignore the field; clients that do verify check it when present and refuse
// when absent (their policy decision).
type Signature struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}
