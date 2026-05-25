package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestCollectArtifactsURLUsesTagNotVersion pins the producer-vs-publisher
// distinction: archive filenames use goreleaser's `.Version` (v-stripped),
// but `gh release create "<tag>"` publishes assets under the *git tag*
// segment (v-prefixed). collectArtifacts must use the tag for URL
// construction; using the version would produce 404 URLs in the manifest.
//
// [LAW:one-source-of-truth] each parameter encodes exactly one concept;
// this test makes the conflation unrepresentable in the contract.
func TestCollectArtifactsURLUsesTagNotVersion(t *testing.T) {
	dist := t.TempDir()
	const validHex = "0000000000000000000000000000000000000000000000000000000000000001"
	const archiveName = "lit_0.1.0_darwin_arm64.tar.gz"
	checksums := validHex + "  " + archiveName + "\n"
	if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(checksums), 0o644); err != nil {
		t.Fatalf("write checksums.txt: %v", err)
	}
	// Archive bytes are irrelevant — collectArtifacts only Stats them.
	if err := os.WriteFile(filepath.Join(dist, archiveName), []byte("archive"), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	const (
		baseURL = "https://github.com/owner/repo/releases/download"
		ver     = "0.1.0"
		tag     = "v0.1.0"
	)
	artifacts, err := collectArtifacts(dist, baseURL, ver, tag)
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(artifacts))
	}

	wantURL := baseURL + "/" + tag + "/" + archiveName
	if artifacts[0].URL != wantURL {
		t.Errorf("URL = %q, want %q", artifacts[0].URL, wantURL)
	}
	// Negative: the URL must NOT contain "/0.1.0/" as its path segment —
	// that would mean the version (not the tag) leaked into the URL.
	if strings.Contains(artifacts[0].URL, "/"+ver+"/") {
		t.Errorf("URL %q contains v-stripped version as path segment; must use tag", artifacts[0].URL)
	}
}

// TestValidateVerTag pins the v-prefix invariants at the CLI boundary:
// `-tag` MUST be v-prefixed (URL path segment), `-version` MUST NOT be
// (goreleaser .Version is v-stripped). Swapping them silently produces
// 404 URLs or never-match filenames; this table makes the swap impossible
// to express. [LAW:types-are-the-program] the type the boundary accepts
// is the exactly-correct shape, not "any non-empty string."
func TestValidateVerTag(t *testing.T) {
	cases := []struct {
		name      string
		ver, tag  string
		wantErr   bool
		wantField string // substring expected in the error message
	}{
		{"canonical pair", "0.1.0", "v0.1.0", false, ""},
		{"snapshot canonical", "0.0.1-snapshot+abc1234", "v0.0.1", false, ""},
		{"tag missing v prefix", "0.1.0", "0.1.0", true, "-tag"},
		{"version with v prefix", "v0.1.0", "v0.1.0", true, "-version"},
		{"swapped (both)", "v0.1.0", "0.1.0", true, "-tag"},
		// URL-path safety: tag is interpolated as a single URL segment;
		// path separators, traversal tokens, or whitespace would produce
		// invalid or surprising URLs.
		{"tag with slash", "0.1.0", "v0.1.0/x", true, "URL path segment"},
		{"tag with backslash", "0.1.0", "v0.1.0\\x", true, "URL path segment"},
		{"tag with traversal", "0.1.0", "v0..1.0", true, "URL path segment"},
		{"tag with space", "0.1.0", "v0.1.0 x", true, "URL path segment"},
		{"tag with tab", "0.1.0", "v0.1.0\tx", true, "URL path segment"},
		{"tag with newline", "0.1.0", "v0.1.0\nx", true, "URL path segment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVerTag(tc.ver, tc.tag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateVerTag(%q, %q) = nil; want error", tc.ver, tc.tag)
				}
				if !strings.Contains(err.Error(), tc.wantField) {
					t.Errorf("error %q should mention %q", err, tc.wantField)
				}
			} else if err != nil {
				t.Fatalf("validateVerTag(%q, %q) = %v; want nil", tc.ver, tc.tag, err)
			}
		})
	}
}

// TestCollectArtifactsRejectsUnsafeFilenames pins the boundary that
// rejects path-traversal and absolute paths inside checksums.txt before
// they reach filepath.Join (Stat) or URL construction. checksums.txt is
// goreleaser's output but is consumed across a parse boundary; the
// accept shape is exactly the bare basenames goreleaser produces, never
// a value containing path separators. [LAW:types-are-the-program] the
// filename's shape is constrained at the boundary so downstream code
// (Stat, URL building) can trust it.
func TestCollectArtifactsRejectsUnsafeFilenames(t *testing.T) {
	const validHex = "0000000000000000000000000000000000000000000000000000000000000001"
	cases := []string{
		"../lit_0.1.0_linux_amd64.tar.gz",
		"../../etc/passwd",
		"/etc/passwd",
		"sub/lit_0.1.0_linux_amd64.tar.gz",
		"lit\\0.1.0_linux_amd64.tar.gz", // backslash separator
		".",
		"..",
	}
	for _, badName := range cases {
		t.Run(badName, func(t *testing.T) {
			dist := t.TempDir()
			checksums := validHex + "  " + badName + "\n"
			if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(checksums), 0o644); err != nil {
				t.Fatalf("write checksums.txt: %v", err)
			}
			_, err := collectArtifacts(dist, "https://example/dl", "0.1.0", "v0.1.0")
			if err == nil {
				t.Fatalf("collectArtifacts accepted unsafe filename %q; want rejection", badName)
			}
			if !strings.Contains(err.Error(), "unsafe path shape") {
				t.Errorf("error %q should mention 'unsafe path shape' for %q", err, badName)
			}
		})
	}
}

// TestCollectArtifactsRejectsMissingArchive pins the contract that
// checksums.txt entries are claims that get verified against dist/ —
// if the referenced archive is missing (e.g., aborted goreleaser run
// wrote checksums.txt but failed to produce one of the archives),
// mkmanifest must fail loudly instead of emitting a 404 URL.
func TestCollectArtifactsRejectsMissingArchive(t *testing.T) {
	dist := t.TempDir()
	const validHex = "0000000000000000000000000000000000000000000000000000000000000001"
	const archiveName = "lit_0.1.0_linux_amd64.tar.gz"
	// checksums.txt references the archive, but we deliberately do NOT
	// create the file on disk.
	checksums := validHex + "  " + archiveName + "\n"
	if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(checksums), 0o644); err != nil {
		t.Fatalf("write checksums.txt: %v", err)
	}

	_, err := collectArtifacts(dist, "https://example/dl", "0.1.0", "v0.1.0")
	if err == nil {
		t.Fatalf("collectArtifacts succeeded with missing archive; want error")
	}
	if !strings.Contains(err.Error(), archiveName) {
		t.Errorf("error %q should name the missing archive %q", err, archiveName)
	}
}
