// Package release defines the per-release artifact manifest. A manifest is
// produced by tools/mkmanifest from the workflow that built a release and
// published as a release asset alongside the per-platform archives.
//
// This package owns the schema only. It is consumed by:
//   - tools/mkmanifest (writes the manifest after goreleaser builds artifacts).
//   - `lit upgrade` and `lit downgrade`: fetch the target release's manifest
//     and read its schema support — upgrade to refuse a release that cannot
//     operate the workspace, downgrade for the schema version to reverse to —
//     then install the current platform's Artifact after verifying its SHA256.
//
// [LAW:one-source-of-truth] One schema definition. The bytes mkmanifest writes
// and the bytes a downgrade client reads share this Go type — there is no
// parallel JSON schema description that could drift from this struct.
package release

import "github.com/promptctl/links-issue-tracker/internal/version"

// Manifest is the per-release index. It embeds version.Info so a release's
// identity is recorded in exactly the same shape a running binary reports.
// Artifacts and Signature are release-only metadata.
//
// [LAW:types-are-the-program] Embedding version.Info means any change to that
// shape propagates automatically; the release format does not maintain its
// own copy of the binary-identity fields. That propagation is the reason the
// wire shape has to be guarded: a field added to version.Info lands in every
// published manifest, and manifests are read by binaries older than the
// producer. IsDev always serializes false for a published manifest (a release
// is by definition not a dev build) and stays on the wire because it has
// always been there. FromSource is tagged `json:"-"` at its declaration and
// never appears: the question is meaningless for a release, and every lit
// already installed would fail to decode a manifest carrying it. Adding a
// field to Info is therefore a wire-format change — see the note on
// FromSource for what it costs, HTTPResolver.Resolve for what absorbs
// it going forward, and TestManifestWireKeys for the release check that
// must change with it.
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
