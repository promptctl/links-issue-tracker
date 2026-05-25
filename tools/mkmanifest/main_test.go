package main

import "testing"

// TestPlatformFromFilenameAcceptReject is the accept/reject table for
// platformFromFilename. The producer (goreleaser, configured by
// .goreleaser.yml) writes archives as `lit_<version>_<goos>_<goarch>.<ext>`
// where <version> is goreleaser's `.Version` template — the tag with the
// leading "v" STRIPPED. This table mirrors that producer shape exactly:
// any other shape returns ok=false so the caller skips non-archive entries
// (source tarballs, the SHA file, the manifest itself) silently.
//
// [LAW:types-are-the-program] The producer is the source of truth; the parser
// rejects every name that does not match the produced shape. If goreleaser's
// archive template changes, .goreleaser.yml and this table change together.
func TestPlatformFromFilenameAcceptReject(t *testing.T) {
	cases := []struct {
		name string
		ver  string
		want string
		ok   bool
	}{
		// Accept: produced shapes. Note the version segment has NO leading
		// "v" — goreleaser's .Version strips it from the tag.
		{"lit_0.1.0_darwin_arm64.tar.gz", "0.1.0", "darwin/arm64", true},
		{"lit_0.1.0_linux_amd64.tar.gz", "0.1.0", "linux/amd64", true},
		{"lit_0.1.0_linux_arm64.tar.gz", "0.1.0", "linux/arm64", true},
		{"lit_0.1.0_windows_amd64.zip", "0.1.0", "windows/amd64", true},
		// Snapshot version shape (from goreleaser's snapshot.version_template
		// in .goreleaser.yml: "{{ incpatch .Version }}-snapshot+{{ .ShortCommit }}").
		{"lit_0.0.1-snapshot+abc1234_linux_amd64.tar.gz", "0.0.1-snapshot+abc1234", "linux/amd64", true},

		// Reject: wrong version (someone built two tags into one dist somehow).
		{"lit_0.2.0_darwin_arm64.tar.gz", "0.1.0", "", false},
		// Reject: version segment HAS a leading v but caller passed v-less.
		// This is the producer-mismatch shape that prompted the explicit
		// note above; if a future workflow drift starts producing v-prefixed
		// archives, this rejection ensures we notice instead of silently
		// double-counting platforms.
		{"lit_v0.1.0_darwin_arm64.tar.gz", "0.1.0", "", false},
		// Reject: non-archive entries goreleaser might emit (source tarball,
		// SHA file, the manifest itself).
		{"lit_0.1.0_source.tar.gz", "0.1.0", "", false},
		{"checksums.txt", "0.1.0", "", false},
		{"release-manifest.json", "0.1.0", "", false},
		// Reject: legacy dash-style naming we explicitly do not accept (must
		// fail loudly so a producer change is observed, not silently ignored).
		{"lit-0.1.0-darwin-arm64.tar.gz", "0.1.0", "", false},
		// Reject: too few parts.
		{"lit_0.1.0.tar.gz", "0.1.0", "", false},
		// Reject: empty components.
		{"lit_0.1.0__arm64.tar.gz", "0.1.0", "", false},
		// Reject: unrecognized extension — must require .tar.gz or .zip.
		{"lit_0.1.0_linux_amd64.deb", "0.1.0", "", false},
		{"lit_0.1.0_linux_amd64.rpm", "0.1.0", "", false},
		{"lit_0.1.0_linux_amd64", "0.1.0", "", false},     // no extension at all
		{"lit_0.1.0_linux_amd64.txt", "0.1.0", "", false}, // wrong extension
		// Reject: non-lit project prefix — guards against unrelated archives
		// landing in dist/ and being mis-classified as our platforms.
		{"otherproj_0.1.0_linux_amd64.tar.gz", "0.1.0", "", false},
		{"_0.1.0_linux_amd64.tar.gz", "0.1.0", "", false}, // empty prefix
	}
	for _, tc := range cases {
		got, ok := platformFromFilename(tc.name, tc.ver)
		if ok != tc.ok {
			t.Errorf("platformFromFilename(%q, %q) ok = %v, want %v", tc.name, tc.ver, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("platformFromFilename(%q, %q) = %q, want %q", tc.name, tc.ver, got, tc.want)
		}
	}
}
