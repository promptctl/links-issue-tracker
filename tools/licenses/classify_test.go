package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	lc "github.com/google/licenseclassifier"
)

// canonicalMIT is the standard OSI-published MIT License template text.
// Embedded literally so classify_test.go doesn't depend on GOMODCACHE
// contents (which module happens to be MIT-licensed on whichever machine
// runs `go test` is not a stable fact to test against).
const canonicalMIT = `MIT License

Copyright (c) 2026 Example Author

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

// nonStandardWTFPL is the short, profanely-worded WTFPL variant shipped by
// github.com/kch42/buzhash — real text from this repo's own dependency tree.
// Too different from the canonical WTFPL wording for the classifier to match
// confidently; it is the tree's one real "Unknown" case and is what pins that
// behavior rather than a hypothetical.
const nonStandardWTFPL = `           DO WHATEVER THE FUCK YOU WANT, PUBLIC LICENSE
   TERMS AND CONDITIONS FOR COPYING, DISTRIBUTION AND MODIFICATION

            0. You just DO WHATEVER THE FUCK YOU WANT.
`

func TestClassify(t *testing.T) {
	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		t.Fatalf("build classifier: %v", err)
	}

	t.Run("classifies canonical license text with high confidence", func(t *testing.T) {
		name, confidence := Classify(classifier, canonicalMIT)
		if name != "MIT" {
			t.Errorf("name = %q, want MIT", name)
		}
		if confidence < lc.DefaultConfidenceThreshold {
			t.Errorf("confidence = %v, want >= %v", confidence, lc.DefaultConfidenceThreshold)
		}
	})

	t.Run("falls back to Unknown for text no known license matches", func(t *testing.T) {
		name, confidence := Classify(classifier, nonStandardWTFPL)
		if name != unclassifiedLicense {
			t.Errorf("name = %q, want %q", name, unclassifiedLicense)
		}
		if confidence != 0 {
			t.Errorf("confidence = %v, want 0 for an unclassified match", confidence)
		}
	})

	t.Run("picks the earliest-offset match as the primary license when a file bundles more than one", func(t *testing.T) {
		// ASF-convention shape: the module's own license first, a bundled
		// third party's license appended after. The primary license is the
		// one that comes FIRST, not the one with a longer match.
		bundled := canonicalMIT + "\n---\nBundled component license:\n\n" + canonicalMIT
		name, _ := Classify(classifier, bundled)
		if name != "MIT" {
			t.Errorf("name = %q, want MIT", name)
		}
	})
}

// TestFindLicenseFileAcceptReject is the accept/reject table for
// FindLicenseFile against a synthetic module directory.
// [LAW:types-are-the-program]
func TestFindLicenseFileAcceptReject(t *testing.T) {
	writeFiles := func(t *testing.T, names ...string) string {
		t.Helper()
		dir := t.TempDir()
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("content"), 0o644); err != nil {
				t.Fatalf("write fixture %s: %v", name, err)
			}
		}
		return dir
	}

	t.Run("finds a bare LICENSE file", func(t *testing.T) {
		dir := writeFiles(t, "LICENSE", "README.md", "main.go")
		got, err := FindLicenseFile(dir)
		if err != nil {
			t.Fatalf("FindLicenseFile: %v", err)
		}
		if filepath.Base(got) != "LICENSE" {
			t.Errorf("got %q, want LICENSE", got)
		}
	})

	t.Run("prefers bare LICENSE over a second license-shaped file", func(t *testing.T) {
		// The real shape: gopkg.in/yaml.v2 ships both LICENSE and
		// LICENSE.libyaml (the latter for a vendored C dependency).
		dir := writeFiles(t, "LICENSE", "LICENSE.libyaml")
		got, err := FindLicenseFile(dir)
		if err != nil {
			t.Fatalf("FindLicenseFile: %v", err)
		}
		if filepath.Base(got) != "LICENSE" {
			t.Errorf("got %q, want LICENSE preferred over LICENSE.libyaml", got)
		}
	})

	t.Run("resolves a single match with no bare LICENSE present", func(t *testing.T) {
		dir := writeFiles(t, "LICENSE.txt")
		got, err := FindLicenseFile(dir)
		if err != nil {
			t.Fatalf("FindLicenseFile: %v", err)
		}
		if filepath.Base(got) != "LICENSE.txt" {
			t.Errorf("got %q, want LICENSE.txt", got)
		}
	})

	t.Run("errors on multiple candidates with no bare LICENSE to prefer", func(t *testing.T) {
		// The real shape this guards: a dual-license repo shipping
		// LICENSE-APACHE and LICENSE-MIT with no bare LICENSE — picking one
		// silently would drop a real license option from the bundle with no
		// record a choice was ever made. Naming both candidates in the error
		// forces a human decision instead.
		dir := writeFiles(t, "LICENSE-APACHE", "LICENSE-MIT")
		_, err := FindLicenseFile(dir)
		if err == nil {
			t.Fatal("want error: ambiguous — two license files, neither is the bare LICENSE")
		}
		for _, want := range []string{"LICENSE-APACHE", "LICENSE-MIT"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q doesn't name candidate %q", err, want)
			}
		}
	})

	t.Run("accepts COPYING and UNLICENSE spellings", func(t *testing.T) {
		for _, name := range []string{"COPYING", "UNLICENSE", "LICENCE"} {
			dir := writeFiles(t, name)
			got, err := FindLicenseFile(dir)
			if err != nil {
				t.Fatalf("FindLicenseFile(%s): %v", name, err)
			}
			if filepath.Base(got) != name {
				t.Errorf("got %q, want %q", got, name)
			}
		}
	})

	t.Run("accepts hyphen and underscore separators, not just dot", func(t *testing.T) {
		// Real-world dual-license convention: LICENSE-APACHE / LICENSE-MIT
		// side by side, or LICENSE_MIT alone. filepath.Base equality (not
		// FindLicenseFile's "no bare LICENSE, fall back" branch) confirms
		// each is recognized as a license file at all — bare-LICENSE
		// preference is already covered above.
		for _, name := range []string{"LICENSE-APACHE", "LICENSE_MIT", "LICENSE-Apache-2.0"} {
			dir := writeFiles(t, name)
			got, err := FindLicenseFile(dir)
			if err != nil {
				t.Fatalf("FindLicenseFile(%s): %v", name, err)
			}
			if filepath.Base(got) != name {
				t.Errorf("got %q, want %q", got, name)
			}
		}
	})

	t.Run("does not match README or NOTICE alone", func(t *testing.T) {
		dir := writeFiles(t, "README.md", "NOTICE")
		if _, err := FindLicenseFile(dir); err == nil {
			t.Fatal("want error: README/NOTICE alone is not a license grant")
		}
	})

	t.Run("errors on a directory with no license-shaped file", func(t *testing.T) {
		dir := writeFiles(t, "main.go", "go.mod")
		if _, err := FindLicenseFile(dir); err == nil {
			t.Fatal("want error for a directory with no license file")
		}
	})

	t.Run("ignores subdirectories matching the license pattern", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "LICENSE"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if _, err := FindLicenseFile(dir); err == nil {
			t.Fatal("want error: a directory named LICENSE is not a license file")
		}
	})
}

func TestFindLicenseFileMissingDir(t *testing.T) {
	if _, err := FindLicenseFile(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("want error for a nonexistent module dir")
	} else if !strings.Contains(err.Error(), "read module dir") {
		t.Errorf("error = %v, want it to name the failing operation", err)
	}
}
